package config

import "testing"

func TestCoordinatorMarkReadyDefaultsToDisabledLooperOnlyScope(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	markReady := cfg.Roles.Coordinator.MarkReady
	if markReady.Enabled || markReady.Scope != CoordinatorMarkReadyScopeLooperOnly {
		t.Fatalf("default markReady = %#v, want disabled looper-only config", markReady)
	}
}

func TestProjectRoleConfigsMergesCoordinatorMarkReadyOverride(t *testing.T) {
	enabled := true
	cfg := Config{
		Roles:    RoleConfigs{Coordinator: CoordinatorRoleConfig{MarkReady: CoordinatorMarkReadyConfig{Enabled: false, Scope: CoordinatorMarkReadyScopeLooperOnly}}},
		Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Coordinator: &PartialCoordinatorRoleConfig{MarkReady: &PartialCoordinatorMarkReadyConfig{Enabled: &enabled}}}}},
	}
	got := ProjectRoleConfigs(cfg, "demo").Coordinator.MarkReady
	if !got.Enabled || got.Scope != CoordinatorMarkReadyScopeLooperOnly {
		t.Fatalf("project markReady = %#v, want enabled looper-only config", got)
	}
}

// Publishing a draft only starts a review if something will claim it. In the
// default single-daemon shape nothing will: the reviewer refuses Pull Requests
// authored by the account it runs as, and GitHub cannot request review from a
// Pull Request's own author.
func TestMarkReadyReviewerUnreachableProjects(t *testing.T) {
	base, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	base.Projects = []ProjectRefConfig{{ID: "demo"}}
	base.Roles.Coordinator.MarkReady = CoordinatorMarkReadyConfig{Enabled: true, Scope: CoordinatorMarkReadyScopeLooperOnly}

	if got := MarkReadyReviewerUnreachableProjects(base); len(got) != 1 || got[0] != "demo" {
		t.Fatalf("unreachable projects = %#v, want [demo] under the default reviewer config", got)
	}

	selfReviewing := base
	selfReviewing.Roles.Reviewer.Discovery.Triggers.EnableSelfReview = true
	selfReviewing.Roles.Coding = CodingRolesFromLegacy(selfReviewing.Roles)
	if got := MarkReadyReviewerUnreachableProjects(selfReviewing); len(got) != 0 {
		t.Fatalf("unreachable projects = %#v, want none once the reviewer reviews its own pull requests", got)
	}

	disabled := base
	disabled.Roles.Coordinator.MarkReady.Enabled = false
	if got := MarkReadyReviewerUnreachableProjects(disabled); len(got) != 0 {
		t.Fatalf("unreachable projects = %#v, want none while mark-ready is off", got)
	}
}

func TestCoordinatorMarkReadyRejectsForeignScope(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.MarkReady = CoordinatorMarkReadyConfig{Enabled: true, Scope: CoordinatorMarkReadyScope("all")}
	issues := []ValidationIssue{}
	validateCoordinatorRoleConfig(cfg.Roles.Coordinator, "roles.coordinator", &issues)
	if len(issues) != 1 || issues[0].Path != "roles.coordinator.markReady.scope" {
		t.Fatalf("issues = %#v, want markReady scope validation", issues)
	}
}

func TestPartialCoordinatorMarkReadyRejectsForeignScope(t *testing.T) {
	scope := CoordinatorMarkReadyScope("everyone")
	issues := []ValidationIssue{}
	validatePartialCoordinatorMarkReady(PartialCoordinatorMarkReadyConfig{Scope: &scope}, "projects[0].roles.coordinator.markReady", &issues)
	if len(issues) != 1 || issues[0].Path != "projects[0].roles.coordinator.markReady.scope" {
		t.Fatalf("issues = %#v, want partial markReady scope validation", issues)
	}
}

func TestClonePartialRoleConfigsPreservesCoordinatorMarkReadyFields(t *testing.T) {
	enabled := true
	scope := CoordinatorMarkReadyScopeLooperOnly
	original := &PartialRoleConfigs{Coordinator: &PartialCoordinatorRoleConfig{MarkReady: &PartialCoordinatorMarkReadyConfig{Enabled: &enabled, Scope: &scope}}}
	cloned := clonePartialRoleConfigs(original)
	if cloned == nil || cloned.Coordinator == nil || cloned.Coordinator.MarkReady == nil || cloned.Coordinator.MarkReady.Enabled == nil || !*cloned.Coordinator.MarkReady.Enabled {
		t.Fatalf("cloned markReady = %#v", cloned)
	}
	*original.Coordinator.MarkReady.Enabled = false
	if !*cloned.Coordinator.MarkReady.Enabled {
		t.Fatal("clone aliases coordinator markReady enabled pointer")
	}
	*original.Coordinator.MarkReady.Scope = CoordinatorMarkReadyScope("all")
	if *cloned.Coordinator.MarkReady.Scope != CoordinatorMarkReadyScopeLooperOnly {
		t.Fatal("clone aliases coordinator markReady scope pointer")
	}
}

func TestNormalizeTrimsCoordinatorMarkReadyScope(t *testing.T) {
	scope := CoordinatorMarkReadyScope(" looper-only ")
	cfg := CoordinatorMarkReadyConfig{Scope: CoordinatorMarkReadyScope("all")}
	mergeCoordinatorMarkReadyConfig(&cfg, PartialCoordinatorMarkReadyConfig{Scope: &scope})
	if cfg.Scope != CoordinatorMarkReadyScopeLooperOnly {
		t.Fatalf("normalized scope = %q, want %q", cfg.Scope, CoordinatorMarkReadyScopeLooperOnly)
	}
}
