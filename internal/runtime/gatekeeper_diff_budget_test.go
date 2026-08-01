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

// Project IDs are matched by exact equality, the same semantics as
// runtimeProjectBinding and config.ProjectRoleConfigs. A case-insensitive or
// trimmed comparison can stop on the wrong project and apply its budget, so an
// ID differing only by case or surrounding whitespace must not match.
func TestGatekeeperDiffBudgetForProjectMatchesProjectIDExactly(t *testing.T) {
	t.Parallel()
	global := &config.GatekeeperDiffBudget{MaxChangedFiles: 20, MaxDeletions: 500}
	projectDeletions := 100
	cfg := config.Config{
		Roles: config.RoleConfigs{Gatekeeper: config.GatekeeperRoleConfig{DiffBudget: global}},
		Projects: []config.ProjectRefConfig{{
			ID: "looper",
			Roles: &config.PartialRoleConfigs{Gatekeeper: &config.PartialGatekeeperRoleConfig{
				DiffBudget: &config.PartialGatekeeperDiffBudget{MaxDeletions: &projectDeletions},
			}},
		}},
	}

	for _, query := range []string{"Looper", "LOOPER", " looper ", "looper "} {
		got := gatekeeperDiffBudgetForProject(cfg, query)
		if got.MaxDeletions != 500 {
			t.Fatalf("gatekeeperDiffBudgetForProject(%q) = %#v, want global deletions=500 (no loose match)", query, got)
		}
	}

	exact := gatekeeperDiffBudgetForProject(cfg, "looper")
	if exact.MaxDeletions != 100 {
		t.Fatalf("gatekeeperDiffBudgetForProject(\"looper\") = %#v, want project deletions=100", exact)
	}
}

// Trust overrides use the same exact project-ID lookup, so a case- or
// whitespace-differing ID must not pick up another project's trust level.
func TestGatekeeperTrustForProjectMatchesProjectIDExactly(t *testing.T) {
	t.Parallel()
	auto := config.GatekeeperTrustAuto
	cfg := config.Config{
		Projects: []config.ProjectRefConfig{{
			ID:    "looper",
			Roles: &config.PartialRoleConfigs{Gatekeeper: &config.PartialGatekeeperRoleConfig{Trust: &auto}},
		}},
	}

	for _, query := range []string{"Looper", "LOOPER", " looper ", "looper "} {
		if got := gatekeeperTrustForProject(cfg, query); got != config.GatekeeperTrustObserve {
			t.Fatalf("gatekeeperTrustForProject(%q) = %q, want observe (no loose match)", query, got)
		}
	}

	if got := gatekeeperTrustForProject(cfg, "looper"); got != config.GatekeeperTrustAuto {
		t.Fatalf("gatekeeperTrustForProject(\"looper\") = %q, want auto", got)
	}
}
