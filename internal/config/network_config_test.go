package config

import "testing"

func TestValidateRejectsProgrammaticRoutedProject(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), Network: ProjectNetworkConfig{Mode: NetworkModeRouted}}}

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want routed validation failure")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("err = %T, want *ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "projects[0].network.mode" {
		t.Fatalf("issues = %#v, want one withdrawn routed-mode failure", validationErr.Issues)
	}
}

func TestValidateAcceptsLocalOnlyProject(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), Network: ProjectNetworkConfig{Mode: NetworkModeOff}}}

	if err := ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()}); err != nil {
		t.Fatalf("ValidateWithOptions() error = %v", err)
	}
}

func TestValidateReportsInvalidProjectNetworkModeOnce(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []ProjectRefConfig{{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), Network: ProjectNetworkConfig{Mode: NetworkMode("invalid")}}}

	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil {
		t.Fatal("ValidateWithOptions() error = nil, want invalid network mode failure")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("err = %T, want *ConfigValidationError", err)
	}
	count := 0
	for _, issue := range validationErr.Issues {
		if issue.Path == "projects[0].network.mode" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("network.mode issue count = %d, want 1; issues=%#v", count, validationErr.Issues)
	}
}
