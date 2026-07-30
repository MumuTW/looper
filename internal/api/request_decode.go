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
	"unicode"

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
	// encoding/json accepts case-insensitive struct-field matches and keeps the
	// last duplicate. Walk only schema-owned objects to reject those ambiguous
	// forms; maps and custom unmarshaler fields retain their own key semantics.
	if aerr := validateStrictJSONFields(raw, reflect.TypeOf(dst)); aerr != nil {
		return aerr
	}
	return nil
}

var jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

// validateStrictJSONFields rejects names that either repeat or reach a struct
// field only through encoding/json's case-insensitive matching. Its walk is
// type-directed: free-form maps and json.RawMessage preserve their consumer's
// last-key-wins semantics, while nested struct values remain strict.
func validateStrictJSONFields(raw json.RawMessage, t reflect.Type) *apiError {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	return walkStrictJSONValue(decoder, t)
}

func walkStrictJSONValue(decoder *json.Decoder, t reflect.Type) *apiError {
	token, err := decoder.Token()
	if err != nil {
		return nil // raw was decoded successfully before this validation pass.
	}
	return walkStrictJSONToken(decoder, token, t)
}

func walkStrictJSONToken(decoder *json.Decoder, token json.Token, t reflect.Type) *apiError {
	if t == nil {
		skipJSONToken(decoder, token)
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if token == nil || implementsJSONUnmarshaler(t) {
		skipJSONToken(decoder, token)
		return nil
	}
	delim, isContainer := token.(json.Delim)
	if !isContainer {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if delim != '{' {
			skipJSONToken(decoder, token)
			return nil
		}
		fields := map[string]reflect.Type{}
		collectCanonicalJSONFields(t, fields)
		seen := map[string]string{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil
			}
			name, _ := keyToken.(string)
			if previous, duplicate := seen[jsonFieldFoldKey(name)]; duplicate {
				return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Request body has a duplicate field: %q (also %q)", name, previous)}
			}
			seen[jsonFieldFoldKey(name)] = name
			fieldType, exact := fields[name]
			if !exact {
				if canonical, matches := canonicalJSONFieldName(fields, name); matches {
					return &apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Request body field %q must use its canonical spelling %q", name, canonical)}
				}
				skipJSONValue(decoder)
				continue
			}
			if aerr := walkStrictJSONValue(decoder, fieldType); aerr != nil {
				return aerr
			}
		}
		_, _ = decoder.Token() // closing '}', guaranteed by the prior decode.
		return nil
	case reflect.Map:
		if delim != '{' {
			skipJSONToken(decoder, token)
			return nil
		}
		for decoder.More() {
			_, _ = decoder.Token() // map keys are intentionally free-form.
			if aerr := walkStrictJSONValue(decoder, t.Elem()); aerr != nil {
				return aerr
			}
		}
		_, _ = decoder.Token() // closing '}'
		return nil
	case reflect.Slice, reflect.Array:
		if delim != '[' {
			skipJSONToken(decoder, token)
			return nil
		}
		for decoder.More() {
			if aerr := walkStrictJSONValue(decoder, t.Elem()); aerr != nil {
				return aerr
			}
		}
		_, _ = decoder.Token() // closing ']'
		return nil
	}
	skipJSONToken(decoder, token)
	return nil
}

func implementsJSONUnmarshaler(t reflect.Type) bool {
	return t.Implements(jsonUnmarshalerType) || reflect.PointerTo(t).Implements(jsonUnmarshalerType)
}

func canonicalJSONFieldName(fields map[string]reflect.Type, name string) (string, bool) {
	for canonical := range fields {
		if strings.EqualFold(canonical, name) {
			return canonical, true
		}
	}
	return "", false
}

// jsonFieldFoldKey gives every rune the smallest member of its Unicode simple
// fold cycle. It is a stable, O(length) map key for strings.EqualFold classes.
func jsonFieldFoldKey(name string) string {
	var key strings.Builder
	key.Grow(len(name))
	for _, r := range name {
		folded := r
		for candidate := unicode.SimpleFold(r); candidate != r; candidate = unicode.SimpleFold(candidate) {
			if candidate < folded {
				folded = candidate
			}
		}
		key.WriteRune(folded)
	}
	return key.String()
}

func skipJSONValue(decoder *json.Decoder) {
	token, err := decoder.Token()
	if err == nil {
		skipJSONToken(decoder, token)
	}
}

func skipJSONToken(decoder *json.Decoder, token json.Token) {
	delim, ok := token.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return
	}
	for decoder.More() {
		if delim == '{' {
			_, _ = decoder.Token()
		}
		skipJSONValue(decoder)
	}
	_, _ = decoder.Token()
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
