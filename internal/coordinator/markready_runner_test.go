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
		Author:         "looper",
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
		State:     "open",
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

// The whole decision is evidence about one head. A push that lands between the
// evidence and `gh pr ready` — which takes no head argument — would publish a
// head nothing has evaluated.
func TestRunnerMarkReadyLeavesDraftAloneWhenHeadMovedBeforeMutation(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	pushed := fixture.github.prDetails[77]
	pushed.HeadSHA = "def456"
	fixture.github.prDetailRevalidations[77] = pushed
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// A hold label is a human veto that lands without moving the head, so the
// revalidation has to read it rather than compare SHAs and stop.
func TestRunnerMarkReadyLeavesDraftAloneWhenHeldBeforeMutation(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	held := fixture.github.prDetails[77]
	held.Labels = append(append([]string(nil), held.Labels...), labels.HoldGlobal)
	fixture.github.prDetailRevalidations[77] = held
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftAloneWhenSameHeadCheckTurnsPending(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCheckRunRevalidations["abc123"] = githubinfra.PullRequestCheckRuns{
		CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "in_progress"}},
	}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftAloneWhenDraftHistoryChangesBeforeMutation(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prDraftEventRevalidations[77] = []githubinfra.PullRequestDraftEvent{
		{Event: githubinfra.DraftEventConvertToDraft, Actor: "octo", CreatedAt: "2026-05-14T12:05:00Z"},
	}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftAloneWhenBaseChangesBeforeMutation(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	retargeted := fixture.github.prDetails[77]
	retargeted.BaseRefName = "release"
	fixture.github.prDetailRevalidations[77] = retargeted
	fixture.github.branchProtection["release"] = fixture.github.branchProtection["main"]
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftAloneWhenCommitterChangesBeforeMutation(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCommits[77] = []githubinfra.PullRequestCommit{{OID: "abc123", Authors: []string{"looper"}, Committers: []string{"looper"}, CommitterKnown: true}}
	fixture.github.prCommitRevalidations[77] = []githubinfra.PullRequestCommit{{OID: "abc123", Authors: []string{"looper"}, Committers: []string{"octo"}, CommitterKnown: true}}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftAloneWhenChecksAreTruncated(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{
		TotalCount: 101,
		CheckRuns:  []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "completed", Conclusion: "success"}},
	}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftAloneWhenIssueClosesBeforeMutation(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	closed := fixture.github.details[1]
	closed.State = "closed"
	fixture.github.details[1] = closed
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyLeavesDraftAloneWhenIssueClosesDuringEvidenceReads(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	closed := fixture.github.details[1]
	closed.State = "closed"
	fixture.github.issueRevalidations[1] = closed
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// Converting a published PR back to draft is an explicit "not ready". The
// timeline is what makes that durable: nothing on the Pull Request itself
// distinguishes it from the draft the machine opened, and an in-memory flag
// would forget across a daemon restart.
func TestRunnerMarkReadyNeverRepublishesAHumanRedraft(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prDraftEvents[77] = []githubinfra.PullRequestDraftEvent{
		{Event: githubinfra.DraftEventReadyForReview, Actor: "looper", CreatedAt: "2026-05-14T11:00:00Z"},
		{Event: githubinfra.DraftEventConvertToDraft, Actor: "octo", CreatedAt: "2026-05-14T11:30:00Z"},
	}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// The daemon's own publish is not a re-draft: a PR it published and that nobody
// converted back stays eligible if it somehow returns to this lane.
func TestRunnerMarkReadyPublishesAfterItsOwnReadyForReviewEvent(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.prDraftEvents[77] = []githubinfra.PullRequestDraftEvent{
		{Event: githubinfra.DraftEventReadyForReview, Actor: "Looper", CreatedAt: "2026-05-14T11:00:00Z"},
	}
	runMarkReadyTick(t, fixture)
	if len(fixture.github.markedReady) != 1 {
		t.Fatalf("markedReady = %#v, want PR #77 published", fixture.github.markedReady)
	}
}

func TestRunnerMarkReadyDraftHistoryReadFailureLeavesDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.failPRDraftEvents[77] = errors.New("HTTP 502 bad gateway")
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// A maintainer can open a draft over a branch of purely machine-written commits
// and label it: scope and every commit check pass, but the Pull Request is
// theirs.
func TestRunnerMarkReadyLeavesHumanOwnedDraftAlone(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t, func(detail *githubinfra.PullRequestDetail) {
		detail.Author = "octo"
	})
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// An unprotected branch whose head has registered no check yet produces the
// same empty set as a green one. Emptiness is not evidence, so the draft waits
// rather than publishing seconds before CI appears.
func TestRunnerMarkReadyLeavesDraftAloneWhenNoCheckIsKnownYet(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.branchProtection["main"] = githubinfra.BranchProtection{}
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

// Branch protection can bind a required context to one GitHub App. A green run
// of the same name from a different App is a different check.
func TestRunnerMarkReadyLeavesDraftAloneWhenRequiredCheckAppDiffers(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.branchProtection["main"] = githubinfra.BranchProtection{
		Enabled:            true,
		HasRequiredChecks:  true,
		RequiredChecks:     []string{"verify"},
		RequiredCheckRules: []githubinfra.RequiredCheckRule{{Context: "verify", AppID: 15368}},
	}
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{
		CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "completed", Conclusion: "success", AppID: 999}},
	}
	runMarkReadyTick(t, fixture)
	assertDraftUntouched(t, fixture)
}

func TestRunnerMarkReadyPublishesWhenRequiredCheckAppMatches(t *testing.T) {
	t.Parallel()
	fixture := newMarkReadyFixture(t)
	fixture.github.branchProtection["main"] = githubinfra.BranchProtection{
		Enabled:            true,
		HasRequiredChecks:  true,
		RequiredChecks:     []string{"verify"},
		RequiredCheckRules: []githubinfra.RequiredCheckRule{{Context: "verify", AppID: 15368}},
	}
	fixture.github.prCheckRuns["abc123"] = githubinfra.PullRequestCheckRuns{
		CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "verify", Status: "completed", Conclusion: "success", AppID: 15368}},
	}
	runMarkReadyTick(t, fixture)
	if len(fixture.github.markedReady) != 1 {
		t.Fatalf("markedReady = %#v, want PR #77 published", fixture.github.markedReady)
	}
}

// Every reference an Issue ever accumulated used to cost this lane a Pull
// Request read per tick, forever — a PR merged six months ago re-read 288 times
// a day to be told it is not a draft. Only an open draft can be acted on, so
// the lane now costs a closed reference nothing at all: the read the fixture
// still sees is merge-watch's own, which happens with mark-ready switched off
// too.
func TestRunnerMarkReadyDoesNotReadHistoricalLinkedPullRequests(t *testing.T) {
	t.Parallel()
	withMergedReference := func(t *testing.T, enabled bool) (int, coordinatorFixture) {
		t.Helper()
		fixture := newMarkReadyFixture(t)
		if !enabled {
			fixture.reconfigure(func(cfg *config.Config) { cfg.Roles.Coordinator.MarkReady.Enabled = false })
		}
		merged := fixture.github.prDetails[77]
		merged.Number = 60
		merged.IsDraft = false
		merged.State = "closed"
		merged.MergedAt = "2026-01-01T00:00:00Z"
		fixture.github.prDetails[60] = merged
		fixture.github.timeline[1] = append(fixture.github.timeline[1], map[string]any{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 60, "html_url": "https://github.com/acme/looper/pull/60"}}}})
		runMarkReadyTick(t, fixture)
		return fixture.github.prDetailReads[60], fixture
	}

	withLane, fixture := withMergedReference(t, true)
	withoutLane, _ := withMergedReference(t, false)
	if withLane != withoutLane {
		t.Fatalf("reads of the merged reference = %d with the lane and %d without it, want the lane to add none", withLane, withoutLane)
	}
	if len(fixture.github.markedReady) != 1 || fixture.github.markedReady[0].PRNumber != 77 {
		t.Fatalf("markedReady = %#v, want PR #77 published", fixture.github.markedReady)
	}
}

