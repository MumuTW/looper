package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

// markReadyFixture wires the smallest world in which a looper-authored draft is
// eligible: a triaged Issue, a linked draft PR that closes it, green required
// checks, and no commits but the daemon's own. Each test below breaks exactly
// one of those and asserts the draft is left alone.
func newMarkReadyFixture(t *testing.T, mutate ...func(*githubinfra.PullRequestDetail)) coordinatorFixture {
	t.Helper()
	fixture := newCoordinatorFixture(t, func(cfg *config.Config) {
		cfg.Roles.Coordinator.Enabled = true
		cfg.Roles.Coordinator.MarkReady.Enabled = true
	})
	detail := githubinfra.PullRequestDetail{
		Number:         77,
		Body:           "Closes #1",
		State:          "open",
		IsDraft:        true,
		HeadSHA:        "abc123",
		BaseRefName:    "main",
		Labels:         []string{labels.DefaultWorkerReadyTrigger},
		Mergeable:      boolPtr(true),
		MergeableState: "draft",
	}
	for _, fn := range mutate {
		fn(&detail)
	}
	fixture.github.issues = []githubinfra.IssueSummary{{Number: 1, Labels: []string{"triaged"}}}
	fixture.github.details[1] = githubinfra.IssueDetail{
		Number:    1,
		Title:     "Bug",
		Author:    "octo",
		Labels:    []string{"triaged"},
		CreatedAt: fixture.now.Add(-time.Hour).Format(time.RFC3339),
	}
	fixture.github.prDetails[77] = detail
	fixture.github.timeline[1] = []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 77, "html_url": "https://github.com/acme/looper/pull/77"}}}}}
	fixture.github.branchProtection["main"] = githubinfra.BranchProtection{Enabled: true, HasRequiredChecks: true, RequiredChecks: []string{"verify"}}
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "completed", Conclusion: "success"}}}
	fixture.github.prCommits[77] = []githubinfra.PullRequestCommit{{OID: "abc123", Authors: []string{"looper"}}}
	return fixture
}

func runMarkReadyTick(t *testing.T, fixture coordinatorFixture) {
	t.Helper()
	if _, err := fixture.runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: fixture.projectID, Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
}

func assertDraftUntouched(t *testing.T, fixture coordinatorFixture) {
	t.Helper()
	if len(fixture.github.markedReady) != 0 {
		t.Fatalf("markedReady = %#v, want draft left in draft", fixture.github.markedReady)
	}
}

func TestRunnerMarkReadyPublishesGreenLooperDraft(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	runMarkReadyTick(t, fixture)
	if len(fixture.github.markedReady) != 1 || fixture.github.markedReady[0].PRNumber != 77 || fixture.github.markedReady[0].Repo != "acme/looper" {
		t.Fatalf("markedReady = %#v, want PR #77 published", fixture.github.markedReady)
	}
	assertOrderedOps(t, fixture.github.ops, []string{"mark-ready:77"})
}

// The lane derives everything from live Pull Request state and keeps no marker
// of its own, so a second tick over an already-published PR must be a no-op
// rather than a repeated mutation.
func TestRunnerMarkReadyIsIdempotentAcrossTicks(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.reconfigure(func(cfg *config.Config) { cfg.Roles.Coordinator.PollInterval = "0s" })
	runMarkReadyTick(t, fixture)
	published := fixture.github.prDetails[77]
	published.IsDraft = false
	fixture.github.prDetails[77] = published
	runMarkReadyTick(t, fixture)
	if len(fixture.github.markedReady) != 1 {
		t.Fatalf("markedReady = %#v, want exactly one publish across two ticks", fixture.github.markedReady)
	}
}

func TestRunnerMarkReadyDisabledByDefault(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.reconfigure(func(cfg *config.Config) { cfg.Roles.Coordinator.MarkReady.Enabled = false })
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadySkipsForeignScope(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.reconfigure(func(cfg *config.Config) {
		cfg.Roles.Coordinator.MarkReady.Scope = config.CoordinatorMarkReadyScope("all")
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesPendingChecksAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "in_progress"}}}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesFailedChecksAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "completed", Conclusion: "failure"}}}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesMissingRequiredCheckAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "lint", Status: "completed", Conclusion: "success"}}}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// Without branch protection to name the required checks, every check observed
// on the head counts — otherwise "no required checks" would read as "nothing to
// wait for" and publish drafts mid-run.
func TestRunnerMarkReadyWaitsOnObservedChecksWithoutBranchProtection(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.branchProtection["main"] = githubinfra.BranchProtection{}
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "in_progress"}}}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesConflictingDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t, func(detail *githubinfra.PullRequestDetail) {
		detail.Mergeable = boolPtr(false)
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDirtyDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t, func(detail *githubinfra.PullRequestDetail) {
		detail.MergeableState = "dirty"
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesHeldDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t, func(detail *githubinfra.PullRequestDetail) {
		detail.Labels = append(detail.Labels, labels.HoldGlobal)
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDoNotMergeDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t, func(detail *githubinfra.PullRequestDetail) {
		detail.Labels = append(detail.Labels, labels.DoNotMerge)
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesHumanAuthoredDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t, func(detail *githubinfra.PullRequestDetail) {
		detail.Labels = []string{"enhancement"}
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesUnlinkedDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t, func(detail *githubinfra.PullRequestDetail) {
		detail.Body = "Refactors the world"
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftWithHumanCommitsAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCommits[77] = []githubinfra.PullRequestCommit{
		{OID: "abc123", Authors: []string{"looper"}},
		{OID: "def456", Authors: []string{"octo"}},
	}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftWithUnattributedCommitsAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCommits[77] = []githubinfra.PullRequestCommit{{OID: "abc123"}}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// A forge failure in this lane must not abort the tick: triage and dispatch run
// after merge-watch and do not depend on a draft being published.
func TestRunnerMarkReadyForgeFailureDoesNotAbortTick(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.failMarkReady[77] = errors.New("HTTP 500 server error")
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyCommitReadFailureLeavesDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.failPRCommits[77] = errors.New("HTTP 502 bad gateway")
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestForeignCommitAuthorsIgnoresDaemonCaseAndDuplicates(t *testing.T) {
	t.Parallel()
	foreign, unattributed := foreignCommitAuthors([]githubinfra.PullRequestCommit{
		{OID: "1", Authors: []string{"Looper"}},
		{OID: "2", Authors: []string{"octo"}},
		{OID: "3", Authors: []string{"octo", ""}},
		{OID: "4"},
	}, "looper")
	if len(foreign) != 1 || foreign[0] != "octo" {
		t.Fatalf("foreign = %#v, want a single octo entry", foreign)
	}
	if unattributed != 2 {
		t.Fatalf("unattributed = %d, want 2", unattributed)
	}
}
