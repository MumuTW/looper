package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
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
	if byName["triager"].Supported(config.ProviderKindGitHub) != true ||
		byName["triager"].Supported(config.ProviderKindForgejo) != false {
		t.Fatal("triager must accept GitHub issues only")
	}
	if !byName[config.CodingRoleFixer].Supported(config.ProviderKindForgejo) {
		t.Fatal("triager registration changed fixer Forgejo discovery support")
	}
}
