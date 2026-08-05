package gatekeeper

import (
	"context"
	"errors"
	"testing"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func TestAutoGatekeeperIgnoresBlockedMergeabilityWhilePublishingOwnStatus(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.mergeable.MergeableState = "blocked"
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible {
		t.Fatalf("report = %#v, want eligible despite blocked mergeability waiting on own status", report)
	}
	if len(fixture.github.statusCalls) != 1 || fixture.github.statusCalls[0].State != "success" {
		t.Fatalf("status calls = %#v, want success published for head-1", fixture.github.statusCalls)
	}
}

func TestAutoGatekeeperAcceptsMarkerlessCleanReviewFromDurableEvidence(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	seedReviewerReviewEvent(t, fixture, "head-1", "COMMENT", "reviewer-loop", 1)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || report.Evidence.CodexReviewOutcome != "clean" {
		t.Fatalf("report = %#v, want eligible clean review from durable evidence", report)
	}
	if len(fixture.github.statusCalls) != 1 || fixture.github.statusCalls[0].State != "success" {
		t.Fatalf("status calls = %#v, want success", fixture.github.statusCalls)
	}
}

func TestAutoGatekeeperPublishesErrorStatusWhenPullRequestReadFails(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.viewErr = errors.New("provider unavailable")
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || report.ObservedHeadSHA != "" {
		t.Fatalf("report = %#v, want provider block without observed head", report)
	}
	if len(fixture.github.statusCalls) != 1 || fixture.github.statusCalls[0].SHA != "head-1" || fixture.github.statusCalls[0].State != "error" {
		t.Fatalf("status calls = %#v, want error status on expected head", fixture.github.statusCalls)
	}
}

func TestAutoGatekeeperDoesNotCacheReportAfterStatusPublishFailure(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}
	fixture.github.statusErr = errors.New("rate limited")
	pr := githubinfra.PullRequestSummary{
		Number: 42, HeadSHA: "head-1", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z",
		BaseRefName: "main", ReviewDecision: "APPROVED",
	}
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{pr}
	runner := fixture.autoRunner()

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.Evaluated != 1 {
		t.Fatalf("first discovery = %#v, want one evaluated pull request", first)
	}

	fixture.github.statusErr = nil
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 {
		t.Fatalf("second discovery = %#v, want re-evaluation after status failure", second)
	}
	if len(fixture.github.statusCalls) < 2 || fixture.github.statusCalls[1].State != "success" {
		t.Fatalf("status calls = %#v, want retried success publication", fixture.github.statusCalls)
	}
}

func TestDiscoverPullRequestsContinuesAfterStatusPublishFailure(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}
	fixture.github.statusErr = errors.New("rate limited")
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{
		{Number: 41, HeadSHA: "head-a", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED"},
		{Number: 42, HeadSHA: "head-b", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED"},
	}
	runner := fixture.autoRunner()

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v, want per-PR status failure isolated", err)
	}
	if result.Evaluated != 2 || len(result.Reports) != 2 {
		t.Fatalf("result = %#v, want both pull requests evaluated", result)
	}
}

func TestAutoGatekeeperNeverMerges(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible {
		t.Fatalf("report = %#v, want eligible status-only auto evaluation", report)
	}
}

func TestAggregatedCommitStatusFailsClosedForSharedHead(t *testing.T) {
	state, description := aggregatedCommitStatus([]Report{
		{Eligible: true, Reasons: []Reason{}},
		{Eligible: false, Reasons: []Reason{{Code: ReasonHold, Subject: labels.HoldGlobal}}},
	})
	if state != "failure" {
		t.Fatalf("state = %q, want failure when any open PR on the head is blocked", state)
	}
	if description != "Another open pull request on this commit failed Gatekeeper" {
		t.Fatalf("description = %q, want shared-head aggregation message", description)
	}
}

func TestGatekeeperCommitStatusPrefersMissingContextOverCodexReview(t *testing.T) {
	state, _ := gatekeeperCommitStatus(Report{
		Reasons: []Reason{
			{Code: ReasonGatekeeperCheckRequired, Subject: RequiredStatusContext},
			{Code: ReasonCodexReviewRequired, Subject: "current_head"},
		},
	})
	if state != "error" {
		t.Fatalf("state = %q, want error for missing required context", state)
	}
}

func TestGatekeeperCommitStatusPublishesPendingForStaleHead(t *testing.T) {
	state, _ := gatekeeperCommitStatus(Report{Reasons: []Reason{{Code: ReasonHeadStale}}})
	if state != "pending" {
		t.Fatalf("state = %q, want pending for stale head", state)
	}
}

func TestAutoGatekeeperRejectsAppBoundRequiredStatusContext(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = []githubinfra.RequiredCheckRule{
		{Context: "ci", AppID: 15368},
		{Context: RequiredStatusContext, AppID: 99999},
	}
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonGatekeeperCheckRequired) {
		t.Fatalf("report = %#v, want app-bound gatekeeper context failure", report)
	}
}

