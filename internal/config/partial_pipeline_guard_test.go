package config

import (
	"reflect"
	"strings"
	"testing"
)

// A configuration field passes through three hand-written stages before it can
// affect anything: the decoder's section registry, mergeConfig, and the partial
// clone. Each is an exhaustive switch nobody is forced to update, so a new field
// compiles, type-checks, validates, and is then silently dropped by whichever
// stage forgot it.
//
// That has happened three times in this package:
//
//	intake      never registered with the decoder — a documented, tested config
//	            section that could not reach Config at all
//	gatekeeper  missing from clonePartialRoleConfigs — a project-level override
//	            lost whenever configuration layers were cloned
//	deployer    the same clone, caught only because the previous one had just
//	            been found
//
// These tests make the omission mechanical to detect rather than dependent on
// someone remembering all three stages.

// TestEveryPartialConfigSectionIsRegisteredWithTheDecoder compares the decoder's
// registry against PartialConfig itself. An unregistered section is skipped
// without error, so nothing else in the pipeline can notice it.
func TestEveryPartialConfigSectionIsRegisteredWithTheDecoder(t *testing.T) {
	t.Parallel()

	registered := map[string]bool{}
	for _, section := range topLevelConfigSections(&PartialConfig{}) {
		registered[section.key] = true
	}

	partialType := reflect.TypeOf(PartialConfig{})
	for i := 0; i < partialType.NumField(); i++ {
		field := partialType.Field(i)
		key := jsonKey(field)
		if key == "" {
			continue
		}
		if !registered[key] {
			t.Errorf("PartialConfig.%s decodes from %q, but no decoder section is registered for it: a config file setting %q would be silently ignored",
				field.Name, key, key)
		}
	}
}

// TestDecoderRegistryHasNoSectionWithoutAField is the other direction: a key
// registered for a field that no longer exists is dead weight that reads as
// support for something unsupported.
func TestDecoderRegistryHasNoSectionWithoutAField(t *testing.T) {
	t.Parallel()

	fields := map[string]bool{}
	partialType := reflect.TypeOf(PartialConfig{})
	for i := 0; i < partialType.NumField(); i++ {
		if key := jsonKey(partialType.Field(i)); key != "" {
			fields[key] = true
		}
	}

	for _, section := range topLevelConfigSections(&PartialConfig{}) {
		if !fields[section.key] {
			t.Errorf("the decoder registers section %q, but PartialConfig has no field decoding from it", section.key)
		}
	}
}

// TestClonePartialConfigPreservesEveryField fills every field of PartialConfig
// with a distinctive value and checks the clone is equal. A field the clone
// forgets shows up as a difference rather than as a defect discovered in
// production months later.
func TestClonePartialConfigPreservesEveryField(t *testing.T) {
	t.Parallel()

	original := PartialConfig{}
	fillPartial(t, reflect.ValueOf(&original).Elem(), 0)

	cloned := clonePartialConfig(original)

	if diff := comparePartials(reflect.ValueOf(original), reflect.ValueOf(cloned), "PartialConfig"); diff != "" && !deliberatelyDropped(diff) {
		t.Fatalf("clonePartialConfig dropped or altered a field.\n%s\n\nA field missing from the clone is silently lost whenever configuration layers are combined.", diff)
	}
}

// deliberatelyDroppedPaths are fields the clone is meant to discard, each with
// the reason. Declaring them here keeps an intentional omission distinguishable
// from an accidental one: adding a path is a decision someone had to write down.
var deliberatelyDroppedPaths = map[string]string{
	"PartialConfig.Roles.Sweeper": "sweeper was retired; the field is accepted in older configuration files and ignored",
}

func deliberatelyDropped(diff string) bool {
	for path := range deliberatelyDroppedPaths {
		if strings.HasPrefix(diff, path+":") {
			return true
		}
	}
	return false
}

// jsonKey returns the field's JSON name, or "" when it is not serialized.
func jsonKey(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	return strings.TrimSpace(name)
}

// fillPartial populates every reachable field with a non-zero value, so a clone
// that drops one produces an observable difference. Depth is bounded because the
// config types nest several layers deep and a few are self-referential through
// maps.
func fillPartial(t *testing.T, value reflect.Value, depth int) {
	t.Helper()
	if depth > 8 || !value.CanSet() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		fillPartial(t, value.Elem(), depth+1)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if !value.Type().Field(i).IsExported() {
				continue
			}
			fillPartial(t, value.Field(i), depth+1)
		}
	case reflect.Map:
		mapValue := reflect.MakeMap(value.Type())
		key := reflect.New(value.Type().Key()).Elem()
		fillScalar(key)
		element := reflect.New(value.Type().Elem()).Elem()
		fillPartial(t, element, depth+1)
		mapValue.SetMapIndex(key, element)
		value.Set(mapValue)
	case reflect.Slice:
		slice := reflect.MakeSlice(value.Type(), 1, 1)
		fillPartial(t, slice.Index(0), depth+1)
		value.Set(slice)
	default:
		fillScalar(value)
	}
}

// fillScalar writes a distinctive value so a clone that zeroes a field is caught
// as well as one that drops it.
func fillScalar(value reflect.Value) {
	if !value.CanSet() {
		return
	}
	switch value.Kind() {
	case reflect.String:
		value.SetString("guard-value")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(7)
	case reflect.Float32, reflect.Float64:
		value.SetFloat(0.75)
	}
}

// comparePartials reports the first structural difference by path, so a failure
// names the field that was dropped rather than dumping two large structs.
func comparePartials(want, got reflect.Value, path string) string {
	if want.Kind() != got.Kind() {
		return path + ": kind changed"
	}
	switch want.Kind() {
	case reflect.Pointer:
		if want.IsNil() != got.IsNil() {
			if got.IsNil() {
				return path + ": present in the original, nil in the clone"
			}
			return path + ": nil in the original, present in the clone"
		}
		if want.IsNil() {
			return ""
		}
		return comparePartials(want.Elem(), got.Elem(), path)
	case reflect.Struct:
		for i := 0; i < want.NumField(); i++ {
			field := want.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			if diff := comparePartials(want.Field(i), got.Field(i), path+"."+field.Name); diff != "" {
				return diff
			}
		}
		return ""
	case reflect.Map:
		if want.Len() != got.Len() {
			return path + ": map length changed"
		}
		for _, key := range want.MapKeys() {
			gotValue := got.MapIndex(key)
			if !gotValue.IsValid() {
				return path + ": key missing from the clone"
			}
			if diff := comparePartials(want.MapIndex(key), gotValue, path+"["+key.String()+"]"); diff != "" {
				return diff
			}
		}
		return ""
	case reflect.Slice:
		if want.Len() != got.Len() {
			return path + ": slice length changed"
		}
		for i := 0; i < want.Len(); i++ {
			if diff := comparePartials(want.Index(i), got.Index(i), path+"[0]"); diff != "" {
				return diff
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			return path + ": value changed"
		}
		return ""
	}
}

// There is deliberately no blanket "the clone shares no pointers" test.
//
// clonePartialConfig exists to protect a caller's partial from
// normalizeLayerPartial, and that function copies each struct it is about to
// mutate before writing to it. A shallow inner pointer is therefore harmless
// here, and asserting otherwise would demand deep copies that nothing needs.
//
// Where aliasing does matter — a field whose value the clone hands out and
// something later edits in place — the owning test says so: see
// TestClonePartialRoleConfigsPreservesDeployer, which checks the command pointer
// and the environment map specifically.
