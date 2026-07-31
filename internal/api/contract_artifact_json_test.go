package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The compat artifacts are regenerated from live handler responses, so the
// encoder here has to preserve the key order the handler actually emits:
// marshaling through map[string]any would sort every object alphabetically and
// turn each regeneration into an unreviewable reordering of the whole file.
type jsonObject struct {
	keys   []string
	values map[string]any
}

func newJSONObject() *jsonObject {
	return &jsonObject{values: make(map[string]any)}
}

func (o *jsonObject) set(key string, value any) *jsonObject {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}

func (o *jsonObject) get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

func decodeOrderedJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	value, err := decodeOrderedValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON data")
	}

	return value, nil
}

func decodeOrderedValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}

	return decodeOrderedFromToken(decoder, token)
}

func decodeOrderedFromToken(decoder *json.Decoder, token json.Token) (any, error) {
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}

	switch delim {
	case '{':
		object := newJSONObject()
		for {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			if closing, ok := keyToken.(json.Delim); ok && closing == '}' {
				return object, nil
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is %T, want string", keyToken)
			}
			value, err := decodeOrderedValue(decoder)
			if err != nil {
				return nil, err
			}
			object.set(key, value)
		}
	case '[':
		items := []any{}
		for {
			itemToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			if closing, ok := itemToken.(json.Delim); ok && closing == ']' {
				return items, nil
			}
			item, err := decodeOrderedFromToken(decoder, itemToken)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
	}

	return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
}

// encodeOrderedJSON matches json.MarshalIndent with a two-space indent, minus
// the HTML escaping that would rewrite the artifacts' <placeholder> tokens.
func encodeOrderedJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeOrderedJSON(&buf, value, ""); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')

	return buf.Bytes(), nil
}

func writeOrderedJSON(buf *bytes.Buffer, value any, indent string) error {
	inner := indent + "  "

	switch typed := value.(type) {
	case *jsonObject:
		if len(typed.keys) == 0 {
			buf.WriteString("{}")
			return nil
		}
		buf.WriteString("{\n")
		for i, key := range typed.keys {
			if i > 0 {
				buf.WriteString(",\n")
			}
			buf.WriteString(inner)
			if err := writeJSONScalar(buf, key); err != nil {
				return err
			}
			buf.WriteString(": ")
			if err := writeOrderedJSON(buf, typed.values[key], inner); err != nil {
				return err
			}
		}
		buf.WriteString("\n" + indent + "}")
		return nil
	case []any:
		if len(typed) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteString("[\n")
		for i, item := range typed {
			if i > 0 {
				buf.WriteString(",\n")
			}
			buf.WriteString(inner)
			if err := writeOrderedJSON(buf, item, inner); err != nil {
				return err
			}
		}
		buf.WriteString("\n" + indent + "]")
		return nil
	default:
		return writeJSONScalar(buf, value)
	}
}

func writeJSONScalar(buf *bytes.Buffer, value any) error {
	// Any composite reaching here would be marshaled on a single line and
	// silently reformat the artifact; every container must be *jsonObject or
	// []any so ordering and indentation stay under this encoder's control.
	if value != nil {
		if kind := reflect.ValueOf(value).Kind(); kind == reflect.Map || kind == reflect.Slice || kind == reflect.Struct {
			return fmt.Errorf("unordered composite %T in artifact tree", value)
		}
	}

	encoder := json.NewEncoder(buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1) // Encode appends a newline.

	return nil
}

// setJSONPointer masks a runtime-generated leaf with its placeholder token. A
// missing path is fatal: it means the handler stopped emitting a field the
// artifact still claims is part of the boundary.
func setJSONPointer(t *testing.T, root any, pointer string, replacement any) {
	t.Helper()

	segments := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	current := root
	for i, segment := range segments {
		last := i == len(segments)-1
		switch container := current.(type) {
		case *jsonObject:
			existing, ok := container.get(segment)
			if !ok {
				t.Fatalf("placeholder path %q: key %q not present in captured response", pointer, segment)
			}
			if last {
				container.set(segment, replacement)
				return
			}
			current = existing
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(container) {
				t.Fatalf("placeholder path %q: index %q out of range in captured response", pointer, segment)
			}
			if last {
				container[index] = replacement
				return
			}
			current = container[index]
		default:
			t.Fatalf("placeholder path %q: %q is not addressable in captured response", pointer, segment)
		}
	}
}

func decodeContractJSON(t *testing.T, label string, raw []byte) any {
	t.Helper()

	value, err := decodeOrderedJSON(raw)
	if err != nil {
		t.Fatalf("decodeOrderedJSON(%s) error = %v\nraw=%s", label, err, string(raw))
	}

	return value
}

// declaredContractJSON parses metadata that is authored here rather than
// captured: provenance headers, prose notes, and shared-behavior descriptions.
func declaredContractJSON(t *testing.T, label, raw string) any {
	t.Helper()

	return decodeContractJSON(t, label, []byte(raw))
}

func declaredContractObject(t *testing.T, label, raw string) *jsonObject {
	t.Helper()

	object, ok := declaredContractJSON(t, label, raw).(*jsonObject)
	if !ok {
		t.Fatalf("declared JSON %s is not an object", label)
	}

	return object
}
