package runtime

import (
	"context"
	"sort"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/gatekeeper"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/triager"
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

	// Priority orders this lane within the tick; see CodingRoleConfig.
	Priority int

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
	Supported func(capabilities forge.Capabilities) bool

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

// roleDiscoverers maps a role name to the runner-backed half of its lane:
// whether a runner exists, what it calls, and which providers can serve it.
// This is the part configuration cannot supply — a role named in TOML with no
// runner compiled in has nothing to run.
func roleDiscoverers(input defaultSchedulerTickInput) map[string]discoveryLane {
	workerDiscoverer, workerCanDiscover := input.Worker.(workerIssueDiscoveryScheduler)

	return map[string]discoveryLane{
		config.CodingRolePlanner: {
			Enabled: func(string) bool { return discoveryEnabled(input.PlannerDiscoveryEnabled) },
			Present: input.Planner != nil,
			Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
				result, err := input.Planner.DiscoverIssues(ctx, planner.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
				return result.QueueItems, err
			},
		},
		config.CodingRoleReviewer: {
			Enabled: func(string) bool { return discoveryEnabled(input.ReviewerDiscoveryEnabled) },
			Present: input.Reviewer != nil,
			Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
				result, err := input.Reviewer.DiscoverPullRequests(ctx, reviewer.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
				return result.QueueItems, err
			},
		},
		config.CodingRoleFixer: {
			Enabled: func(string) bool { return discoveryEnabled(input.FixerDiscoveryEnabled) },
			Present: input.Fixer != nil,
			Supported: func(capabilities forge.Capabilities) bool {
				return capabilities.PullRequests || capabilities.GitHubPullRequests
			},
			Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
				result, err := input.Fixer.DiscoverPullRequests(ctx, fixer.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
				return result.QueueItems, err
			},
		},
		config.CodingRoleWorker: {
			Enabled: func(string) bool { return discoveryEnabled(input.WorkerDiscoveryEnabled) },
			Present: input.Worker != nil && workerCanDiscover,
			Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
				result, err := workerDiscoverer.DiscoverIssues(ctx, worker.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
				return result.QueueItems, err
			},
		},
		config.RoleGatekeeper: {
			Enabled:   func(string) bool { return true },
			Present:   input.Gatekeeper != nil,
			Supported: func(capabilities forge.Capabilities) bool { return capabilities.GitHubPullRequests },
			Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
				_, err := input.Gatekeeper.DiscoverPullRequests(ctx, gatekeeper.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
				return nil, err
			},
		},
	}
}

// supportsGitHubIssueDiscovery is the single authority for lanes that discover
// GitHub issues through the GitHub gateway (triager and coordinator). A
// provider that does not serve issues through that gateway must not feed them.
//
// Both lanes used to hand-write this predicate independently, and each picked a
// different wrong flag (coordinator: GitHubPullRequests; triager:
// GitHubCLIPullRequestCreation), which is the repeated one-line fix this
// centralization replaces. The GitHub-issue authority now lives in one place so
// the lanes cannot drift apart again.
func supportsGitHubIssueDiscovery(capabilities forge.Capabilities) bool {
	return capabilities.GitHubIssues
}

// coordinatorLane is built separately because coordination is not a coding
// role: it has no agent, no discovery config, and is enabled per project.
func coordinatorLane(input defaultSchedulerTickInput) discoveryLane {
	return discoveryLane{
		Name:      "coordinator",
		Priority:  config.PriorityCoordinator,
		Present:   input.Coordinator != nil,
		Enabled:   func(projectID string) bool { return coordinatorEnabledForProject(input, projectID) },
		Supported: supportsGitHubIssueDiscovery,
		Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
			_, err := input.Coordinator.DiscoverIssues(ctx, coordinator.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot})
			return nil, err
		},
	}
}

// triagerLane is an internal issue-source role. Its persisted report is the
// routing authority; the lane does not need a label-gated CodingRoleConfig.
func triagerLane(input defaultSchedulerTickInput) discoveryLane {
	// One budget per tick, shared by every project this lane discovers. Projects
	// are discovered concurrently, so the budget synchronizes its own reservation.
	decisionBudget := triager.NewDecisionBudget(triager.DefaultDecisionLimit)
	return discoveryLane{
		Name:     "triager",
		Priority: config.PriorityTriager,
		Present:  input.Triager != nil,
		Enabled: func(projectID string) bool {
			return input.TriagerEnabled != nil && input.TriagerEnabled(projectID)
		},
		Supported: supportsGitHubIssueDiscovery,
		Discover: func(ctx context.Context, projectID, repo string, snapshot *githubinfra.DiscoverySnapshot) ([]storage.QueueItemRecord, error) {
			result, err := input.Triager.DiscoverIssues(ctx, triager.DiscoveryInput{ProjectID: projectID, Repo: repo, Snapshot: snapshot, DecisionBudget: decisionBudget})
			return result.QueueItems, err
		},
	}
}

// discoveryLanes builds the ordered lane list for one tick from registered
// coding and internal roles.
//
// Configuration decides which lanes exist, how each filters, and in what
// order they run; the discoverer registry decides what can actually run. A
// role configured without a runner is skipped rather than failing the tick,
// because a tick is the wrong place to report a static configuration fault.
func discoveryLanes(input defaultSchedulerTickInput) []discoveryLane {
	// A nil Config is legal (unit tests, and any caller driving the tick
	// directly). Fall back to the shipped role set so the lanes still run in
	// their documented order: losing every lane because configuration was
	// absent would turn a missing-config bug into silent inactivity.
	roleConfigs := config.RoleConfigs{}
	if input.Config != nil {
		roleConfigs = input.Config.Roles
	}

	discoverers := roleDiscoverers(input)
	roles := config.EffectiveCodingRoles(roleConfigs)
	names := config.CodingRoleNames(roleConfigs)

	lanes := make([]discoveryLane, 0, len(names)+2)
	for _, name := range names {
		role := roles[name]
		lane, ok := discoverers[name]
		if !ok {
			continue
		}
		lane.Name = name
		lane.Priority = role.Priority
		lane.LogWhenDisabled = true
		lanes = append(lanes, lane)
	}

	lanes = append(lanes, triagerLane(input), coordinatorLane(input))
	sort.SliceStable(lanes, func(i, j int) bool { return lanes[i].Priority < lanes[j].Priority })
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
