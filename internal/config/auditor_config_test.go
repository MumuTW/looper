package config

import "testing"

func TestAuditorConfigDefaultsToDisabledWithBoundedWindow(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Roles.Auditor.Enabled || cfg.Roles.Auditor.WindowMinutes <= 0 {
		t.Fatalf("default auditor = %#v, want disabled positive-window config", cfg.Roles.Auditor)
	}
}

func TestProjectRoleConfigsMergesAuditorOverride(t *testing.T) {
	enabled, window := true, 15
	cfg := Config{Roles: RoleConfigs{Auditor: AuditorRoleConfig{Enabled: false, WindowMinutes: 60}}, Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Auditor: &PartialAuditorRoleConfig{Enabled: &enabled, WindowMinutes: &window}}}}}
	got := ProjectRoleConfigs(cfg, "demo").Auditor
	if !got.Enabled || got.WindowMinutes != 15 {
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

func TestAuditorRejectsGatekeeperAutoTrust(t *testing.T) {
	t.Parallel()
	var issues []ValidationIssue
	validateAuditorGatekeeperCompatibility(Config{
		Roles: RoleConfigs{
			Auditor:    AuditorRoleConfig{Enabled: true, WindowMinutes: 60},
			Gatekeeper: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto},
		},
	}, &issues)
	if len(issues) != 1 || issues[0].Path != "roles.auditor.enabled" {
		t.Fatalf("issues = %#v, want global auditor/auto conflict", issues)
	}
}

func TestAuditorRejectsProjectGatekeeperAutoOverride(t *testing.T) {
	t.Parallel()
	auto := GatekeeperTrustAuto
	var issues []ValidationIssue
	validateAuditorGatekeeperCompatibility(Config{
		Roles: RoleConfigs{Auditor: AuditorRoleConfig{Enabled: true, WindowMinutes: 60}},
		Projects: []ProjectRefConfig{{
			ID: "demo",
			Roles: &PartialRoleConfigs{
				Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto},
				Auditor:    &PartialAuditorRoleConfig{Enabled: ptrBool(true)},
			},
		}},
	}, &issues)
	if len(issues) != 1 || issues[0].Path != "projects[0].roles.gatekeeper.trust" {
		t.Fatalf("issues = %#v, want project gatekeeper/auto conflict", issues)
	}
}

func ptrBool(v bool) *bool { return &v }

func TestClonePartialRoleConfigsPreservesAuditorFields(t *testing.T) {
	enabled, window := true, 20
	original := &PartialRoleConfigs{Auditor: &PartialAuditorRoleConfig{Enabled: &enabled, WindowMinutes: &window}}
	cloned := clonePartialRoleConfigs(original)
	if cloned == nil || cloned.Auditor == nil || cloned.Auditor.Enabled == nil || cloned.Auditor.WindowMinutes == nil || !*cloned.Auditor.Enabled || *cloned.Auditor.WindowMinutes != 20 {
		t.Fatalf("cloned auditor = %#v", cloned)
	}
	*original.Auditor.WindowMinutes = 1
	if *cloned.Auditor.WindowMinutes != 20 {
		t.Fatal("clone aliases auditor window pointer")
	}
}