func TestDiscoverPullRequestsPublishesKnownHeadsOnAbort(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}
	mergeable := true
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{
		{Number: 41, HeadSHA: "head-a", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED", Labels: []string{labels.HoldGlobal}},
		{Number: 42, HeadSHA: "head-b", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED"},
	}
	views := 0
	fixture.github.beforeView = func(f *fakeGatekeeperGitHub) {
		views++
		switch views {
		case 1:
			f.detail = githubinfra.PullRequestDetail{
				Number: 41, State: "OPEN", HeadSHA: "head-a", BaseRefName: "main",
				ReviewDecision: "APPROVED", Labels: []string{labels.HoldGlobal},
			}
			f.mergeable = githubinfra.PullRequestDetail{Number: 41, HeadSHA: "head-a", Mergeable: &mergeable, MergeableState: "clean"}
			f.finalHeadSHA = "head-a"
		default:
			if fixture.closeDB != nil {
				_ = fixture.closeDB()
			}
			f.detail = githubinfra.PullRequestDetail{
				Number: 42, State: "OPEN", HeadSHA: "head-b", BaseRefName: "main", ReviewDecision: "APPROVED",
			}
			f.mergeable = githubinfra.PullRequestDetail{Number: 42, HeadSHA: "head-b", Mergeable: &mergeable, MergeableState: "clean"}
			f.finalHeadSHA = "head-b"
		}
	}

	result, err := fixture.autoRunner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err == nil {
		t.Fatalf("DiscoverPullRequests() error = nil, want persistence abort after the first evaluation")
	}
	if result.Evaluated != 1 || len(result.Reports) != 1 || result.Reports[0].Eligible {
		t.Fatalf("result = %#v, want one blocked report retained before abort", result)
	}
	if len(fixture.github.statusCalls) != 1 || fixture.github.statusCalls[0].SHA != "head-a" || fixture.github.statusCalls[0].State != "failure" {
		t.Fatalf("status calls = %#v, want fail-closed status for the known blocked head on abort", fixture.github.statusCalls)
	}
}

func TestDiscoverPullRequestsSkipsUnchangedCommitStatusPublication(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}
	pr := githubinfra.PullRequestSummary{
		Number: 42, HeadSHA: "head-1", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z",
		BaseRefName: "main", ReviewDecision: "APPROVED",
	}
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{pr}
	runner := fixture.autoRunner()

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.Evaluated != 1 || len(fixture.github.statusCalls) != 1 {
		t.Fatalf("first discovery = %#v status calls = %d, want one evaluation and one status write", first, len(fixture.github.statusCalls))
	}

	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.Skipped != 1 || len(fixture.github.statusCalls) != 1 {
		t.Fatalf("second discovery = %#v status calls = %d, want skip without another status write", second, len(fixture.github.statusCalls))
	}
}

func TestPublishDiscoveryCommitStatusesOnPartialDiscoveryResult(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	runner := fixture.autoRunner()
	reports := []Report{{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 41,
		ObservedHeadSHA: "head-a", ExpectedHeadSHA: "head-a",
		Reasons:  []Reason{{Code: ReasonHold, Subject: labels.HoldGlobal}},
		Evidence: Evidence{PullRequestState: "OPEN", HoldLabels: []string{labels.HoldGlobal}},
	}}
	runner.publishDiscoveryCommitStatuses(context.Background(), "project_1", "acme/looper", "", reports)
	if len(fixture.github.statusCalls) != 1 || fixture.github.statusCalls[0].SHA != "head-a" || fixture.github.statusCalls[0].State != "failure" {
		t.Fatalf("status calls = %#v, want blocked head-a published for partial discovery result", fixture.github.statusCalls)
	}
}

func TestAutoGatekeeperAuthenticatedMarkerClearsMissingReviewReason(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want authenticated marker sufficient without missing-review reason", report)
	}
}

func TestTargetedEvaluationAggregatesSharedHeadStatus(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}
	sharedSHA := "shared-head"
	fixture.github.detail.HeadSHA = sharedSHA
	fixture.github.mergeable.HeadSHA = sharedSHA
	fixture.github.finalHeadSHA = sharedSHA
	seedGateReport(t, fixture, Report{
		Version: reportVersion, Status: StatusBlocked, ProjectID: "project_1",
		Repo: "acme/looper", PRNumber: 41, ObservedHeadSHA: sharedSHA, ExpectedHeadSHA: sharedSHA,
		Reasons:     []Reason{{Code: ReasonHold, Subject: labels.HoldGlobal}},
		Evidence:    Evidence{PullRequestState: "OPEN", HoldLabels: []string{labels.HoldGlobal}},
		EvaluatedAt: fixture.now.Format(time.RFC3339Nano),
	})
	runner := fixture.autoRunner()

	_, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: sharedSHA,
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if len(fixture.github.statusCalls) != 1 || fixture.github.statusCalls[0].SHA != sharedSHA || fixture.github.statusCalls[0].State != "failure" {
		t.Fatalf("status calls = %#v, want aggregated failure for shared head", fixture.github.statusCalls)
	}
}
