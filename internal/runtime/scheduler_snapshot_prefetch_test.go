package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

// forgeResolverForPrefetchTest matches what the tick builds when no config is
// captured: the default GitHub provider, which is the case that uses snapshots.
func forgeResolverForPrefetchTest() forge.Resolver {
	return forge.NewResolver(config.Config{})
}

func prefetchTestProjects(ids ...string) []storage.ProjectRecord {
	projects := make([]storage.ProjectRecord, 0, len(ids))
	for _, id := range ids {
		metadata := fmt.Sprintf(`{"repo":"nexu-io/%s"}`, id)
		projects = append(projects, storage.ProjectRecord{ID: id, MetadataJSON: &metadata, RepoPath: "/tmp/" + id})
	}
	return projects
}

func prefetchTestLanes(readsPullRequests, readsIssues bool) []discoveryLane {
	return []discoveryLane{{
		Name:              "fixture",
		Present:           true,
		Enabled:           func(string) bool { return true },
		ReadsPullRequests: readsPullRequests,
		ReadsIssues:       readsIssues,
	}}
}

func freshWalkIndex() *atomic.Int64 {
	var walked atomic.Int64
	walked.Store(-1)
	return &walked
}

// The barrier only releases once every warmed project's forge read is in flight
// at the same time, so a serial warm blocks until the deadline instead of passing
// quietly. Project index 0 is deliberately not warmed — the walk reaches it
// immediately — so three projects yield two concurrent reads.
func TestPrefetchProjectSnapshotsWarmsLaterProjectsConcurrently(t *testing.T) {
	t.Parallel()

	projects := prefetchTestProjects("alpha", "beta", "gamma")
	inFlight := make(chan string, len(projects)*2)
	release := make(chan struct{})
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		inFlight <- strings.Join(options.Args, " ")
		<-release
		return shell.Result{Stdout: "[]"}, nil
	}})

	wait := prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway},
		projects, forgeResolverForPrefetchTest(), prefetchTestLanes(true, false),
		snapshotForPrefetchTest(gateway), freshWalkIndex())

	deadline := time.After(10 * time.Second)
	seen := 0
	for seen < 2 {
		select {
		case <-inFlight:
			seen++
		case <-deadline:
			close(release)
			t.Fatalf("concurrent forge reads in flight = %d, want 2; snapshot warming is serialized", seen)
		}
	}
	close(release)
	wait()
}

// Blocking the walk on the warm would let one slow repo stall every healthy
// project's discovery and claims. prefetchProjectSnapshots must therefore return
// while its reads are still outstanding.
func TestPrefetchProjectSnapshotsDoesNotBlockOnSlowRepo(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, _ shell.Options) (shell.Result, error) {
		entered <- struct{}{}
		<-release
		return shell.Result{Stdout: "[]"}, nil
	}})

	returned := make(chan func(), 1)
	go func() {
		returned <- prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway},
			prefetchTestProjects("alpha", "beta"), forgeResolverForPrefetchTest(), prefetchTestLanes(true, false),
			snapshotForPrefetchTest(gateway), freshWalkIndex())
	}()

	// The warm must be in flight...
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("warm never started")
	}
	// ...and the call must already have returned despite that read still blocking.
	var wait func()
	select {
	case wait = <-returned:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("prefetchProjectSnapshots blocked on an outstanding warm; one slow repo would stall every project")
	}
	close(release)
	wait()
}

