package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileMergesCoordinatorConflictPolicyOverride(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "config.json")
	if err := os.WriteFile(path, []byte(`{"roles":{"coordinator":{"conflictPolicy":{"maxRepairs":4}}}}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	loaded, err := LoadFile(LoadFileOptions{CWD: cwd, ConfigPath: path, LookupEnv: emptyEnvLookup, LookPath: fakeLookPath(map[string]string{"git": "/git", "gh": "/gh", "osascript": "/osascript"})})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Config.Roles.Coordinator.ConflictPolicy == nil || loaded.Config.Roles.Coordinator.ConflictPolicy.MaxRepairs != 4 {
		t.Fatalf("conflict policy = %#v, want maxRepairs=4", loaded.Config.Roles.Coordinator.ConflictPolicy)
	}
}

func TestValidateRejectsNonPositiveCoordinatorConflictRepairs(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.ConflictPolicy = &CoordinatorConflictPolicyConfig{MaxRepairs: 0}
	validationErr, ok := Validate(cfg).(*ConfigValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T, want *ConfigValidationError", Validate(cfg))
	}
	assertValidationIssue(t, validationErr, "roles.coordinator.conflictPolicy.maxRepairs", "must be a positive integer")
}

func TestProjectRoleConfigsClonesCoordinatorConflictPolicyBeforeOverlay(t *testing.T) {
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Coordinator.ConflictPolicy = &CoordinatorConflictPolicyConfig{MaxRepairs: 2}
	projectMax := 5
	cfg.Projects = []ProjectRefConfig{{ID: "project-override", Roles: &PartialRoleConfigs{Coordinator: &PartialCoordinatorRoleConfig{ConflictPolicy: &PartialCoordinatorConflictPolicyConfig{MaxRepairs: &projectMax}}}}}

	projectRoles := ProjectRoleConfigs(cfg, "project-override")
	if projectRoles.Coordinator.ConflictPolicy == nil || projectRoles.Coordinator.ConflictPolicy.MaxRepairs != projectMax {
		t.Fatalf("project conflict policy = %#v, want maxRepairs=%d", projectRoles.Coordinator.ConflictPolicy, projectMax)
	}
	globalRoles := ProjectRoleConfigs(cfg, "other-project")
	if globalRoles.Coordinator.ConflictPolicy == nil || globalRoles.Coordinator.ConflictPolicy.MaxRepairs != 2 {
		t.Fatalf("global conflict policy = %#v, want untouched maxRepairs=2", globalRoles.Coordinator.ConflictPolicy)
	}
}
