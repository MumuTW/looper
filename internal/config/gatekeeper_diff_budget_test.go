package config

import "testing"

func TestGatekeeperDiffBudgetRejectsNegativeBounds(t *testing.T) {
	t.Parallel()
	issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{
		DiffBudget: &GatekeeperDiffBudget{MaxChangedFiles: -1, MaxDeletions: -2},
	}}})
	if len(issues) != 2 {
		t.Fatalf("issues = %+v, want one issue per negative bound", issues)
	}
	if issues[0].Path != "roles.gatekeeper.diffBudget.maxChangedFiles" || issues[1].Path != "roles.gatekeeper.diffBudget.maxDeletions" {
		t.Fatalf("issues = %+v, want deterministic diff-budget paths", issues)
	}
}

func TestMergeAndCloneGatekeeperDiffBudget(t *testing.T) {
	t.Parallel()
	maxFiles := 20
	partial := PartialConfig{Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{
		DiffBudget: &PartialGatekeeperDiffBudget{MaxChangedFiles: &maxFiles},
	}}}
	cloned := clonePartialConfig(partial)
	if cloned.Roles == nil || cloned.Roles.Gatekeeper == nil || cloned.Roles.Gatekeeper.DiffBudget == nil || cloned.Roles.Gatekeeper.DiffBudget.MaxChangedFiles == nil {
		t.Fatal("clone dropped gatekeeper diff budget")
	}
	*cloned.Roles.Gatekeeper.DiffBudget.MaxChangedFiles = 30
	if maxFiles != 20 {
		t.Fatalf("clone aliases maxChangedFiles, original = %d", maxFiles)
	}

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	mergeConfig(&cfg, partial)
	if cfg.Roles.Gatekeeper.DiffBudget == nil || cfg.Roles.Gatekeeper.DiffBudget.MaxChangedFiles != 20 {
		t.Fatalf("merged budget = %#v, want maxChangedFiles=20", cfg.Roles.Gatekeeper.DiffBudget)
	}
}

func TestProjectGatekeeperDiffBudgetOverridesGlobalBounds(t *testing.T) {
	t.Parallel()
	globalFiles, globalDeletions := 20, 500
	projectDeletions := 100
	cfg := Config{
		Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{DiffBudget: &GatekeeperDiffBudget{MaxChangedFiles: globalFiles, MaxDeletions: globalDeletions}}},
		Projects: []ProjectRefConfig{{
			ID:    "demo",
			Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{DiffBudget: &PartialGatekeeperDiffBudget{MaxDeletions: &projectDeletions}}},
		}},
	}
	roles := ProjectRoleConfigs(cfg, "demo")
	if roles.Gatekeeper.DiffBudget == nil || roles.Gatekeeper.DiffBudget.MaxChangedFiles != globalFiles || roles.Gatekeeper.DiffBudget.MaxDeletions != projectDeletions {
		t.Fatalf("project budget = %#v, want files=%d deletions=%d", roles.Gatekeeper.DiffBudget, globalFiles, projectDeletions)
	}
	if cfg.Roles.Gatekeeper.DiffBudget.MaxDeletions != globalDeletions {
		t.Fatal("project override mutated global budget")
	}
}
