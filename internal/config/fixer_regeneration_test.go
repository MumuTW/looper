package config

import "testing"

func TestProjectRoleConfigsMergesFixerRegenerationOverride(t *testing.T) {
	deleteBranch := false
	cfg := Config{
		Roles: RoleConfigs{Fixer: FixerRoleConfig{AutoDiscovery: true}},
		Projects: []ProjectRefConfig{{
			ID:    "demo",
			Roles: &PartialRoleConfigs{Fixer: &PartialFixerRoleConfig{Regeneration: &PartialFixerRegenerationConfig{DeleteBranch: &deleteBranch}}},
		}},
	}
	got := ProjectRoleConfigs(cfg, "demo").Fixer.Regeneration
	if got == nil || got.DeleteBranch {
		t.Fatalf("effective fixer regeneration = %#v, want explicit deleteBranch=false", got)
	}
	if cfg.Roles.Fixer.Regeneration != nil {
		t.Fatal("project overlay mutated global fixer regeneration")
	}
}

func TestProjectRoleConfigsEmptyRegenerationSectionKeepsSafeDeleteDefault(t *testing.T) {
	cfg := Config{
		Roles: RoleConfigs{Fixer: FixerRoleConfig{}},
		Projects: []ProjectRefConfig{{
			ID:    "demo",
			Roles: &PartialRoleConfigs{Fixer: &PartialFixerRoleConfig{Regeneration: &PartialFixerRegenerationConfig{}}},
		}},
	}
	got := ProjectRoleConfigs(cfg, "demo").Fixer.Regeneration
	if got == nil || !got.DeleteBranch {
		t.Fatalf("effective fixer regeneration = %#v, want deleteBranch=true for an empty section", got)
	}
}

func TestFixerRegenerationNilUsesSafeDeleteDefault(t *testing.T) {
	roles := ProjectRoleConfigs(Config{Roles: RoleConfigs{Fixer: FixerRoleConfig{}}}, "missing")
	if roles.Fixer.Regeneration != nil {
		t.Fatalf("nil regeneration = %#v, want nil so runtime safe default remains authoritative", roles.Fixer.Regeneration)
	}
}