// A recording logger, so the startup warning below is asserted on rather than
// assumed. Only Warn is inspected; the rest satisfy the interface.
type recordingCoordinatorLogger struct {
	warnings []string
}

func (l *recordingCoordinatorLogger) Debug(string, map[string]any) {}
func (l *recordingCoordinatorLogger) Info(string, map[string]any)  {}
func (l *recordingCoordinatorLogger) Warn(message string, _ map[string]any) {
	l.warnings = append(l.warnings, message)
}
func (l *recordingCoordinatorLogger) Error(string, map[string]any) {}

// Mark-ready hands the reviewer lane a Pull Request it will refuse in the
// default configuration. Silently flipping enableSelfReview is not this
// feature's call to make, so it says so at startup instead.
func TestNewWarnsWhenMarkReadyPublishesForANonReviewingReviewer(t *testing.T) {
	t.Parallel()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []config.ProjectRefConfig{{ID: "demo"}}
	cfg.Roles.Coordinator.MarkReady.Enabled = true

	logger := &recordingCoordinatorLogger{}
	New(Options{Config: &cfg, Logger: logger})
	if len(logger.warnings) != 1 {
		t.Fatalf("warnings = %#v, want one mark-ready reviewer warning", logger.warnings)
	}

	cfg.Roles.Reviewer.Discovery.Triggers.EnableSelfReview = true
	cfg.Roles.Reviewer.Discovery.Triggers.RequireReviewRequest = false
	cfg.Roles.Coding = config.CodingRolesFromLegacy(cfg.Roles)
	reviewing := &recordingCoordinatorLogger{}
	New(Options{Config: &cfg, Logger: reviewing})
	if len(reviewing.warnings) != 0 {
		t.Fatalf("warnings = %#v, want none once the reviewer reviews looper-authored pull requests", reviewing.warnings)
	}
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

func TestPRLinksIssueAcceptsFullGitHubURL(t *testing.T) {
	t.Parallel()
	if !prLinksIssue("acme/looper", 42, "Closes https://github.com/acme/looper/issues/42") {
		t.Fatal("prLinksIssue() = false, want full GitHub issue URL to link the issue")
	}
}
