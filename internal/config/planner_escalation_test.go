package config

import "testing"

func TestPlannerEscalationDefaultsAreOptInWithDocumentedThresholds(t *testing.T) {
	t.Parallel()

	cfg, err := Normalize("")
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	escalation := cfg.Roles.Planner.Escalation
	if escalation.Enabled {
		t.Fatal("planner escalation defaults to enabled; it must be opt-in like hitl.enabled")
	}
	if escalation.MaxFilesTouched != 10 || escalation.MaxPackagesTouched != 3 {
		t.Fatalf("blast-radius defaults = (%d files, %d packages), want (10, 3)", escalation.MaxFilesTouched, escalation.MaxPackagesTouched)
	}
	if !escalation.OnPublicSurfaceChange || !escalation.OnADRConflict || !escalation.OnUnauthorizedDecision {
		t.Fatalf("criterion defaults = %#v, want all three enabled", escalation)
	}
}

func TestPlannerEscalationIsConfigurablePerProject(t *testing.T) {
	t.Parallel()

	cfg, err := Normalize("")
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	enabled := true
	maxFiles := 40
	surface := false
	cfg.Projects = []ProjectRefConfig{{
		ID: "project_1", Name: "Looper", RepoPath: "/tmp/repo",
		Roles: &PartialRoleConfigs{Planner: &PartialPlannerRoleConfig{Escalation: &PartialPlannerEscalationConfig{
			Enabled: &enabled, MaxFilesTouched: &maxFiles, OnPublicSurfaceChange: &surface,
		}}},
	}}

	global := ProjectRoleConfigs(cfg, "unknown_project").Planner.Escalation
	if global.Enabled || global.MaxFilesTouched != 10 {
		t.Fatalf("unknown project = %#v, want the global defaults", global)
	}
	project := ProjectRoleConfigs(cfg, "project_1").Planner.Escalation
	if !project.Enabled || project.MaxFilesTouched != 40 || project.OnPublicSurfaceChange {
		t.Fatalf("project override = %#v", project)
	}
	// Unset fields fall through to the global defaults rather than zeroing.
	if project.MaxPackagesTouched != 3 || !project.OnADRConflict || !project.OnUnauthorizedDecision {
		t.Fatalf("project override zeroed unset criteria: %#v", project)
	}
}

func TestValidatePlannerEscalationRejectsInertAndNegativePolicies(t *testing.T) {
	t.Parallel()

	var issues []ValidationIssue
	validatePlannerEscalation(PlannerEscalationConfig{Enabled: true}, "roles.planner.escalation", &issues)
	if len(issues) != 1 || issues[0].Path != "roles.planner.escalation" {
		t.Fatalf("issues = %#v, want one issue for an enabled policy with no criteria", issues)
	}

	issues = nil
	validatePlannerEscalation(PlannerEscalationConfig{MaxFilesTouched: -1, MaxPackagesTouched: -2}, "roles.planner.escalation", &issues)
	if len(issues) != 2 {
		t.Fatalf("issues = %#v, want both negative thresholds rejected", issues)
	}

	issues = nil
	validatePlannerEscalation(PlannerEscalationConfig{Enabled: true, OnADRConflict: true}, "roles.planner.escalation", &issues)
	if len(issues) != 0 {
		t.Fatalf("issues = %#v, want a single-criterion policy accepted", issues)
	}
}
