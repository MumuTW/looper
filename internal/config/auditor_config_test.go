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

func ptrBool(v bool) *bool { return &v }

func TestAuditorAllowsGatekeeperAutoTrust(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Auditor = AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	cfg.Roles.Gatekeeper.Trust = GatekeeperTrustAuto
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want auditor and merge-outcome auto to coexist", err)
	}
}

func TestProjectAuditorAllowsGatekeeperAutoOverride(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	auto := GatekeeperTrustAuto
	cfg.Projects = []ProjectRefConfig{{
		ID: "demo", Name: "Demo", RepoPath: t.TempDir(),
		Roles: &PartialRoleConfigs{
			Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto},
			Auditor:    &PartialAuditorRoleConfig{Enabled: ptrBool(true)},
		},
	}}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want project auditor and gatekeeper auto override to coexist", err)
	}
}

func TestPostMergeDigestAllowsGatekeeperAutoTrust(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Gatekeeper.Trust = GatekeeperTrustAuto
	cfg.Roles.Coordinator.PostMergeDigest = &CoordinatorPostMergeDigestConfig{Enabled: true, Schedule: "08:00", Timezone: "UTC", MaxItems: 20}
	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v, want post-merge digest and merge-outcome auto to coexist", err)
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
