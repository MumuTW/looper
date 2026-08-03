package gatekeeper

import (
	"context"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func openPullRequestFixture() githubinfra.PullRequestSummary {
	return githubinfra.PullRequestSummary{
		Number: 42, State: "OPEN", HeadSHA: "head-1", BaseRefName: "main",
		ReviewDecision: "APPROVED", UpdatedAt: "2026-07-30T09:00:00Z",
	}
}

func TestSourceFingerprintIncludesExactDiffBudgetBounds(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	pullRequest := openPullRequestFixture()
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github,
		PolicyPermitsTarget: func(string, string, string) bool { return true },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		DiffBudgetForProject: func(string) config.GatekeeperDiffBudget {
			return config.GatekeeperDiffBudget{MaxChangedFiles: 20, MaxDeletions: 100}
		},
	})
	first := runner.sourceFingerprintForProjectWithContract(pullRequest, "project_1", "acme/looper", "")
	runner.diffBudgetForProject = func(string) config.GatekeeperDiffBudget {
		return config.GatekeeperDiffBudget{MaxChangedFiles: 4, MaxDeletions: 100}
	}
	second := runner.sourceFingerprintForProjectWithContract(pullRequest, "project_1", "acme/looper", "")
	if first == second {
		t.Fatalf("fingerprint unchanged after tightening diff budget: %q", first)
	}
}

func discover(t *testing.T, fixture *gatekeeperFixture) DiscoveryResult {
	t.Helper()
	result, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{
		ProjectID: "project_1", Repo: "acme/looper",
	})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	return result
}

// Evaluating one pull request costs branch protection, check runs, the detail
// view, and a review-thread query, so this lane was O(open PRs) in forge round
// trips every tick. A second tick over unchanged pull requests must make none of
// those calls.
func TestDiscoverPullRequestsSkipsUnchangedPullRequests(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}

	first := discover(t, fixture)
	if first.Evaluated != 1 || first.Skipped != 0 {
		t.Fatalf("first tick = %d evaluated / %d skipped, want 1 / 0", first.Evaluated, first.Skipped)
	}
	callsAfterFirst := fixture.github.perPullRequestCalls
	if callsAfterFirst == 0 {
		t.Fatal("first tick made no per-pull-request calls; the fixture cannot detect a skip")
	}

	second := discover(t, fixture)
	if second.Evaluated != 0 || second.Skipped != 1 {
		t.Fatalf("second tick = %d evaluated / %d skipped, want 0 / 1", second.Evaluated, second.Skipped)
	}
	if fixture.github.perPullRequestCalls != callsAfterFirst {
		t.Fatalf("per-pull-request calls = %d, want %d (skip must make no forge calls)",
			fixture.github.perPullRequestCalls, callsAfterFirst)
	}
	if len(second.Reports) != 1 || second.Reports[0].ExpectedHeadSHA != "head-1" {
		t.Fatalf("skipped report = %#v, want the previous report reused", second.Reports)
	}
}

// Anything the list page can observe changing must force a fresh evaluation.
func TestDiscoverPullRequestsReevaluatesWhenTheListPageChanges(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*githubinfra.PullRequestSummary)
	}{
		{name: "head sha", mutate: func(p *githubinfra.PullRequestSummary) { p.HeadSHA = "head-2" }},
		{name: "updated at", mutate: func(p *githubinfra.PullRequestSummary) { p.UpdatedAt = "2026-07-30T09:30:00Z" }},
		{name: "review decision", mutate: func(p *githubinfra.PullRequestSummary) { p.ReviewDecision = "CHANGES_REQUESTED" }},
		{name: "labels", mutate: func(p *githubinfra.PullRequestSummary) { p.Labels = []string{labels.HoldGlobal} }},
		{name: "draft", mutate: func(p *githubinfra.PullRequestSummary) { p.IsDraft = true }},
		{name: "conflicts", mutate: func(p *githubinfra.PullRequestSummary) { p.HasConflicts = true }},
		{name: "base branch", mutate: func(p *githubinfra.PullRequestSummary) { p.BaseRefName = "release" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newGatekeeperFixture(t)
			fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
			discover(t, fixture)

			changed := openPullRequestFixture()
			testCase.mutate(&changed)
			fixture.github.openPullRequests = []githubinfra.PullRequestSummary{changed}

			second := discover(t, fixture)
			if second.Evaluated != 1 || second.Skipped != 0 {
				t.Fatalf("after %s changed: %d evaluated / %d skipped, want 1 / 0",
					testCase.name, second.Evaluated, second.Skipped)
			}
		})
	}
}

