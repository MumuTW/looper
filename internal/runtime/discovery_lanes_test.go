package runtime

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/triager"
)

func TestDiscoveryLanesRegisterTriagerAheadOfPlannerWithoutChangingFixerSupport(t *testing.T) {
	t.Parallel()
	lanes := discoveryLanes(defaultSchedulerTickInput{})
	byName := make(map[string]discoveryLane, len(lanes))
	positions := make(map[string]int, len(lanes))
	for index, lane := range lanes {
		byName[lane.Name] = lane
		positions[lane.Name] = index
	}

	if positions["triager"] >= positions[config.CodingRolePlanner] {
		t.Fatalf("lane positions = %#v, want triager before planner", positions)
	}
	githubCapabilities, _ := forge.StaticCapabilities(forge.ProviderKindGitHub)
	forgejoCapabilities, _ := forge.StaticCapabilities(forge.ProviderKindForgejo)
	if byName["triager"].Supported(githubCapabilities) != true ||
		byName["triager"].Supported(forgejoCapabilities) != false {
		t.Fatal("triager must accept GitHub issues only")
	}
	if !byName[config.CodingRoleFixer].Supported(forgejoCapabilities) {
		t.Fatal("triager registration changed fixer Forgejo discovery support")
	}
}

func TestTriagerLaneSharesOneDecisionBudgetAcrossProjects(t *testing.T) {
	t.Parallel()
	runner := &budgetTriager{}
	lane := triagerLane(defaultSchedulerTickInput{Triager: runner})
	if _, err := lane.Discover(context.Background(), "project_1", "acme/one", nil); err != nil {
		t.Fatalf("first Discover() error = %v", err)
	}
	if _, err := lane.Discover(context.Background(), "project_2", "acme/two", nil); err != nil {
		t.Fatalf("second Discover() error = %v", err)
	}
	if len(runner.budgets) != 2 || runner.budgets[0] != 1 || runner.budgets[1] != 0 {
		t.Fatalf("decision budgets = %v, want [1 0]", runner.budgets)
	}
}

func TestDiscoveryLanesOrderFromCanonicalRegistryPriority(t *testing.T) {
	t.Parallel()
	priority := 1
	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{Roles: &config.PartialRoleConfigs{Coding: map[string]config.PartialCodingRoleConfig{
		config.CodingRoleWorker: {Priority: &priority},
	}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	lanes := discoveryLanes(defaultSchedulerTickInput{Config: &cfg})
	for _, lane := range lanes {
		if lane.Name == config.CodingRoleWorker {
			if lane.Priority != 1 {
				t.Fatalf("worker lane priority = %d, want canonical registry priority", lane.Priority)
			}
			return
		}
	}
	t.Fatal("worker discovery lane missing")
}

type budgetTriager struct{ budgets []int }

func (f *budgetTriager) DiscoverIssues(_ context.Context, input triager.DiscoveryInput) (triager.DiscoveryResult, error) {
	if input.DecisionBudget == nil {
		f.budgets = append(f.budgets, -1)
		return triager.DiscoveryResult{}, nil
	}
	f.budgets = append(f.budgets, *input.DecisionBudget)
	if *input.DecisionBudget > 0 {
		*input.DecisionBudget--
	}
	return triager.DiscoveryResult{}, nil
}