// Warming a page no enabled lane reads is a `gh` call per project per tick that
// hides no latency.
func TestPrefetchProjectSnapshotsSkipsPagesWithNoConsumer(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name              string
		readsPullRequests bool
		readsIssues       bool
		wantPRCalls       int
		wantIssueCalls    int
	}{
		{name: "pull requests only", readsPullRequests: true, wantPRCalls: 1},
		{name: "issues only", readsIssues: true, wantIssueCalls: 1},
		{name: "both", readsPullRequests: true, readsIssues: true, wantPRCalls: 1, wantIssueCalls: 1},
		{name: "neither"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var prCalls, issueCalls int
			gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
				args := strings.Join(options.Args, " ")
				mu.Lock()
				switch {
				case strings.Contains(args, "issue"):
					issueCalls++
				case strings.Contains(args, "pr"):
					prCalls++
				}
				mu.Unlock()
				return shell.Result{Stdout: "[]"}, nil
			}})

			wait := prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway},
				prefetchTestProjects("alpha", "beta"), forgeResolverForPrefetchTest(),
				prefetchTestLanes(testCase.readsPullRequests, testCase.readsIssues),
				snapshotForPrefetchTest(gateway), freshWalkIndex())
			wait()

			mu.Lock()
			defer mu.Unlock()
			if prCalls != testCase.wantPRCalls || issueCalls != testCase.wantIssueCalls {
				t.Fatalf("warm calls = %d PR / %d issue, want %d / %d", prCalls, issueCalls, testCase.wantPRCalls, testCase.wantIssueCalls)
			}
		})
	}
}

// A project the walk already reached is fetching for itself; warming it would
// duplicate exactly the call this change exists to remove.
func TestPrefetchProjectSnapshotsSkipsProjectsTheWalkPassed(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, _ shell.Options) (shell.Result, error) {
		calls.Add(1)
		return shell.Result{Stdout: "[]"}, nil
	}})

	walked := freshWalkIndex()
	walked.Store(5) // the walk is already past every project

	wait := prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway},
		prefetchTestProjects("alpha", "beta", "gamma"), forgeResolverForPrefetchTest(),
		prefetchTestLanes(true, true), snapshotForPrefetchTest(gateway), walked)
	wait()

	if got := calls.Load(); got != 0 {
		t.Fatalf("forge calls = %d, want 0 (every project was already walked)", got)
	}
}

// A cold snapshot is a valid outcome: the lane fetches it itself. A failing warm
// must never surface as a panic or a tick error.
func TestPrefetchProjectSnapshotsToleratesFailure(t *testing.T) {
	t.Parallel()

	gateway := githubinfra.New(githubinfra.Options{GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		return shell.Result{ExitCode: 1, Stderr: "gh: rate limited"}, fmt.Errorf("gh failed")
	}})
	wait := prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway},
		prefetchTestProjects("alpha", "beta"), forgeResolverForPrefetchTest(), prefetchTestLanes(true, true),
		snapshotForPrefetchTest(gateway), freshWalkIndex())
	wait()
}

// A lone project has no latency to overlap and the walk reaches it immediately.
func TestPrefetchProjectSnapshotsSkipsSingleProject(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		calls.Add(1)
		return shell.Result{Stdout: "[]"}, nil
	}})
	wait := prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway},
		prefetchTestProjects("alpha"), forgeResolverForPrefetchTest(), prefetchTestLanes(true, true),
		snapshotForPrefetchTest(gateway), freshWalkIndex())
	wait()

	if got := calls.Load(); got != 0 {
		t.Fatalf("forge calls = %d, want 0 (a lone project warms itself on first use)", got)
	}
}

func snapshotForPrefetchTest(gateway *githubinfra.Gateway) func(string) *githubinfra.DiscoverySnapshot {
	tickState := githubinfra.NewDiscoveryTickState()
	var mu sync.Mutex
	snapshots := map[string]*githubinfra.DiscoverySnapshot{}
	return func(projectID string) *githubinfra.DiscoverySnapshot {
		mu.Lock()
		defer mu.Unlock()
		if snapshot, ok := snapshots[projectID]; ok {
			return snapshot
		}
		snapshot := githubinfra.NewDiscoverySnapshot(gateway, tickState, githubinfra.DiscoverySnapshotOptions{PullRequestLimit: 30, IssueLimit: 30})
		snapshots[projectID] = snapshot
		return snapshot
	}
}
