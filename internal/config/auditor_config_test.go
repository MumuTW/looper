package config

import "testing"

func TestAuditorConfigDefaultsToDisabledWithBoundedWindow(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Roles.Auditor.Enabled || cfg.Roles.Auditor.AllowRevertProposals || cfg.Roles.Auditor.WindowMinutes <= 0 {
		t.Fatalf("default auditor = %#v, want disabled positive-window config", cfg.Roles.Auditor)
	}
}

func TestProjectRoleConfigsMergesAuditorOverride(t *testing.T) {
	enabled, window, allow := true, 15, true
	cfg := Config{Roles: RoleConfigs{Auditor: AuditorRoleConfig{Enabled: false, WindowMinutes: 60}}, Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Auditor: &PartialAuditorRoleConfig{Enabled: &enabled, WindowMinutes: &window, AllowRevertProposals: &allow}}}}}
	got := ProjectRoleConfigs(cfg, "demo").Auditor
	if !got.Enabled || got.WindowMinutes != 15 || !got.AllowRevertProposals {
		t.Fatalf("project auditor = %#v, want enabled 15-minute window", got)
	}
}

func TestAuditorConfigRejectsNonPositiveEnabledWindow(t *testing.T) {
	issues := []ValidationIssue{}
	validateAuditorRoleConfig(AuditorRoleConfig{Enabled: true, WindowMinutes: 0}, "roles.auditor", &issues)
	if len(issues) != 1 || issues[0].Path != "roles.auditor.windowMinutes" {
		t.Fatalf("issues = %#v, want window validation", issues)
	}
}

func TestAuditorAcceptsGatekeeperAutoTrust(t *testing.T) {
	t.Parallel()
	var issues []ValidationIssue
	validateCoreConfig(Config{
		Roles: RoleConfigs{
			Auditor:    AuditorRoleConfig{Enabled: true, WindowMinutes: 60},
			Gatekeeper: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto},
		},
	}, &issues)
	for _, issue := range issues {
		if issue.Path == "roles.auditor.enabled" {
			t.Fatalf("auditor/auto compatibility was rejected: %#v", issues)
		}
	}
}

func TestAuditorAcceptsProjectGatekeeperAutoOverride(t *testing.T) {
	t.Parallel()
	auto := GatekeeperTrustAuto
	var issues []ValidationIssue
	validateCoreConfig(Config{
		Roles: RoleConfigs{Auditor: AuditorRoleConfig{Enabled: true, WindowMinutes: 60}},
		Projects: []ProjectRefConfig{{
			ID: "demo",
			Roles: &PartialRoleConfigs{
				Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto},
				Auditor:    &PartialAuditorRoleConfig{Enabled: ptrBool(true)},
			},
		}},
	}, &issues)
	for _, issue := range issues {
		if issue.Path == "projects[0].roles.gatekeeper.trust" {
			t.Fatalf("project auditor/auto compatibility was rejected: %#v", issues)
		}
	}
}

func ptrBool(v bool) *bool { return &v }

func TestPostMergeDigestAcceptsGatekeeperAutoTrust(t *testing.T) {
	t.Parallel()
	var issues []ValidationIssue
	validateCoreConfig(Config{
		Roles: RoleConfigs{
			Gatekeeper:  GatekeeperRoleConfig{Trust: GatekeeperTrustAuto},
			Coordinator: CoordinatorRoleConfig{PostMergeDigest: &CoordinatorPostMergeDigestConfig{Enabled: true, Schedule: "08:00", Timezone: "UTC", MaxItems: 20}},
		},
	}, &issues)
	for _, issue := range issues {
		if issue.Path == "roles.coordinator.postMergeDigest.enabled" {
			t.Fatalf("post-merge digest/auto compatibility was rejected: %#v", issues)
		}
	}
}

func TestClonePartialRoleConfigsPreservesAuditorFields(t *testing.T) {
	enabled, window, allow := true, 20, true
	original := &PartialRoleConfigs{Auditor: &PartialAuditorRoleConfig{Enabled: &enabled, WindowMinutes: &window, AllowRevertProposals: &allow}}
	cloned := clonePartialRoleConfigs(original)
	if cloned == nil || cloned.Auditor == nil || cloned.Auditor.Enabled == nil || cloned.Auditor.WindowMinutes == nil || cloned.Auditor.AllowRevertProposals == nil || !*cloned.Auditor.Enabled || *cloned.Auditor.WindowMinutes != 20 || !*cloned.Auditor.AllowRevertProposals {
		t.Fatalf("cloned auditor = %#v", cloned)
	}
	*original.Auditor.WindowMinutes = 1
	if *cloned.Auditor.WindowMinutes != 20 {
		t.Fatal("clone aliases auditor window pointer")
	}
}
