package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// The barrier only releases once every project's forge read is in flight at the
// same time, so a serial warm blocks until the deadline instead of passing
// quietly. This is the whole point of the prefetch: a tick used to pay each
// project's forge latency end to end.
func TestPrefetchProjectSnapshotsWarmsProjectsConcurrently(t *testing.T) {
	t.Parallel()

	projects := []storage.ProjectRecord{}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		metadata := fmt.Sprintf(`{"repo":"nexu-io/%s"}`, id)
		projects = append(projects, storage.ProjectRecord{ID: id, MetadataJSON: &metadata, RepoPath: "/tmp/" + id})
	}

	inFlight := make(chan string, len(projects)*2)
	release := make(chan struct{})
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		inFlight <- strings.Join(options.Args, " ")
		<-release
		return shell.Result{Stdout: "[]"}, nil
	}})

	tickState := githubinfra.NewDiscoveryTickState()
	var snapshotMu sync.Mutex
	snapshots := map[string]*githubinfra.DiscoverySnapshot{}
	snapshotFor := func(projectID string) *githubinfra.DiscoverySnapshot {
		snapshotMu.Lock()
		defer snapshotMu.Unlock()
		if snapshot, ok := snapshots[projectID]; ok {
			return snapshot
		}
		snapshot := githubinfra.NewDiscoverySnapshot(gateway, tickState, githubinfra.DiscoverySnapshotOptions{PullRequestLimit: 30, IssueLimit: 30})
		snapshots[projectID] = snapshot
		return snapshot
	}

	input := defaultSchedulerTickInput{GitHubGateway: gateway}
	done := make(chan struct{})
	go func() {
		defer close(done)
		prefetchProjectSnapshots(context.Background(), input, projects, forgeResolverForPrefetchTest(), snapshotFor)
	}()

	deadline := time.After(10 * time.Second)
	seen := 0
	for seen < len(projects) {
		select {
		case <-inFlight:
			seen++
		case <-deadline:
			close(release)
			t.Fatalf("concurrent forge reads in flight = %d, want %d; snapshot warming is serialized", seen, len(projects))
		}
	}
	close(release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prefetchProjectSnapshots did not return after the reads were released")
	}
}

// A cold snapshot is a valid outcome: the lane fetches it itself. A failing warm
// must therefore never surface as a tick error or a panic.
func TestPrefetchProjectSnapshotsToleratesFailure(t *testing.T) {
	t.Parallel()

	metadataA := `{"repo":"nexu-io/alpha"}`
	metadataB := `{"repo":"nexu-io/beta"}`
	projects := []storage.ProjectRecord{
		{ID: "alpha", MetadataJSON: &metadataA, RepoPath: "/tmp/alpha"},
		{ID: "beta", MetadataJSON: &metadataB, RepoPath: "/tmp/beta"},
	}
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		return shell.Result{ExitCode: 1, Stderr: "gh: rate limited"}, fmt.Errorf("gh failed")
	}})
	tickState := githubinfra.NewDiscoveryTickState()
	snapshotFor := func(string) *githubinfra.DiscoverySnapshot {
		return githubinfra.NewDiscoverySnapshot(gateway, tickState, githubinfra.DiscoverySnapshotOptions{PullRequestLimit: 30, IssueLimit: 30})
	}

	prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway}, projects, forgeResolverForPrefetchTest(), snapshotFor)
}

// A single project has no latency to overlap, so warming must not spawn for it.
func TestPrefetchProjectSnapshotsSkipsSingleProject(t *testing.T) {
	t.Parallel()

	metadata := `{"repo":"nexu-io/alpha"}`
	var calls int
	gateway := githubinfra.New(githubinfra.Options{GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		calls++
		return shell.Result{Stdout: "[]"}, nil
	}})
	tickState := githubinfra.NewDiscoveryTickState()
	snapshotFor := func(string) *githubinfra.DiscoverySnapshot {
		return githubinfra.NewDiscoverySnapshot(gateway, tickState, githubinfra.DiscoverySnapshotOptions{PullRequestLimit: 30, IssueLimit: 30})
	}

	prefetchProjectSnapshots(context.Background(), defaultSchedulerTickInput{GitHubGateway: gateway},
		[]storage.ProjectRecord{{ID: "alpha", MetadataJSON: &metadata, RepoPath: "/tmp/alpha"}}, forgeResolverForPrefetchTest(), snapshotFor)

	if calls != 0 {
		t.Fatalf("forge calls = %d, want 0 (a lone project warms itself on first use)", calls)
	}
}
