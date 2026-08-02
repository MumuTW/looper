package gatekeeper

import (
	"context"
	"errors"
	"testing"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
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
	if first.Evaluated != 1 || first.Reports[0].SourceFingerprint != "" {
		t.Fatalf("first discovery = %#v, want evaluated report with cleared fingerprint after status failure", first)
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
