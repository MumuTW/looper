package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
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
	// genuine duplicate. Comparison uses strings.EqualFold to match the
	// Unicode simple-fold equivalence that encoding/json applies when
	// matching struct fields: {"force":true,"Force":false} and even
	// {"status":"x","ſtatus":"y"} (ſ folds to s) are both caught.
	if dup := firstDuplicateJSONName(raw); dup != "" {
		return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Request body has a duplicate field: %q", dup)}
	}
	if aerr := validateCanonicalFieldCasing(raw, reflect.TypeOf(dst)); aerr != nil {
		return aerr
	}
	return nil
}

// firstDuplicateJSONName walks every object in raw and returns the first
// member name that repeats within one object under the same Unicode simple
// folding encoding/json uses for field matching (strings.EqualFold), or "".
// raw must already be known-valid JSON.
func firstDuplicateJSONName(raw []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	type objectFrame struct {
		seen      []string
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
				stack = append(stack, &objectFrame{expectKey: true, isObject: true})
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
				for _, seen := range frame.seen {
					if strings.EqualFold(seen, t) {
						return t
					}
				}
				frame.seen = append(frame.seen, t)
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

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// validateCanonicalFieldCasing rejects member names that reach a struct field
// only through encoding/json's case-insensitive matching: a lone {"Force":true}
// passes DisallowUnknownFields yet sets the force field, so the frozen schema's
// exact spelling is enforced here. Plain maps accept arbitrary keys and types
// with custom unmarshalers own their wire format, so both stop the walk.
func validateCanonicalFieldCasing(raw json.RawMessage, t reflect.Type) *apiError {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	if t.Kind() != reflect.Struct && t.Kind() != reflect.Slice && t.Kind() != reflect.Array && t.Kind() != reflect.Map {
		return nil
	}
	if reflect.PointerTo(t).Implements(jsonUnmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return nil
		}
		for _, elem := range elems {
			if aerr := validateCanonicalFieldCasing(elem, t.Elem()); aerr != nil {
				return aerr
			}
		}
		return nil
	case reflect.Map:
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil
		}
		for _, value := range members {
			if aerr := validateCanonicalFieldCasing(value, t.Elem()); aerr != nil {
				return aerr
			}
		}
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil
	}
	canonical := map[string]reflect.Type{}
	collectCanonicalJSONFields(t, canonical)
	for name, value := range members {
		fieldType, exact := canonical[name]
		if exact {
			if aerr := validateCanonicalFieldCasing(value, fieldType); aerr != nil {
				return aerr
			}
			continue
		}
		for tag := range canonical {
			if strings.EqualFold(tag, name) {
				return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Request body field %q must use its canonical spelling %q", name, tag)}
			}
		}
	}
	return nil
}

// collectCanonicalJSONFields records the exact wire names of t's fields,
// flattening embedded structs the way encoding/json does.
func collectCanonicalJSONFields(t reflect.Type, out map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" && tag == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				collectCanonicalJSONFields(embedded, out)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}
		out[name] = field.Type
	}
}
