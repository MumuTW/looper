package config

import "testing"

func TestProjectRoleConfigsOverridesPlannerEscalationWithoutMutatingGlobal(t *testing.T) {
	globalMax, projectMax := 20, 5
	publicAPI := true
	cfg := Config{
		Roles:    RoleConfigs{Planner: PlannerRoleConfig{Escalation: &PlannerEscalationConfig{MaxEstimatedFiles: globalMax}}},
		Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Planner: &PartialPlannerRoleConfig{Escalation: &PartialPlannerEscalationConfig{MaxEstimatedFiles: &projectMax, PublicAPI: &publicAPI}}}}},
	}
	got := ProjectRoleConfigs(cfg, "demo").Planner.Escalation
	if got == nil || got.MaxEstimatedFiles != projectMax || !got.PublicAPI {
		t.Fatalf("project escalation = %#v", got)
	}
	if cfg.Roles.Planner.Escalation.MaxEstimatedFiles != globalMax || cfg.Roles.Planner.Escalation.PublicAPI {
		t.Fatalf("global escalation mutated = %#v", cfg.Roles.Planner.Escalation)
	}
}

func TestValidateRejectsNegativePlannerEscalationThreshold(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Planner.Escalation = &PlannerEscalationConfig{MaxEstimatedFiles: -1}
	err = Validate(cfg)
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}
	assertValidationIssue(t, validationErr, "roles.planner.escalation.maxEstimatedFiles", "must be an integer >= 0")
}
