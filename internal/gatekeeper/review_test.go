package gatekeeper

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

func TestEvaluatePullRequestRequiresCodexReviewForCurrentHead(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	report, err := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }}).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want current-head review blocker", report)
	}
	if report.Evidence.CodexReview == nil || report.Evidence.CodexReview.RequiredHeadSHA != "head-1" || report.Evidence.CodexReview.CurrentHeadValid {
		t.Fatalf("Codex review evidence = %#v, want missing current-head review", report.Evidence.CodexReview)
	}
}

func TestEvaluatePullRequestAcceptsDurableCodexReviewForCurrentHead(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	seedReviewerReviewEvent(t, fixture, "head-1", "APPROVE", "reviewer-loop", 1)

	report, err := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }}).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want eligible with current-head review", report)
	}
	evidence := report.Evidence.CodexReview
	if evidence == nil || !evidence.CurrentHeadValid || evidence.Event != "APPROVE" || evidence.ReviewedHeadSHA != "head-1" {
		t.Fatalf("Codex review evidence = %#v, want verified current-head review", evidence)
	}
}

func TestEvaluatePullRequestRejectsStaleCodexReview(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	seedReviewerReviewEvent(t, fixture, "old-head", "COMMENT", "reviewer-loop", 1)

	report, err := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }}).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want stale-review blocker", report)
	}
	if got := report.Evidence.CodexReview.ReviewedHeadSHA; got != "old-head" {
		t.Fatalf("reviewed head = %q, want old-head", got)
	}
}

func TestEvaluatePullRequestIgnoresNonReviewerReviewEvent(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	seedReviewerReviewEvent(t, fixture, "head-1", "APPROVE", "human", 1)

	report, err := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }}).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want Reviewer-authority blocker", report)
	}
}

func TestDiscoverPullRequestsReevaluatesMissingCodexReview(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{{Number: 42, HeadSHA: "head-1", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED"}}
	runner := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }})

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.Evaluated != 1 || first.Skipped != 0 || first.Reports[0].Eligible {
		t.Fatalf("first discovery = %#v, want missing-review blocker", first)
	}

	seedReviewerReviewEvent(t, fixture, "head-1", "COMMENT", "reviewer-loop", 2)
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 || !second.Reports[0].Eligible {
		t.Fatalf("second discovery = %#v, want re-evaluated eligible report", second)
	}
}

// A markerless COMMENT event (clean no-op) records that the Reviewer processed
// the head without publishing a structured GitHub review, so it must not satisfy
// the current-head gate. Only a marker-verified event proves a structured
// review was published.
func TestEvaluatePullRequestRejectsMarkerlessReviewEvent(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	seedReviewerReviewEventWithMarkerVerified(t, fixture, "head-1", "COMMENT", "reviewer-loop", 1, false)

	report, err := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }}).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want markerless event rejected", report)
	}
}

// While a pull request is waiting for a current-head review event that has not
// yet appeared in the local event log, discovery must skip it cheaply rather
// than re-evaluating every tick. This is the rate-limit protection for
// repositories with many out-of-scope pull requests that may never receive a
// Reviewer review.
func TestDiscoverPullRequestsSkipsWhileWaitingForCodexReview(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{{Number: 42, HeadSHA: "head-1", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED"}}
	runner := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }})

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.Evaluated != 1 || first.Skipped != 0 || first.Reports[0].Eligible {
		t.Fatalf("first discovery = %#v, want missing-review blocker", first)
	}
	callsAfterFirst := fixture.github.perPullRequestCalls

	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.Evaluated != 0 || second.Skipped != 1 {
		t.Fatalf("second discovery = %d evaluated / %d skipped, want 0 / 1 (skip while waiting for review)", second.Evaluated, second.Skipped)
	}
	if fixture.github.perPullRequestCalls != callsAfterFirst {
		t.Fatalf("per-pull-request calls = %d, want %d (skip must make no forge calls)",
			fixture.github.perPullRequestCalls, callsAfterFirst)
	}
}

func seedReviewerReviewEvent(t *testing.T, fixture *gatekeeperFixture, headSHA, reviewEvent, actorID string, ordinal int) {
	t.Helper()
	seedReviewerReviewEventWithMarkerVerified(t, fixture, headSHA, reviewEvent, actorID, ordinal, true)
}

func seedReviewerReviewEventWithMarkerVerified(t *testing.T, fixture *gatekeeperFixture, headSHA, reviewEvent, actorID string, ordinal int, markerVerified bool) {
	t.Helper()
	projectID := "project_1"
	entityType := "pull_request"
	entityID := "acme/looper#42"
	actorType := "system"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		ID: fmt.Sprintf("review-posted-%d", ordinal), EventType: reviewerReviewPostedEventType,
		ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		ActorType: &actorType, ActorID: &actorID,
		Payload:   map[string]any{"repo": "acme/looper", "prNumber": int64(42), "event": reviewEvent, "headSha": headSHA, "markerVerified": markerVerified},
		CreatedAt: fixture.now.Add(time.Duration(ordinal) * time.Second),
	}); err != nil {
		t.Fatalf("append reviewer review event: %v", err)
	}
}
