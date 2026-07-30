package api

import (
	"reflect"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestSerializeProjectValidationMakesPolicySourceVisible(t *testing.T) {
	t.Parallel()

	project := serializeProjectValidation(map[string]any{
		"validation": map[string]any{"commands": []any{"go test ./..."}},
	}, config.Config{})
	if project == nil || project.Source != "project" || !reflect.DeepEqual(project.Commands, []string{"go test ./..."}) || project.OptOut {
		t.Fatalf("project policy = %#v", project)
	}

	optOut := serializeProjectValidation(map[string]any{
		"validation": map[string]any{"optOut": true},
	}, config.Config{})
	if optOut == nil || optOut.Source != "project" || !optOut.OptOut || len(optOut.Commands) != 0 {
		t.Fatalf("opt-out policy = %#v", optOut)
	}

	legacy := serializeProjectValidation(map[string]any{}, config.Config{Defaults: config.DefaultsConfig{ValidationCommands: []string{"make check"}}})
	if legacy == nil || legacy.Source != "defaults" || !reflect.DeepEqual(legacy.Commands, []string{"make check"}) {
		t.Fatalf("legacy policy = %#v", legacy)
	}
}
