package config

import "testing"

func TestDefaultReviewerConvergenceConfig(t *testing.T) {
	got := DefaultReviewerConvergenceConfig()
	if got.MaxConsecutiveUnproductive != 3 || got.MaxFixerAttemptsPerItem != 4 || got.MaxTotalRounds != 40 || got.SeverityFloor != ReviewerSeverityFloorNonBlocking {
		t.Fatalf("DefaultReviewerConvergenceConfig() = %#v", got)
	}
}

func TestProjectReviewerConvergenceOverrideKeepsGlobalImmutable(t *testing.T) {
	global := DefaultReviewerConvergenceConfig()
	projectRounds := 9
	cfg := Config{
		Roles:    RoleConfigs{Reviewer: ReviewerRoleConfig{Behavior: ReviewerConfig{Convergence: &global}}},
		Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{Behavior: &PartialReviewerConfig{Convergence: &PartialReviewerConvergenceConfig{MaxTotalRounds: &projectRounds}}}}}},
	}
	got := ProjectRoleConfigs(cfg, "demo").Reviewer.Behavior.Convergence
	if got == nil || got.MaxTotalRounds != projectRounds || got.MaxConsecutiveUnproductive != global.MaxConsecutiveUnproductive {
		t.Fatalf("project convergence = %#v", got)
	}
	if cfg.Roles.Reviewer.Behavior.Convergence.MaxTotalRounds != global.MaxTotalRounds {
		t.Fatalf("global convergence mutated = %#v", cfg.Roles.Reviewer.Behavior.Convergence)
	}
}

func TestValidateRejectsInvalidReviewerConvergence(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Reviewer.Behavior.Convergence = &ReviewerConvergenceConfig{MaxTotalRounds: 0, MaxConsecutiveUnproductive: 0, MaxFixerAttemptsPerItem: 0, SeverityFloor: "bogus"}
	err = Validate(cfg)
	if err == nil {
		t.Fatal("Validate() error = nil")
	}
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
	}
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.convergence.maxConsecutiveUnproductive", "must be a positive integer")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.convergence.maxFixerAttemptsPerItem", "must be a positive integer")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.convergence.maxTotalRounds", "must be a positive integer")
	assertValidationIssue(t, validationErr, "roles.reviewer.behavior.convergence.severityFloor", "must be one of: blocking, non_blocking, all")
}
