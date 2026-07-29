package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultConfigHasNoValidationCommands(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if len(cfg.Defaults.ValidationCommands) != 0 {
		t.Fatalf("DefaultConfig().Defaults.ValidationCommands = %#v, want empty", cfg.Defaults.ValidationCommands)
	}
	if commands := ResolveValidationCommands(cfg); commands != nil {
		t.Fatalf("ResolveValidationCommands(default) = %#v, want nil", commands)
	}
}

func TestReadConfigFileParsesValidationCommands(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	contents := "[defaults]\nvalidationCommands = [\"go vet ./...\", \"go test ./...\"]\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	partial, present, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error = %v", err)
	}
	if !present {
		t.Fatal("readConfigFile() present = false, want true")
	}
	if partial.Defaults == nil || partial.Defaults.ValidationCommands == nil {
		t.Fatalf("readConfigFile() defaults = %#v, want validationCommands set", partial.Defaults)
	}
	if want := []string{"go vet ./...", "go test ./..."}; !reflect.DeepEqual(*partial.Defaults.ValidationCommands, want) {
		t.Fatalf("readConfigFile() validationCommands = %#v, want %#v", *partial.Defaults.ValidationCommands, want)
	}
}

func TestNormalizeAppliesValidationCommandsOverride(t *testing.T) {
	t.Parallel()

	commands := []string{"make check"}
	cfg, err := Normalize(t.TempDir(), PartialConfig{Defaults: &PartialDefaultsConfig{ValidationCommands: &commands}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if !reflect.DeepEqual(cfg.Defaults.ValidationCommands, commands) {
		t.Fatalf("Normalize().Defaults.ValidationCommands = %#v, want %#v", cfg.Defaults.ValidationCommands, commands)
	}

	// Arrays are replaced as a whole by later layers.
	replacement := []string{"go build ./..."}
	cfg, err = Normalize(t.TempDir(),
		PartialConfig{Defaults: &PartialDefaultsConfig{ValidationCommands: &commands}},
		PartialConfig{Defaults: &PartialDefaultsConfig{ValidationCommands: &replacement}},
	)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if !reflect.DeepEqual(cfg.Defaults.ValidationCommands, replacement) {
		t.Fatalf("Normalize().Defaults.ValidationCommands = %#v, want %#v", cfg.Defaults.ValidationCommands, replacement)
	}

	// An omitted partial keeps the previous layer's value.
	cfg, err = Normalize(t.TempDir(),
		PartialConfig{Defaults: &PartialDefaultsConfig{ValidationCommands: &commands}},
		PartialConfig{Defaults: &PartialDefaultsConfig{}},
	)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if !reflect.DeepEqual(cfg.Defaults.ValidationCommands, commands) {
		t.Fatalf("Normalize().Defaults.ValidationCommands = %#v, want %#v", cfg.Defaults.ValidationCommands, commands)
	}
}

func TestValidateRejectsBlankValidationCommands(t *testing.T) {
	t.Parallel()

	cfg, err := Normalize(t.TempDir())
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	cfg.Defaults.ValidationCommands = []string{"go test ./...", "   "}

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want ConfigValidationError")
	}
	validationErr := &ConfigValidationError{}
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateWithOptions() error = %T, want ConfigValidationError", err)
	}
	if !validationIssuesContainPath(validationErr.Issues, "defaults.validationCommands[1]") {
		t.Fatalf("validation issues = %#v, missing defaults.validationCommands[1]", validationErr.Issues)
	}
	if validationIssuesContainPath(validationErr.Issues, "defaults.validationCommands[0]") {
		t.Fatalf("validation issues = %#v, unexpected defaults.validationCommands[0]", validationErr.Issues)
	}
}

func TestValidateAcceptsValidationCommands(t *testing.T) {
	t.Parallel()

	cfg, err := Normalize(t.TempDir())
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	cfg.Defaults.ValidationCommands = []string{"go vet ./...", "go test ./..."}

	if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("ValidateWithOptions() error = %v", err)
	}
}

func TestResolveValidationCommandsTrimsAndDetaches(t *testing.T) {
	t.Parallel()

	cfg := Config{Defaults: DefaultsConfig{ValidationCommands: []string{"  go test ./...  ", "", "   ", "go build ./..."}}}
	commands := ResolveValidationCommands(cfg)
	want := []string{"go test ./...", "go build ./..."}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("ResolveValidationCommands() = %#v, want %#v", commands, want)
	}

	commands[0] = "mutated"
	if cfg.Defaults.ValidationCommands[0] != "  go test ./...  " {
		t.Fatal("ResolveValidationCommands() aliased the config slice, want a detached copy")
	}
}
