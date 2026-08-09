package runtime

import (
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestGatekeeperRoleConfigForProjectMatchesExactID(t *testing.T) {
	t.Parallel()
	merge := config.MergeStrategyMerge
	rebase := config.MergeStrategyRebase
	cfg := config.Config{
		Roles: config.RoleConfigs{Gatekeeper: config.GatekeeperRoleConfig{Strategy: config.MergeStrategySquash}},
		Projects: []config.ProjectRefConfig{
			{ID: "Project", Roles: &config.PartialRoleConfigs{Gatekeeper: &config.PartialGatekeeperRoleConfig{Strategy: &merge}}},
			{ID: "project", Roles: &config.PartialRoleConfigs{Gatekeeper: &config.PartialGatekeeperRoleConfig{Strategy: &rebase}}},
		},
	}
	if got := gatekeeperRoleConfigForProject(cfg, "project"); got.Strategy != rebase {
		t.Fatalf("strategy = %q, want exact lowercase project override %q", got.Strategy, rebase)
	}
}

func TestGatekeeperRoleConfigForProjectDoesNotTrimDistinctIDs(t *testing.T) {
	t.Parallel()
	merge := config.MergeStrategyMerge
	rebase := config.MergeStrategyRebase
	cfg := config.Config{
		Roles: config.RoleConfigs{Gatekeeper: config.GatekeeperRoleConfig{Strategy: config.MergeStrategySquash}},
		Projects: []config.ProjectRefConfig{
			{ID: "demo", Roles: &config.PartialRoleConfigs{Gatekeeper: &config.PartialGatekeeperRoleConfig{Strategy: &merge}}},
			{ID: " demo ", Roles: &config.PartialRoleConfigs{Gatekeeper: &config.PartialGatekeeperRoleConfig{Strategy: &rebase}}},
		},
	}
	if got := gatekeeperRoleConfigForProject(cfg, " demo "); got.Strategy != rebase {
		t.Fatalf("strategy = %q, want padded project override %q", got.Strategy, rebase)
	}
	if got := gatekeeperRoleConfigForProject(cfg, "demo"); got.Strategy != merge {
		t.Fatalf("strategy = %q, want unpadded project override %q", got.Strategy, merge)
	}
}
