package config

import "testing"

func TestEscalatorDefaultsAndMerge(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles.Escalator.Enabled || cfg.Roles.Escalator.CadenceSeconds != 3600 || cfg.Roles.Escalator.MaxItems != 500 {
		t.Fatalf("DefaultConfig().Roles.Escalator = %#v", cfg.Roles.Escalator)
	}
	enabled, cadence, retries := true, 600, int64(4)
	mergeConfig(&cfg, PartialConfig{Roles: &PartialRoleConfigs{Escalator: &PartialEscalatorRoleConfig{
		Enabled: &enabled, CadenceSeconds: &cadence, RetryAttemptThreshold: &retries,
	}}})
	if !cfg.Roles.Escalator.Enabled || cfg.Roles.Escalator.CadenceSeconds != 600 || cfg.Roles.Escalator.RetryAttemptThreshold != 4 {
		t.Fatalf("merged escalator = %#v", cfg.Roles.Escalator)
	}
}

func TestEscalatorValidation(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Escalator.CadenceSeconds = 59
	cfg.Roles.Escalator.RetryAttemptThreshold = 0
	cfg.Roles.Escalator.UnroutedAfterSeconds = 0
	cfg.Roles.Escalator.StaleHeadAfterSeconds = 0
	cfg.Roles.Escalator.MaxItems = 5001
	var issues []ValidationIssue
	validateEscalatorRoleConfig(cfg.Roles.Escalator, "roles.escalator", &issues)
	want := map[string]bool{
		"roles.escalator.cadenceSeconds": false, "roles.escalator.retryAttemptThreshold": false,
		"roles.escalator.unroutedAfterSeconds": false, "roles.escalator.staleHeadAfterSeconds": false,
		"roles.escalator.maxItems": false,
	}
	for _, issue := range issues {
		if _, ok := want[issue.Path]; ok {
			want[issue.Path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Fatalf("missing validation issue for %s: %#v", path, issues)
		}
	}
}

func TestClonePartialRoleConfigsPreservesEscalator(t *testing.T) {
	enabled, cadence := true, 900
	original := &PartialRoleConfigs{Escalator: &PartialEscalatorRoleConfig{Enabled: &enabled, CadenceSeconds: &cadence}}
	cloned := clonePartialRoleConfigs(original)
	if cloned == nil || cloned.Escalator == nil || cloned.Escalator.Enabled == nil || cloned.Escalator.CadenceSeconds == nil {
		t.Fatalf("clone = %#v", cloned)
	}
	*original.Escalator.CadenceSeconds = 1200
	if *cloned.Escalator.CadenceSeconds != 900 {
		t.Fatalf("clone cadence = %d, want independent 900", *cloned.Escalator.CadenceSeconds)
	}
}

func TestEscalatorRejectsProjectOverride(t *testing.T) {
	enabled := true
	_, err := Normalize(t.TempDir(), PartialConfig{Projects: &[]PartialProjectRefConfig{{
		ID: "demo", Name: "Demo", RepoPath: t.TempDir(),
		Roles: &PartialRoleConfigs{Escalator: &PartialEscalatorRoleConfig{Enabled: &enabled}},
	}}})
	validationErr, ok := err.(*ConfigValidationError)
	if !ok || len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "projects[0].roles.escalator" {
		t.Fatalf("Normalize() error = %#v, want global-only escalator path", err)
	}
}
