package projects

import (
	"errors"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestValidateCatalogValidationPoliciesPreservesIndicesAndQuarantinesLegacyProject(t *testing.T) {
	t.Parallel()

	global, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	global.Agent.Vendor = &vendor
	allowed := map[string]struct{}{"legacy": {}}
	materialized := []config.ProjectRefConfig{
		{ID: "legacy"},
		{ID: "invalid", Validation: &config.ProjectValidationConfig{}},
	}

	_, err = validateCatalogValidationPolicies(global, materialized, allowed)
	var validationErr *config.ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("validateCatalogValidationPolicies() error = %v, want ConfigValidationError", err)
	}
	if len(validationErr.Issues) != 1 || validationErr.Issues[0].Path != "projects[1].validation.commands" {
		t.Fatalf("validation issues = %#v, want original projects[1] index", validationErr.Issues)
	}

	materialized[1].Validation = &config.ProjectValidationConfig{OptOut: true}
	runnable, err := validateCatalogValidationPolicies(global, materialized, allowed)
	if err != nil {
		t.Fatalf("validateCatalogValidationPolicies(valid) error = %v", err)
	}
	if len(runnable) != 1 || runnable[0].ID != "invalid" {
		t.Fatalf("runnable = %#v, want legacy project quarantined", runnable)
	}
	if materialized[0].Validation != nil {
		t.Fatalf("materialized legacy validation = %#v, synthetic opt-out leaked", materialized[0].Validation)
	}

	global.Defaults.ValidationCommands = []string{"go test ./..."}
	runnable, err = validateCatalogValidationPolicies(global, materialized, allowed)
	if err != nil {
		t.Fatalf("validateCatalogValidationPolicies(default fallback) error = %v", err)
	}
	if len(runnable) != 2 {
		t.Fatalf("runnable with legacy default = %#v, want both projects", runnable)
	}
}
