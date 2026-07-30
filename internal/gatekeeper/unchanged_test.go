package gatekeeper

import (
	"context"
	"testing"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func openPullRequestFixture() githubinfra.PullRequestSummary {
	return githubinfra.PullRequestSummary{
		Number: 42, State: "OPEN", HeadSHA: "head-1", BaseRefName: "main",
		ReviewDecision: "APPROVED", UpdatedAt: "2026-07-30T09:00:00Z",
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

func hasReason(report Report, code ReasonCode) bool {
	for _, reason := range report.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}