// A pending check resolves on its own: it turns green while every field the list
// page can see stays identical, so it must never be skipped.
func TestDiscoverPullRequestsNeverSkipsWhileACheckIsPending(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	fixture.github.checks = githubinfra.PullRequestCheckRuns{
		TotalCount: 1,
		CheckRuns:  []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "in_progress", AppID: 15368}},
	}

	first := discover(t, fixture)
	if !reportAwaitsCheckState(first.Reports[0]) {
		t.Fatalf("first report reasons = %v, want a pending-check reason so the guard is exercised", reasonCodes(first.Reports[0].Reasons))
	}

	second := discover(t, fixture)
	if second.Evaluated != 1 || second.Skipped != 0 {
		t.Fatalf("second tick = %d evaluated / %d skipped, want 1 / 0 while a check is pending",
			second.Evaluated, second.Skipped)
	}
}

// A failed check is the opposite case and the one that decides whether this
// optimisation is worth anything: on a live daemon nearly every open pull request
// carried one, so treating it as volatile made almost nothing skippable. A failure
// does not fix itself — a re-run with a push moves the head SHA and is caught by
// the fingerprint, and a re-run without one is bounded by maxSkipAge.
func TestDiscoverPullRequestsSkipsWhenACheckHasFailed(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	fixture.github.checks = githubinfra.PullRequestCheckRuns{
		TotalCount: 1,
		CheckRuns:  []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure", AppID: 15368}},
	}

	first := discover(t, fixture)
	if !hasReason(first.Reports[0], ReasonCheckFailed) {
		t.Fatalf("first report reasons = %v, want required_check_failed", reasonCodes(first.Reports[0].Reasons))
	}

	second := discover(t, fixture)
	if second.Evaluated != 0 || second.Skipped != 1 {
		t.Fatalf("second tick = %d evaluated / %d skipped, want 0 / 1 (a failed check does not resolve itself)",
			second.Evaluated, second.Skipped)
	}
}

// Branch protection and project policy are gate inputs the fingerprint cannot
// model at all, so an unchanged pull request is still re-evaluated periodically.
//
// The durations here are literals rather than maxSkipAge ± something: expressing
// them in terms of the constant under test makes the test pass for any value of
// it. These pin the 30-minute policy, so widening it is a deliberate edit here too.
func TestDiscoverPullRequestsReevaluatesAfterMaxSkipAge(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	discover(t, fixture)

	fixture.now = fixture.now.Add(29 * time.Minute)
	if second := discover(t, fixture); second.Skipped != 1 {
		t.Fatalf("29 minutes after evaluation: %d skipped, want 1 (still inside the window)", second.Skipped)
	}

	fixture.now = fixture.now.Add(2 * time.Minute)
	third := discover(t, fixture)
	if third.Evaluated != 1 || third.Skipped != 0 {
		t.Fatalf("31 minutes after evaluation: %d evaluated / %d skipped, want 1 / 0", third.Evaluated, third.Skipped)
	}
}

func TestDiscoverPullRequestsReevaluatesWhenTrustIsDemoted(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	trust := config.GatekeeperTrustAuto
	runner := func() *Runner {
		return New(Options{
			Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
			PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
			TrustForProject:     func(string) config.GatekeeperTrustLevel { return trust },
		})
	}

	first := runner().DiscoverPullRequests
	if result, err := first(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("auto discovery() error = %v", err)
	} else if result.Evaluated != 1 || result.Skipped != 0 {
		t.Fatalf("auto discovery = %d evaluated / %d skipped, want 1 / 0", result.Evaluated, result.Skipped)
	}
	trust = config.GatekeeperTrustObserve
	second, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("observe discovery() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 {
		t.Fatalf("demotion discovery = %d evaluated / %d skipped, want 1 / 0", second.Evaluated, second.Skipped)
	}
	if len(fixture.github.labelRemoves) < 2 || fixture.github.labelRemoves[len(fixture.github.labelRemoves)-2].Labels[0] != labels.AutoMerge {
		t.Fatalf("label removals after trust demotion = %#v, want auto-merge retirement", fixture.github.labelRemoves)
	}
}

func hasReason(report Report, code ReasonCode) bool {
	for _, reason := range report.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

// At auto trust the merge route is published only during an evaluation. A
// failed check that is manually rerun to success turns the gate green without
// moving any field the list page can observe, so the failed report must not be
// reused — the PR is re-evaluated so the now-green eligible state is queued
// promptly rather than waiting for maxSkipAge.
func TestDiscoverPullRequestsReevaluatesFailedCheckAtAutoTrust(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	fixture.github.checks = githubinfra.PullRequestCheckRuns{
		TotalCount: 1,
		CheckRuns:  []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure", AppID: 15368}},
	}
	trust := config.GatekeeperTrustAuto
	runner := func() *Runner {
		return New(Options{
			Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
			PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
			TrustForProject:     func(string) config.GatekeeperTrustLevel { return trust },
		})
	}

	first, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first discovery() error = %v", err)
	}
	if first.Evaluated != 1 || first.Skipped != 0 {
		t.Fatalf("first tick = %d evaluated / %d skipped, want 1 / 0", first.Evaluated, first.Skipped)
	}
	if !hasReason(first.Reports[0], ReasonCheckFailed) {
		t.Fatalf("first report reasons = %v, want required_check_failed", reasonCodes(first.Reports[0].Reasons))
	}

	second, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second discovery() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 {
		t.Fatalf("auto trust second tick = %d evaluated / %d skipped, want 1 / 0 (failed check must re-evaluate)",
			second.Evaluated, second.Skipped)
	}
}

