package runtime

import (
	"context"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/fixer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

// discoveryLane is one coding role's discovery pass over one project.
//
// Every role's lane used to be its own hand-written block in the scheduler
// tick, differing only in these five fields. Expressing them as data is what
// makes a role a configuration concern: adding one means registering a lane,
// not editing the tick.
type discoveryLane struct {
	// Name is the role name; it also forms the lane label in logs and the
	// claim-phase label, so both stay in sync with the role automatically.
	Name string

	// Present reports whether the runner for this role exists at all. A
	// missing runner is distinct from a disabled one: only the latter is
	// worth telling the operator about.
	Present bool

	// Enabled is evaluated per project, because coordinator admission is
	// per-project while the other roles use a single daemon-wide flag.
	Enabled func(projectID string) bool

	// Supported guards providers that cannot serve this lane (a task source
	// without pull requests cannot feed a PR-source role). Nil means the
	// lane runs on every provider.
	Supported func(kind config.ProviderKind) bool

	// LogWhenDisabled keeps the per-role "auto-discovery disabled" debug
	// line. Coordinator opts out: it is disabled per project rather than
	// daemon-wide, so logging it would emit a line for every project on
	// every tick in the common case where coordination is simply off.
	LogWhenDisabled bool

	// Discover runs the pass. Returning the queue items lets the tick track
	// runnable discoveries uniformly; a lane that enqueues nothing directly
	// (coordinator) returns nil.
	Discover func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error)
}

// codingDiscoveryLanes builds the ordered lane list for one tick.
//
// Order is load-bearing: a claim phase runs between consecutive lanes, so
// earlier roles get first call on the available run slots. Planner leads
// because newly planned work should be claimable in the same tick that
// discovered it.
func codingDiscoveryLanes(input defaultSchedulerTickInput) []discoveryLane {
	lanes := make([]discoveryLane, 0, 5)

	lanes = append(lanes, discoveryLane{
		Name:            config.CodingRolePlanner,
		LogWhenDisabled: true,
		Present:         input.Planner != nil,
		Enabled:         func(string) bool { return discoveryEnabled(input.PlannerDiscoveryEnabled) },
		Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
			result, err := input.Planner.DiscoverIssues(ctx, planner.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
			return result.QueueItems, err
		},
	})

	lanes = append(lanes, discoveryLane{
		Name:      "coordinator",
		Present:   input.Coordinator != nil,
		Enabled:   func(projectID string) bool { return coordinatorEnabledForProject(input, projectID) },
		Supported: providerHasGitHubPullRequests,
		Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
			_, err := input.Coordinator.DiscoverIssues(ctx, coordinator.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
			return nil, err
		},
	})

	lanes = append(lanes, discoveryLane{
		Name:            config.CodingRoleReviewer,
		LogWhenDisabled: true,
		Present:         input.Reviewer != nil,
		Enabled:         func(string) bool { return discoveryEnabled(input.ReviewerDiscoveryEnabled) },
		Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
			result, err := input.Reviewer.DiscoverPullRequests(ctx, reviewer.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
			return result.QueueItems, err
		},
	})

	lanes = append(lanes, discoveryLane{
		Name:            config.CodingRoleFixer,
		LogWhenDisabled: true,
		Present:         input.Fixer != nil,
		Enabled:         func(string) bool { return discoveryEnabled(input.FixerDiscoveryEnabled) },
		Supported:       providerSupportsFixerDiscovery,
		Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
			result, err := input.Fixer.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
			return result.QueueItems, err
		},
	})

	workerDiscoverer, workerCanDiscover := input.Worker.(workerIssueDiscoveryScheduler)
	lanes = append(lanes, discoveryLane{
		Name:            config.CodingRoleWorker,
		LogWhenDisabled: true,
		Present:         input.Worker != nil && workerCanDiscover,
		Enabled:         func(string) bool { return discoveryEnabled(input.WorkerDiscoveryEnabled) },
		Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
			result, err := workerDiscoverer.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
			return result.QueueItems, err
		},
	})

	return lanes
}

// laneLabel is the lane name used in logs and wrapped errors. Preserved
// verbatim from the hand-written blocks so operators' existing log greps and
// the scheduler's slow-lane warnings keep matching.
func (l discoveryLane) laneLabel() string {
	if l.Name == config.CodingRoleWorker {
		return "worker issue discovery"
	}
	return l.Name + " discovery"
}

// claimPhaseLabel names the claim phase that runs after this lane.
func (l discoveryLane) claimPhaseLabel() string {
	return "post_" + l.Name + "_discovery"
}
