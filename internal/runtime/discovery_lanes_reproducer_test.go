package runtime

import (
	"context"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/reproducer"
)

// The ordering is the design: reproducing after Planner would mean the planning
// cost is already paid before anyone knows the bug can be made to fail.
func TestReproducerLaneRunsBetweenTriagerAndPlanner(t *testing.T) {
	t.Parallel()
	lanes := discoveryLanes(defaultSchedulerTickInput{})
	positions := make(map[string]int, len(lanes))
	byName := make(map[string]discoveryLane, len(lanes))
	for index, lane := range lanes {
		positions[lane.Name] = index
		byName[lane.Name] = lane
	}
	if positions["triager"] >= positions["reproducer"] || positions["reproducer"] >= positions[config.CodingRolePlanner] {
		t.Fatalf("lane positions = %#v, want triager < reproducer < planner", positions)
	}
	if config.PriorityReproducer <= config.PriorityTriager || config.PriorityReproducer >= config.PriorityPlanner {
		t.Fatalf("PriorityReproducer = %d, want it strictly between %d and %d", config.PriorityReproducer, config.PriorityTriager, config.PriorityPlanner)
	}
	githubCapabilities, _ := forge.StaticCapabilities(forge.ProviderKindGitHub)
	forgejoCapabilities, _ := forge.StaticCapabilities(forge.ProviderKindForgejo)
	if !byName["reproducer"].Supported(githubCapabilities) || byName["reproducer"].Supported(forgejoCapabilities) {
		t.Fatal("reproducer must share the GitHub-issue authority predicate with triager")
	}
}

// Absent configuration must not enable the Role: it changes what "done" means
// for every bug in a project, so it is opt-in.
func TestReproducerLaneIsDisabledWithoutARunnerOrAFlag(t *testing.T) {
	t.Parallel()
	lane := reproducerLane(defaultSchedulerTickInput{})
	if lane.Present {
		t.Fatal("reproducer lane reports a runner that was never wired")
	}
	if lane.Enabled("project_1") {
		t.Fatal("reproducer lane is enabled with no ReproducerEnabled predicate")
	}
}

// `roles.coding.planner.priority` is an authorable value, and the shipped sort
// honours it literally. A priority below Reproducer's would run Planner first,
// where it legitimately finds no triage report and takes the documented
// no-report path — permanently defeating the pre-Planning guarantee through an
// ordinary configuration setting rather than a bug.
func TestConfiguredPriorityCannotRunPlannerBeforeReproducer(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Roles: config.RoleConfigs{Coding: map[string]config.CodingRoleConfig{
		config.CodingRolePlanner:  {Priority: 1},
		config.CodingRoleReviewer: {Priority: config.PriorityReviewer},
	}}}
	lanes := discoveryLanes(defaultSchedulerTickInput{Config: cfg, Reproducer: stubReproducerScheduler{}})

	positions := make(map[string]int, len(lanes))
	for index, lane := range lanes {
		positions[lane.Name] = index
	}
	if positions["triager"] >= positions[config.CodingRolePlanner] || positions["reproducer"] >= positions[config.CodingRolePlanner] {
		t.Fatalf("lane positions = %#v, want the internal lanes to stay ahead of an under-prioritized planner", positions)
	}
	// Relative order among the coding roles themselves is preserved: the floor
	// raises a lane, it does not reshuffle the ones already above it.
	if positions[config.CodingRolePlanner] >= positions[config.CodingRoleReviewer] {
		t.Fatalf("lane positions = %#v, want planner still ahead of reviewer", positions)
	}
}

// Without a Reproducer runner there is no guarantee to protect, so an authored
// priority is honoured exactly as before.
func TestConfiguredPriorityIsUntouchedWithoutReproducer(t *testing.T) {
	t.Parallel()
	if got := orderAfterInternalLanes(1, false); got != 1 {
		t.Fatalf("orderAfterInternalLanes(1, false) = %d, want the authored priority untouched", got)
	}
	if got := orderAfterInternalLanes(config.PriorityPlanner, true); got != config.PriorityPlanner {
		t.Fatalf("orderAfterInternalLanes(%d, true) = %d, want a priority already past the floor untouched", config.PriorityPlanner, got)
	}
}

// stubReproducerScheduler exists only to make the lane Present; the ordering
// floor is a function of the runner existing, not of what it does.
type stubReproducerScheduler struct{}

func (stubReproducerScheduler) DiscoverIssues(context.Context, reproducer.DiscoveryInput) (reproducer.DiscoveryResult, error) {
	return reproducer.DiscoveryResult{}, nil
}
