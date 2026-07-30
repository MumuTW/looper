package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/gatekeeper"
	"github.com/nexu-io/looper/internal/triager"
)

func TestDiscoveryLanesRegisterTriagerAheadOfPlanner(t *testing.T) {
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
	// Every lane runs on every project: one provider kind means no lane can be
	// unsupported. The per-lane capability predicate that used to encode this
	// is gone, so the registration itself is the whole contract.
	for _, name := range []string{"triager", "coordinator", config.CodingRoleFixer, config.RoleGatekeeper} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("lane %q missing from registration: %#v", name, positions)
		}
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

// Projects are discovered concurrently through one lane, so every project shares
// the tick-wide budget. This asserts the lane still hands the same budget to each
// caller under concurrency; the reservation invariant itself is proven under real
// contention by TestDecisionBudgetReserveNeverExceedsLimitUnderContention.
func TestTriagerLaneDecisionBudgetIsSharedAcrossConcurrentProjects(t *testing.T) {
	t.Parallel()
	const projects = 4
	runner := &budgetTriager{}
	lane := triagerLane(defaultSchedulerTickInput{Triager: runner})

	var wg sync.WaitGroup
	wg.Add(projects)
	for i := 0; i < projects; i++ {
		go func() {
			defer wg.Done()
			if _, err := lane.Discover(context.Background(), "project", "acme/repo", nil); err != nil {
				t.Errorf("Discover() error = %v", err)
			}
		}()
	}
	wg.Wait()

	if got := int(runner.reserved.Load()); got != triager.DefaultDecisionLimit {
		t.Fatalf("successful reservations = %d, want %d (lane handed out more than one tick-wide budget)", got, triager.DefaultDecisionLimit)
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

type budgetTriager struct {
	mu       sync.Mutex
	budgets  []int
	reserved atomic.Int64
	onCall   func()
}

func (f *budgetTriager) DiscoverIssues(_ context.Context, input triager.DiscoveryInput) (triager.DiscoveryResult, error) {
	if f.onCall != nil {
		f.onCall()
	}
	if input.DecisionBudget == nil {
		f.mu.Lock()
		f.budgets = append(f.budgets, -1)
		f.mu.Unlock()
		return triager.DiscoveryResult{}, nil
	}
	remaining := input.DecisionBudget.Remaining()
	if input.DecisionBudget.Reserve() {
		f.reserved.Add(1)
	}
	f.mu.Lock()
	f.budgets = append(f.budgets, remaining)
	f.mu.Unlock()
	return triager.DiscoveryResult{}, nil
}

// Gatekeeper lists DefaultDiscoveryPullRequestLimit open PRs with no filters, so a
// smaller shared page always fails the snapshot's truncation check: the lane
// discards the page it was handed and pays a second raw query. Sizing the page to
// the largest limit a lane requests is what makes the shared page usable.
func TestProjectDiscoverySnapshotOptionsCoversGatekeeperPullRequestLimit(t *testing.T) {
	t.Parallel()

	withoutGatekeeper := projectDiscoverySnapshotOptions(defaultSchedulerTickInput{}, "looper")
	if withoutGatekeeper.PullRequestLimit >= gatekeeper.DefaultDiscoveryPullRequestLimit {
		t.Fatalf("PullRequestLimit without gatekeeper = %d, want below the gatekeeper limit %d",
			withoutGatekeeper.PullRequestLimit, gatekeeper.DefaultDiscoveryPullRequestLimit)
	}

	withGatekeeper := projectDiscoverySnapshotOptions(defaultSchedulerTickInput{Gatekeeper: &fakeGatekeeperScheduler{}}, "looper")
	if withGatekeeper.PullRequestLimit < gatekeeper.DefaultDiscoveryPullRequestLimit {
		t.Fatalf("PullRequestLimit with gatekeeper = %d, want at least %d so the page is not discarded",
			withGatekeeper.PullRequestLimit, gatekeeper.DefaultDiscoveryPullRequestLimit)
	}
}
