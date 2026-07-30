package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadConfigFileParsesProjectValidationPolicy(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[[projects]]
id = "looper"
name = "Looper"
repoPath = "/repos/looper"

[projects.validation]
commands = ["scripts/verify.sh", "go test ./..."]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	partial, present, err := readConfigFile(path)
	if err != nil || !present {
		t.Fatalf("readConfigFile() = (%#v, %t, %v)", partial, present, err)
	}
	if partial.Projects == nil || len(*partial.Projects) != 1 || (*partial.Projects)[0].Validation == nil || (*partial.Projects)[0].Validation.Commands == nil {
		t.Fatalf("projects = %#v, want project validation commands", partial.Projects)
	}
	if got, want := *(*partial.Projects)[0].Validation.Commands, []string{"scripts/verify.sh", "go test ./..."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %#v, want %#v", got, want)
	}
}

func TestProjectValidationCommandsOverrideLegacyDefaults(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Defaults: DefaultsConfig{ValidationCommands: []string{"make check"}},
		Projects: []ProjectRefConfig{
			{ID: "looper", Validation: &ProjectValidationConfig{Commands: []string{"  scripts/verify.sh  "}}},
			{ID: "novel"},
			{ID: "fluenx", Validation: &ProjectValidationConfig{OptOut: true}},
		},
	}

	if got, want := ResolveProjectValidationCommands(cfg, "looper"), []string{"scripts/verify.sh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveProjectValidationCommands(looper) = %#v, want %#v", got, want)
	}
	if got, want := ResolveProjectValidationCommands(cfg, "novel"), []string{"make check"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveProjectValidationCommands(novel) = %#v, want legacy fallback %#v", got, want)
	}
	if got := ResolveProjectValidationCommands(cfg, "fluenx"); got != nil {
		t.Fatalf("ResolveProjectValidationCommands(fluenx) = %#v, want explicit opt-out", got)
	}
	if !ProjectValidationUsesLegacyDefaults(cfg, "novel") || ProjectValidationUsesLegacyDefaults(cfg, "looper") {
		t.Fatal("ProjectValidationUsesLegacyDefaults() did not preserve source provenance")
	}
	if !ProjectValidationOptedOut(cfg, "fluenx") || ProjectValidationOptedOut(cfg, "novel") {
		t.Fatal("ProjectValidationOptedOut() did not distinguish explicit opt-out")
	}
}

func TestValidateProjectValidationFailsClosedUnlessExplicitlyConfigured(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: t.TempDir()}}
	vendor := AgentVendorCodex
	cfg.Agent.Vendor = &vendor

	err = ValidateProjectValidationPolicies(cfg)
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateWithOptions() error = %v, want ConfigValidationError", err)
	}
	if !validationIssuesContainPath(validationErr.Issues, "projects[0].validation") {
		t.Fatalf("validation issues = %#v, missing projects[0].validation", validationErr.Issues)
	}

	cfg.Projects[0].Validation = &ProjectValidationConfig{OptOut: true}
	if err := ValidateProjectValidationPolicies(cfg); err != nil {
		t.Fatalf("explicit opt-out rejected: %v", err)
	}
}

func TestValidateProjectValidationRejectsAmbiguousOrInvalidPolicies(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		validation *ProjectValidationConfig
		wantPath   string
	}{
		{name: "empty commands", validation: &ProjectValidationConfig{}, wantPath: "projects[0].validation.commands"},
		{name: "blank command", validation: &ProjectValidationConfig{Commands: []string{"go test ./...", "  "}}, wantPath: "projects[0].validation.commands[1]"},
		{name: "opt out with commands", validation: &ProjectValidationConfig{Commands: []string{"go test ./..."}, OptOut: true}, wantPath: "projects[0].validation"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			cfg.Projects = []ProjectRefConfig{{ID: "demo", Name: "Demo", RepoPath: t.TempDir(), Validation: testCase.validation}}
			err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
			var validationErr *ConfigValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateWithOptions() error = %v, want ConfigValidationError", err)
			}
			if !validationIssuesContainPath(validationErr.Issues, testCase.wantPath) {
				t.Fatalf("validation issues = %#v, missing %s", validationErr.Issues, testCase.wantPath)
			}
		})
	}
}

func TestNormalizeProjectValidationKeepsDistinctProjectPolicies(t *testing.T) {
	t.Parallel()

	looperCommands := []string{"scripts/verify.sh"}
	optOut := true
	cfg, err := Normalize(t.TempDir(), PartialConfig{Projects: &[]PartialProjectRefConfig{
		{ID: "looper", Name: "Looper", RepoPath: "/repos/looper", Validation: &PartialProjectValidationConfig{Commands: &looperCommands}},
		{ID: "fluenx", Name: "FluenX", RepoPath: "/repos/fluenx", Validation: &PartialProjectValidationConfig{OptOut: &optOut}},
	}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got := cfg.Projects[0].Validation; got == nil || !reflect.DeepEqual(got.Commands, looperCommands) || got.OptOut {
		t.Fatalf("looper validation = %#v", got)
	}
	if got := cfg.Projects[1].Validation; got == nil || !got.OptOut || len(got.Commands) != 0 {
		t.Fatalf("fluenx validation = %#v", got)
	}
}
