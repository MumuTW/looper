package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

// maxJSONMutationBodyBytes bounds every JSON mutation request body decoded
// through the shared boundary. Routes with their own larger, already-bounded
// decoders (config PATCH, bootstrap, external webhook passthrough) keep them.
const maxJSONMutationBodyBytes = 1 << 20

// decodeJSONMutationBody is the single request boundary for JSON mutations:
// bounded read, strict unknown-field validation, and exactly one JSON value.
// The frozen public request schema is the authority for mutations — a
// misspelled field must fail loudly, not silently proceed with defaults.
//
// requireBody controls empty-body semantics: create-style routes require a
// JSON object, while retry/respond-style routes treat an absent or empty body
// as the zero request. The returned *apiError is ready to surface.
func decodeJSONMutationBody(r *http.Request, dst any, requireBody bool) *apiError {
	if r.Body == nil {
		if requireBody {
			return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "Request body is required"}
		}
		return nil
	}
	defer r.Body.Close()
	limited := http.MaxBytesReader(nil, r.Body, maxJSONMutationBodyBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return &apiError{code: pkgapi.ErrorCodeRequestTooLarge, status: http.StatusRequestEntityTooLarge, message: fmt.Sprintf("Request body exceeds %d bytes", maxJSONMutationBodyBytes)}
		}
		return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Request body could not be read: %v", err)}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if requireBody {
			return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "Request body is required"}
		}
		return nil
	}
	return decodeStrictJSONValue(raw, dst)
}

// decodeStrictJSONValue decodes exactly one JSON value from raw into dst with
// unknown fields disallowed. It is the bytes-level half of the shared
// boundary, for call sites that must peek at a body they later restore.
func decodeStrictJSONValue(raw []byte, dst any) *apiError {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		message := "Request body must be valid JSON"
		if strings.Contains(err.Error(), "unknown field") {
			message = "Request body has an unknown field"
		}
		return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s: %v", message, err)}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "Request body must contain exactly one JSON value"}
	}
	// raw is valid single-value JSON at this point, so a failure here is a
	// genuine duplicate. Comparison is case-folded because encoding/json
	// matches struct fields case-insensitively even with
	// DisallowUnknownFields: {"force":true,"Force":false} would silently let
	// the last spelling win.
	if dup := firstDuplicateJSONName(raw); dup != "" {
		return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Request body has a duplicate field: %q", dup)}
	}
	return nil
}

// firstDuplicateJSONName walks every object in raw and returns the first
// member name that repeats within one object under case-folding, or "".
// raw must already be known-valid JSON.
func firstDuplicateJSONName(raw []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	type objectFrame struct {
		seen      map[string]string
		expectKey bool
		isObject  bool
	}
	var stack []*objectFrame
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		switch t := token.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &objectFrame{seen: map[string]string{}, expectKey: true, isObject: true})
			case '[':
				stack = append(stack, &objectFrame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].isObject {
					stack[len(stack)-1].expectKey = true
				}
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].isObject && stack[len(stack)-1].expectKey {
				frame := stack[len(stack)-1]
				folded := strings.ToLower(t)
				if _, dup := frame.seen[folded]; dup {
					return t
				}
				frame.seen[folded] = t
				frame.expectKey = false
				continue
			}
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				stack[len(stack)-1].expectKey = true
			}
		default:
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				stack[len(stack)-1].expectKey = true
			}
		}
	}
}
