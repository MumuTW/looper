package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// TestDiscoverIssuesSurvivesSingleIssueLoadFailure covers the failure mode
// from issue #543: one issue whose timeline cannot be loaded — for example a
// payload beyond the gh capture cap — must defer only itself. Returning the
// error aborted the tick and suspended triage and dispatch for every other
// issue in the repo for as long as that one issue kept failing. The skipped
// issue stays open and re-enters discovery on the next tick.
func TestDiscoverIssuesSurvivesSingleIssueLoadFailure(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) { cfg.Roles.Coordinator.Enabled = true })
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}, {Number: 2}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "Healthy", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	fixture.github.details[2] = githubinfra.IssueDetail{Number: 2, Title: "Heavy", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	fixture.github.timelineErr[2] = errors.New("GitHub command output truncated: stdout after 262144 bytes")

	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v, want the tick to survive one issue's load failure", err)
	}

	if got := countOperations(fixture.github.ops, "create-comment"); got != 1 {
		t.Fatalf("comment creates = %d, want 1: the healthy issue still triaged; ops=%v", got, fixture.github.ops)
	}
	if got := countOperations(fixture.github.ops, "add:triaged"); got != 1 {
		t.Fatalf("triaged label adds = %d, want 1; ops=%v", got, fixture.github.ops)
	}
}

// TestDiscoverIssuesSurfacesSystemicIssueLoadFailure covers the scheduler-facing
// contract for issue #543's resilience path. A single issue-local load failure
// is isolated (see TestDiscoverIssuesSurvivesSingleIssueLoadFailure), but when
// every per-issue read fails after ListOpenIssues succeeds — a repo-wide forge
// outage or command incompatibility — the lane produced nothing and must not
// report Ticked:true as if it had simply had no work. Surfacing the error keeps
// the scheduler from masking the lane failure while triage, dispatch, and
// merge-watch are all suspended; other lanes still run because the scheduler
// records a lane error without aborting their ticks.
func TestDiscoverIssuesSurfacesSystemicIssueLoadFailure(t *testing.T) {
	t.Parallel()
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) { cfg.Roles.Coordinator.Enabled = true })
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1}, {Number: 2}}
	fixture.github.details[1] = githubinfra.IssueDetail{Number: 1, Title: "A", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	fixture.github.details[2] = githubinfra.IssueDetail{Number: 2, Title: "B", Author: "octo", CreatedAt: fixture.now.Format(time.RFC3339)}
	fixture.github.timelineErr[1] = errors.New("GitHub command output truncated: stdout after 262144 bytes")
	fixture.github.timelineErr[2] = errors.New("GitHub command output truncated: stdout after 262144 bytes")

	_, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"})
	if err == nil {
		t.Fatal("DiscoverIssues() error = nil, want an error surfacing the systemic lane failure when every issue load failed")
	}
	if !strings.Contains(err.Error(), "could not load any issue") {
		t.Fatalf("DiscoverIssues() error = %v, want a systemic-load error naming the repo", err)
	}
	if got := countOperations(fixture.github.ops, "create-comment"); got != 0 {
		t.Fatalf("comment creates = %d, want 0: no issue should triage when none could be loaded; ops=%v", got, fixture.github.ops)
	}
}
