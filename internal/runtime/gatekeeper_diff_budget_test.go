package runtime

import (
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestGatekeeperDiffBudgetForProjectOverlaysGlobalBounds(t *testing.T) {
	t.Parallel()
	global := &config.GatekeeperDiffBudget{MaxChangedFiles: 20, MaxDeletions: 500}
	projectDeletions := 100
	cfg := config.Config{
		Roles: config.RoleConfigs{Gatekeeper: config.GatekeeperRoleConfig{DiffBudget: global}},
		Projects: []config.ProjectRefConfig{{
			ID: "demo",
			Roles: &config.PartialRoleConfigs{Gatekeeper: &config.PartialGatekeeperRoleConfig{
				DiffBudget: &config.PartialGatekeeperDiffBudget{MaxDeletions: &projectDeletions},
			}},
		}},
	}

	got := gatekeeperDiffBudgetForProject(cfg, "demo")
	if got.MaxChangedFiles != 20 || got.MaxDeletions != 100 {
		t.Fatalf("budget = %#v, want files=20 deletions=100", got)
	}
	if global.MaxDeletions != 500 {
		t.Fatal("project override mutated global budget")
	}
}
