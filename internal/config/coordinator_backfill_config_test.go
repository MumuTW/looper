package config

import "testing"

func TestCoordinatorBackfillDefaultsDisabled(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if cfg.Roles.Coordinator.BackfillEnabled {
		t.Fatal("coordinator backfill defaults enabled; want explicit opt-in")
	}
}

func TestCoordinatorBackfillProjectOverrideAndClone(t *testing.T) {
	enabled := true
	base := Config{Roles: RoleConfigs{Coordinator: CoordinatorRoleConfig{BackfillEnabled: false}}, Projects: []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Coordinator: &PartialCoordinatorRoleConfig{BackfillEnabled: &enabled}}}}}
	got := ProjectRoleConfigs(base, "demo").Coordinator.BackfillEnabled
	if !got {
		t.Fatal("project backfill override was not applied")
	}
	original := &PartialRoleConfigs{Coordinator: &PartialCoordinatorRoleConfig{BackfillEnabled: &enabled}}
	cloned := clonePartialRoleConfigs(original)
	if cloned == nil || cloned.Coordinator == nil || cloned.Coordinator.BackfillEnabled == nil {
		t.Fatal("clone lost coordinator backfill override")
	}
	*original.Coordinator.BackfillEnabled = false
	if !*cloned.Coordinator.BackfillEnabled {
		t.Fatal("clone aliases coordinator backfill override pointer")
	}
}
