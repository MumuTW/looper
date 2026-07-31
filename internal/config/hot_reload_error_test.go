package config

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strings"
	"testing"
)

type clonePrivateState struct {
	values []int
}

func (s *clonePrivateState) SetValue(index, value int) {
	s.values[index] = value
}

func (s *clonePrivateState) Value(index int) int {
	return s.values[index]
}

type cloneDefinedPointerTarget struct {
	Value int
}

type cloneDefinedPointer *cloneDefinedPointerTarget

type clonePointerJSONValue struct {
	Value int `json:"-"`
}

func (v *clonePointerJSONValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Encoded int `json:"encoded"`
	}{Encoded: v.Value})
}

func (v *clonePointerJSONValue) UnmarshalJSON(raw []byte) error {
	var payload struct {
		Encoded int `json:"encoded"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	v.Value = payload.Encoded
	return nil
}

func TestCloneConfigDetachesNonJSONValuesWithoutPanicking(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	callback := func() {}
	nested := map[string]any{"callback": callback, "labels": []string{"one"}}
	cfg.Agent.Params["custom"] = nested

	cloned := CloneConfig(cfg)
	clonedNested, ok := cloned.Agent.Params["custom"].(map[string]any)
	if !ok {
		t.Fatalf("cloned custom params = %#v", cloned.Agent.Params["custom"])
	}
	if reflect.ValueOf(clonedNested["callback"]).Pointer() != reflect.ValueOf(callback).Pointer() {
		t.Fatal("function-valued config parameter was not preserved")
	}
	clonedNested["labels"].([]string)[0] = "changed"
	if got := nested["labels"].([]string)[0]; got != "one" {
		t.Fatalf("source nested slice changed = %q", got)
	}
}

func TestCloneConfigPreservesCyclesWithoutAliasingSource(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	cfg.Agent.Params["cycle"] = cycle

	cloned := CloneConfig(cfg)
	clonedCycle := cloned.Agent.Params["cycle"].(map[string]any)
	if reflect.ValueOf(clonedCycle).Pointer() == reflect.ValueOf(cycle).Pointer() {
		t.Fatal("cycle map aliases source")
	}
	if got := clonedCycle["self"].(map[string]any); reflect.ValueOf(got).Pointer() != reflect.ValueOf(clonedCycle).Pointer() {
		t.Fatal("cycle did not point at cloned map")
	}
}

func TestCloneConfigSeparatesOverlappingSlicesAndUnexportedState(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := []byte{1, 2, 3}
	cfg.Agent.Params["short"] = base[:1]
	cfg.Agent.Params["long"] = base[:2]
	originalBig := big.NewInt(7)
	cfg.Agent.Params["big"] = originalBig

	cloned := CloneConfig(cfg)
	short := cloned.Agent.Params["short"].([]byte)
	long := cloned.Agent.Params["long"].([]byte)
	if len(short) != 1 || len(long) != 2 {
		t.Fatalf("overlapping slice lengths = (%d, %d), want (1, 2)", len(short), len(long))
	}
	long[0] = 9
	if base[0] != 1 || short[0] != 1 {
		t.Fatalf("overlapping slice clone aliased source or sibling: base=%v short=%v", base, short)
	}

	clonedBig := cloned.Agent.Params["big"].(*big.Int)
	clonedBig.SetBit(clonedBig, 1, 0)
	if originalBig.String() != "7" {
		t.Fatalf("big.Int clone shares unexported storage: original=%s", originalBig)
	}
}

func TestCloneValueDetachesUnexportedMutableState(t *testing.T) {
	original := &clonePrivateState{values: []int{1, 2}}
	cloned := CloneValue(map[string]any{"state": original})
	state, ok := cloned["state"].(*clonePrivateState)
	if !ok {
		t.Fatalf("cloned state type = %T, want *clonePrivateState", cloned["state"])
	}
	state.SetValue(0, 9)
	if got := original.Value(0); got != 1 {
		t.Fatalf("source private state changed through clone: %d", got)
	}
}

func TestCloneValuePreservesDefinedPointerTypes(t *testing.T) {
	original := cloneDefinedPointer(&cloneDefinedPointerTarget{Value: 42})
	cloned := CloneValue(map[string]any{"pointer": original})
	got, ok := cloned["pointer"].(cloneDefinedPointer)
	if !ok {
		t.Fatalf("cloned pointer type = %T, want cloneDefinedPointer", cloned["pointer"])
	}
	if (*got).Value != 42 {
		t.Fatalf("cloned pointer value = %d, want 42", (*got).Value)
	}
	(*got).Value = 7
	if (*original).Value != 42 {
		t.Fatalf("source pointer changed through clone: %d", (*original).Value)
	}
}

func TestCloneValueInvokesPointerJSONMarshalMethod(t *testing.T) {
	cloned := CloneValue(map[string]any{"value": &clonePointerJSONValue{Value: 42}})
	got, ok := cloned["value"].(*clonePointerJSONValue)
	if !ok {
		t.Fatalf("cloned JSON value type = %T, want *clonePointerJSONValue", cloned["value"])
	}
	if got.Value != 42 {
		t.Fatalf("cloned JSON value = %d, want 42", got.Value)
	}
}

func TestRestartRequiredChangesCheckedReturnsComparisonError(t *testing.T) {
	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	newConfig := CloneConfig(oldConfig)
	newConfig.Agent.Params["callback"] = func() {}

	changes, err := RestartRequiredChangesChecked(oldConfig, newConfig)
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("RestartRequiredChangesChecked() = (%#v, %v), want marshal error", changes, err)
	}
	if got := RestartRequiredChanges(oldConfig, newConfig); !reflect.DeepEqual(got, []string{"<config-comparison-error>"}) {
		t.Fatalf("RestartRequiredChanges() = %#v, want fail-closed sentinel", got)
	}
}
