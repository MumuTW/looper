package config

import (
	"encoding/json"
	"fmt"
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

// TestEveryDecoderRegistryCallbackPopulatesItsPartialConfigField proves that
// the registry does more than name every section. A callback wired to a
// neighboring field would satisfy both registry-key tests while discarding the
// setting a user actually authored.
func TestEveryDecoderRegistryCallbackPopulatesItsPartialConfigField(t *testing.T) {
	t.Parallel()

	fieldByKey := partialConfigFieldsByJSONKey(t)
	for _, registeredSection := range topLevelConfigSections(&PartialConfig{}) {
		registeredSection := registeredSection
		t.Run(registeredSection.key, func(t *testing.T) {
			field, ok := fieldByKey[registeredSection.key]
			if !ok {
				t.Fatalf("decoder section %q has no PartialConfig field", registeredSection.key)
			}

			partial := PartialConfig{}
			section := decoderSectionForKey(t, &partial, registeredSection.key)
			if err := section.decode(emptyJSONValueFor(field.Type)); err != nil {
				t.Fatalf("decode %q: %v", registeredSection.key, err)
			}

			decoded := reflect.ValueOf(partial).FieldByIndex(field.Index)
			if isZeroValue(decoded) {
				t.Fatalf("decoder section %q left PartialConfig.%s empty", registeredSection.key, field.Name)
			}
		})
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

// TestMergeConfigAppliesEveryPartialConfigField starts from a zero Config, so
// a merge case omitted from mergeConfig cannot hide behind a default value.
// PartialConfig and Config deliberately share their top-level names except for
// reviewer, which is the legacy spelling of Roles.Reviewer.Behavior.
func TestMergeConfigAppliesEveryPartialConfigField(t *testing.T) {
	t.Parallel()

	partialType := reflect.TypeOf(PartialConfig{})
	for i := 0; i < partialType.NumField(); i++ {
		partialField := partialType.Field(i)
		t.Run(partialField.Name, func(t *testing.T) {
			partial := PartialConfig{}
			assertFullyPopulated(t, reflect.ValueOf(&partial).Elem().Field(i), "PartialConfig."+partialField.Name)

			var merged Config
			mergeConfig(&merged, partial)

			outputFieldName := partialConfigOutputField(partialField.Name)
			output := reflect.ValueOf(merged).FieldByName(outputFieldName)
			if !output.IsValid() {
				t.Fatalf("PartialConfig.%s has no declared Config output field; add it to partialConfigOutputField before relying on mergeConfig", partialField.Name)
			}
			if isZeroValue(output) {
				t.Fatalf("mergeConfig left Config.%s empty after a populated PartialConfig.%s; the partial setting is being silently discarded", outputFieldName, partialField.Name)
			}
		})
	}
}

// TestClonePartialConfigPreservesEveryField fills every field of PartialConfig
// with a distinctive value and checks the clone is equal. A field the clone
// forgets shows up as a difference rather than as a defect discovered in
// production months later.
func TestClonePartialConfigPreservesEveryField(t *testing.T) {
	t.Parallel()

	original := PartialConfig{}
	assertFullyPopulatedPartial(t, reflect.ValueOf(&original).Elem())
	if original.Roles == nil || original.Roles.Reviewer == nil || original.Roles.Reviewer.Discovery == nil || original.Roles.Reviewer.Discovery.Triggers == nil || original.Roles.Reviewer.Discovery.Triggers.Labels == nil {
		t.Fatal("fixture did not populate PartialConfig.Roles.Reviewer.Discovery.Triggers.Labels beyond the former depth cutoff")
	}

	cloned := clonePartialConfig(original)

	var unexpected []string
	for _, diff := range comparePartials(reflect.ValueOf(original), reflect.ValueOf(cloned), "PartialConfig") {
		if !deliberatelyDropped(diff.path) {
			unexpected = append(unexpected, diff.String())
		}
	}
	if len(unexpected) > 0 {
		t.Fatalf("clonePartialConfig dropped or altered fields.\n%s\n\nA field missing from the clone is silently lost whenever configuration layers are combined.", strings.Join(unexpected, "\n"))
	}
}

func TestDeliberatelyDroppedDifferenceDoesNotHideLaterDifference(t *testing.T) {
	t.Parallel()

	sweeper := map[string]any{"retired": "guard-value"}
	providers := []PartialProviderConfig{{ID: "guard-value"}}
	want := PartialConfig{
		Roles:     &PartialRoleConfigs{Sweeper: &sweeper},
		Providers: &providers,
	}
	got := PartialConfig{Roles: &PartialRoleConfigs{}}

	var unexpected []string
	for _, diff := range comparePartials(reflect.ValueOf(want), reflect.ValueOf(got), "PartialConfig") {
		if !deliberatelyDropped(diff.path) {
			unexpected = append(unexpected, diff.String())
		}
	}
	if len(unexpected) != 1 || !strings.HasPrefix(unexpected[0], "PartialConfig.Providers:") {
		t.Fatalf("unexpected differences after filtering retired sweeper: %v", unexpected)
	}
}

// deliberatelyDroppedPaths are fields the clone is meant to discard, each with
// the reason. Declaring them here keeps an intentional omission distinguishable
// from an accidental one: adding a path is a decision someone had to write down.
var deliberatelyDroppedPaths = map[string]string{
	"Roles.Sweeper": "sweeper was retired; the field is accepted in older configuration files and ignored",
}

func deliberatelyDropped(path string) bool {
	for droppedPath := range deliberatelyDroppedPaths {
		if strings.HasSuffix(path, "."+droppedPath) || path == droppedPath {
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

// assertFullyPopulatedPartial fills every reachable field with a non-zero value,
// so a clone that drops one produces an observable difference.
func assertFullyPopulatedPartial(t *testing.T, value reflect.Value) {
	t.Helper()
	assertFullyPopulated(t, value, "PartialConfig")
}

func assertFullyPopulated(t *testing.T, value reflect.Value, path string) {
	t.Helper()

	if skipped := fillPartial(value, path, map[reflect.Type]string{}); len(skipped) > 0 {
		t.Fatalf("partial fixture skipped fields:\n%s", strings.Join(skipped, "\n"))
	}
}

// fillPartial populates each reachable field. It uses the active type path to
// detect recursive schemas instead of an arbitrary depth cap; any skipped
// field is a test failure, so a new deep field cannot silently become zero in
// this fixture.
func fillPartial(value reflect.Value, path string, active map[reflect.Type]string) []string {
	if !value.CanSet() {
		return []string{path + ": cannot set"}
	}

	typ := value.Type()
	switch value.Kind() {
	case reflect.Pointer:
		if ancestor, ok := active[typ]; ok {
			return []string{fmt.Sprintf("%s: recursive type %s already active at %s", path, typ, ancestor)}
		}
		active[typ] = path
		defer delete(active, typ)
		value.Set(reflect.New(value.Type().Elem()))
		return fillPartial(value.Elem(), path, active)
	case reflect.Struct:
		if ancestor, ok := active[typ]; ok {
			return []string{fmt.Sprintf("%s: recursive type %s already active at %s", path, typ, ancestor)}
		}
		active[typ] = path
		defer delete(active, typ)
		var skipped []string
		for i := 0; i < value.NumField(); i++ {
			if !value.Type().Field(i).IsExported() {
				continue
			}
			field := value.Type().Field(i)
			skipped = append(skipped, fillPartial(value.Field(i), path+"."+field.Name, active)...)
		}
		return skipped
	case reflect.Map:
		if ancestor, ok := active[typ]; ok {
			return []string{fmt.Sprintf("%s: recursive type %s already active at %s", path, typ, ancestor)}
		}
		active[typ] = path
		defer delete(active, typ)
		mapValue := reflect.MakeMap(value.Type())
		key := reflect.New(value.Type().Key()).Elem()
		keySkipped := fillPartial(key, path+"[key]", active)
		element := reflect.New(value.Type().Elem()).Elem()
		elementSkipped := fillPartial(element, path+"[value]", active)
		mapValue.SetMapIndex(key, element)
		value.Set(mapValue)
		return append(keySkipped, elementSkipped...)
	case reflect.Slice:
		if ancestor, ok := active[typ]; ok {
			return []string{fmt.Sprintf("%s: recursive type %s already active at %s", path, typ, ancestor)}
		}
		active[typ] = path
		defer delete(active, typ)
		slice := reflect.MakeSlice(value.Type(), 1, 1)
		skipped := fillPartial(slice.Index(0), path+"[0]", active)
		value.Set(slice)
		return skipped
	case reflect.Interface:
		value.Set(reflect.ValueOf("guard-value"))
		return nil
	default:
		if fillScalar(value) {
			return nil
		}
		return []string{path + ": unsupported kind " + value.Kind().String()}
	}
}

// fillScalar writes a distinctive value so a clone that zeroes a field is caught
// as well as one that drops it.
func fillScalar(value reflect.Value) bool {
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
	default:
		return false
	}
	return true
}

type partialDifference struct {
	path   string
	detail string
}

func (difference partialDifference) String() string {
	return difference.path + ": " + difference.detail
}

// comparePartials reports every structural difference by path. The clone has
// one documented omission; collecting all differences makes that exception
// unable to hide a later accidental omission.
func comparePartials(want, got reflect.Value, path string) []partialDifference {
	if want.Kind() != got.Kind() {
		return []partialDifference{{path: path, detail: "kind changed"}}
	}
	switch want.Kind() {
	case reflect.Pointer:
		if want.IsNil() != got.IsNil() {
			if got.IsNil() {
				return []partialDifference{{path: path, detail: "present in the original, nil in the clone"}}
			}
			return []partialDifference{{path: path, detail: "nil in the original, present in the clone"}}
		}
		if want.IsNil() {
			return nil
		}
		return comparePartials(want.Elem(), got.Elem(), path)
	case reflect.Struct:
		var differences []partialDifference
		for i := 0; i < want.NumField(); i++ {
			field := want.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			differences = append(differences, comparePartials(want.Field(i), got.Field(i), path+"."+field.Name)...)
		}
		return differences
	case reflect.Map:
		var differences []partialDifference
		if want.Len() != got.Len() {
			differences = append(differences, partialDifference{path: path, detail: "map length changed"})
		}
		for _, key := range want.MapKeys() {
			gotValue := got.MapIndex(key)
			if !gotValue.IsValid() {
				differences = append(differences, partialDifference{path: path + "[" + fmt.Sprint(key.Interface()) + "]", detail: "key missing from the clone"})
				continue
			}
			differences = append(differences, comparePartials(want.MapIndex(key), gotValue, path+"["+fmt.Sprint(key.Interface())+"]")...)
		}
		return differences
	case reflect.Slice:
		var differences []partialDifference
		if want.Len() != got.Len() {
			differences = append(differences, partialDifference{path: path, detail: "slice length changed"})
		}
		for i := 0; i < want.Len() && i < got.Len(); i++ {
			differences = append(differences, comparePartials(want.Index(i), got.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
		return differences
	default:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			return []partialDifference{{path: path, detail: "value changed"}}
		}
		return nil
	}
}

func partialConfigFieldsByJSONKey(t *testing.T) map[string]reflect.StructField {
	t.Helper()

	fields := make(map[string]reflect.StructField)
	partialType := reflect.TypeOf(PartialConfig{})
	for i := 0; i < partialType.NumField(); i++ {
		field := partialType.Field(i)
		if key := jsonKey(field); key != "" {
			fields[key] = field
		}
	}
	return fields
}

func decoderSectionForKey(t *testing.T, partial *PartialConfig, key string) topLevelConfigSection {
	t.Helper()

	for _, section := range topLevelConfigSections(partial) {
		if section.key == key {
			return section
		}
	}
	t.Fatalf("decoder section %q not found", key)
	return topLevelConfigSection{}
}

func emptyJSONValueFor(typ reflect.Type) json.RawMessage {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Slice:
		return json.RawMessage("[]")
	case reflect.Map:
		return json.RawMessage("{}")
	default:
		return json.RawMessage("{}")
	}
}

func partialConfigOutputField(partialField string) string {
	if partialField == "LegacyReviewer" {
		return "Roles"
	}
	return partialField
}

func isZeroValue(value reflect.Value) bool {
	return value.IsZero()
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
