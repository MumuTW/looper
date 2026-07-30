package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
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