func TestDiscoverPullRequestsReevaluatesCancelledCheckAtAutoTrust(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	fixture.github.checks = githubinfra.PullRequestCheckRuns{
		TotalCount: 1,
		CheckRuns:  []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "cancelled", AppID: 15368}},
	}
	trust := config.GatekeeperTrustAuto
	runner := func() *Runner {
		return New(Options{
			Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
			PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
			TrustForProject:     func(string) config.GatekeeperTrustLevel { return trust },
		})
	}

	first, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first discovery() error = %v", err)
	}
	if first.Evaluated != 1 || first.Skipped != 0 {
		t.Fatalf("first tick = %d evaluated / %d skipped, want 1 / 0", first.Evaluated, first.Skipped)
	}
	if !hasReason(first.Reports[0], ReasonCheckCancelled) {
		t.Fatalf("first report reasons = %v, want required_check_cancelled", reasonCodes(first.Reports[0].Reasons))
	}

	second, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second discovery() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 {
		t.Fatalf("auto trust second tick = %d evaluated / %d skipped, want 1 / 0 (cancelled check must re-evaluate)",
			second.Evaluated, second.Skipped)
	}
}

// A provider block after the review check replaces all reasons with the
// provider reason but leaves Evidence.CodexReview.CurrentHeadValid false.
// reportAwaitsCurrentHeadReview must still detect this as waiting for a review
// so the PR is not silently skipped for up to maxSkipAge.
func TestReportAwaitsCurrentHeadReviewDetectsProviderBlockedEvidence(t *testing.T) {
	report := Report{
		Reasons: []Reason{{Code: ReasonProviderStateUnavailable, Subject: "mergeability"}},
		Evidence: Evidence{CodexReview: &CodexReviewEvidence{
			RequiredHeadSHA:  "head-1",
			CurrentHeadValid: false,
		}},
	}
	if !reportAwaitsCurrentHeadReview(report) {
		t.Fatalf("reportAwaitsCurrentHeadReview() = false, want true for provider-blocked report with invalid CodexReview evidence")
	}
}

// A blocked convergence report used to disable unchanged-report reuse
// indefinitely, so the default scheduler re-ran the full EvaluatePullRequest
// sequence every tick even though the changing input is local SQLite metadata.
// The convergence revision is now compared locally: while the Reviewer state is
// unchanged the blocked report is reused without forge round trips, and only an
// actual advance (Reviewer progress) forces a re-evaluation.
func TestDiscoverPullRequestsSkipsBlockedConvergenceUntilReviewerAdvances(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":3,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"open"}}},"action":"continue","reason":"converging","status":"active","updatedAt":"2026-07-30T10:00:00Z"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)

	first := discover(t, fixture)
	if first.Evaluated != 1 || first.Skipped != 0 || first.Reports[0].Eligible {
		t.Fatalf("first discovery = %#v, want evaluated convergence blocker", first)
	}
	if !hasReason(first.Reports[0], ReasonReviewerConvergence) {
		t.Fatalf("first report reasons = %v, want convergence blocker", reasonCodes(first.Reports[0].Reasons))
	}
	callsAfterFirst := fixture.github.perPullRequestCalls
	if callsAfterFirst == 0 {
		t.Fatal("first tick made no per-pull-request calls; the fixture cannot detect a skip")
	}

	// No Reviewer progress: the convergence revision is unchanged, so the
	// blocked report is reused and the second tick makes no forge round trips.
	second := discover(t, fixture)
	if second.Evaluated != 0 || second.Skipped != 1 {
		t.Fatalf("second tick = %d evaluated / %d skipped, want 0 / 1 (unchanged convergence revision)", second.Evaluated, second.Skipped)
	}
	if fixture.github.perPullRequestCalls != callsAfterFirst {
		t.Fatalf("per-pull-request calls = %d, want %d (unchanged convergence must not re-poll forge)",
			fixture.github.perPullRequestCalls, callsAfterFirst)
	}

	// Reviewer progress advances the convergence revision, so the third tick
	// re-evaluates and picks up the resolved state.
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":4,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"resolved"}}},"action":"complete","reason":"severity_floor_reached","status":"active","updatedAt":"2026-07-30T10:05:00Z"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)
	third := discover(t, fixture)
	if third.Evaluated != 1 || third.Skipped != 0 || !third.Reports[0].Eligible {
		t.Fatalf("third discovery = %#v, want reevaluated eligible report after convergence advanced", third)
	}
}
