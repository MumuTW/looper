package runtime

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/gatekeeper"
)

func TestCodingDiscoveryLanesRegistersSourceBasedGatekeeper(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	runner := &fakeGatekeeperScheduler{}
	lanes := discoveryLanes(defaultSchedulerTickInput{Config: &cfg, Gatekeeper: runner})

	var found *discoveryLane
	for i := range lanes {
		if lanes[i].Name == config.RoleGatekeeper {
			found = &lanes[i]
			break
		}
	}
	if found == nil {
		t.Fatal("gatekeeper discovery lane is not registered")
	}
	if !found.Present || !found.Enabled("project_1") || found.Priority != config.PriorityGatekeeper {
		t.Fatalf("gatekeeper lane = %#v, want present enabled policy lane", *found)
	}
	if _, err := found.Discover(context.Background(), "project_1", "acme/looper", nil); err != nil {
		t.Fatalf("gatekeeper Discover() error = %v", err)
	}
	if runner.discoveryInput.ProjectID != "project_1" || runner.discoveryInput.Repo != "acme/looper" {
		t.Fatalf("gatekeeper discovery input = %#v", runner.discoveryInput)
	}
}

type fakeGatekeeperScheduler struct {
	discoveryInput  gatekeeper.DiscoveryInput
	retirementCalls int
}

func (f *fakeGatekeeperScheduler) DiscoverPullRequests(_ context.Context, input gatekeeper.DiscoveryInput) (gatekeeper.DiscoveryResult, error) {
	f.discoveryInput = input
	return gatekeeper.DiscoveryResult{}, nil
}

func (*fakeGatekeeperScheduler) EvaluatePullRequest(context.Context, gatekeeper.EvaluationInput) (gatekeeper.Report, error) {
	return gatekeeper.Report{}, nil
}

func (f *fakeGatekeeperScheduler) ReconcileLegacyVerdictComments(context.Context) error {
	f.retirementCalls++
	return nil
}
