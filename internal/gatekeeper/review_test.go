package gatekeeper

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
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

func TestEvaluatePullRequestRejectsBlockingCommentReview(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	seedReviewerReviewEventWithOutcome(t, fixture, "head-1", "COMMENT", "blocking", "reviewer-loop", 1)

	report, err := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }}).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want blocking COMMENT excluded from merge-eligible review evidence", report)
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

// Project IDs must be compared exactly, without trimming. A project renamed
// from "foo" to the valid legacy ID " foo " retains old event rows in storage;
// trimming would treat them as identical and let a review written under the
// old project authorize Gatekeeper under the new one, violating the
// same-project authority contract that EvaluatePullRequest preserves.
func TestEvaluatePullRequestComparesProjectIDsExactly(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	// Create both the current project "foo" and the legacy project " foo "
	// (with surrounding spaces) so events and gate reports can be stored.
	nowISO := fixture.now.Format(time.RFC3339Nano)
	for _, pid := range []string{"foo", " foo "} {
		if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
			ID: pid, Name: "Project " + pid, RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Projects.Upsert(%q) error = %v", pid, err)
		}
	}
	// Seed a marker-verified review event under " foo " (with surrounding
	// spaces, a valid legacy project ID that trims to "foo").
	seedReviewerReviewEventWithProjectID(t, fixture, " foo ", "head-1", "APPROVE", "reviewer-loop", 1, true)

	// Gatekeeper evaluates under "foo" (without spaces). The event must not
	// satisfy the gate despite trimming to the same value.
	report, err := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }}).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "foo", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want exact project ID mismatch rejected", report)
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
	seedReviewerReviewEventWithProjectID(t, fixture, "project_1", headSHA, reviewEvent, actorID, ordinal, markerVerified)
}

func seedReviewerReviewEventWithProjectID(t *testing.T, fixture *gatekeeperFixture, projectID, headSHA, reviewEvent, actorID string, ordinal int, markerVerified bool) {
	t.Helper()
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

func seedReviewerReviewEventWithOutcome(t *testing.T, fixture *gatekeeperFixture, headSHA, reviewEvent, outcome, actorID string, ordinal int) {
	t.Helper()
	projectID := "project_1"
	entityType := "pull_request"
	entityID := "acme/looper#42"
	actorType := "system"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		ID: fmt.Sprintf("review-posted-outcome-%d", ordinal), EventType: reviewerReviewPostedEventType,
		ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		ActorType: &actorType, ActorID: &actorID,
		Payload:   map[string]any{"repo": "acme/looper", "prNumber": int64(42), "event": reviewEvent, "outcome": outcome, "headSha": headSHA, "markerVerified": true},
		CreatedAt: fixture.now.Add(time.Duration(ordinal) * time.Second),
	}); err != nil {
		t.Fatalf("append reviewer review event: %v", err)
	}
}

// seedGateReport appends a gate report event so discovery's latestGateReports
// picks it up as the previous report for the pull request.
func seedGateReport(t *testing.T, fixture *gatekeeperFixture, report Report) {
	t.Helper()
	projectID := "project_1"
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", report.Repo, report.PRNumber)
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		ID: fmt.Sprintf("gate-report-%d", time.Now().UnixNano()), EventType: GateReportEventType,
		ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: report, CreatedAt: fixture.now,
	}); err != nil {
		t.Fatalf("append gate report: %v", err)
	}
}

// A codex_review provider block (transient event-store read failure) must
// persist an invalid CodexReview placeholder so reportAwaitsCurrentHeadReview
// recognises the report as waiting on review evidence. Without the placeholder
// the report carries only the provider reason and a nil projection, which
// neither signal detects, so unchanged discovery would reuse the failed report
// for up to maxSkipAge even after the review event appears.
func TestDiscoverPullRequestsReevaluatesAfterCodexReviewProviderBlock(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	pr := githubinfra.PullRequestSummary{Number: 42, HeadSHA: "head-1", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED"}
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{pr}
	runner := New(Options{Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now }})

	// Seed a provider-block report matching the shape EvaluatePullRequest now
	// produces when the codex_review read fails: the provider reason plus an
	// invalid CodexReview placeholder (CurrentHeadValid=false).
	fingerprint := sourceFingerprint(pr, false, nil) + "\x1fdiff-budget=0,0" + "\x1fgatekeeper-trust=observe" + "\x1fconfigured-target=" + "\x1fpolicy-permits=true" + "\x1freview-threshold=200"
	seedGateReport(t, fixture, Report{
		Version: reportVersion, Status: StatusBlocked, ProjectID: "project_1",
		Mode: string(config.GatekeeperTrustObserve),
		Repo: "acme/looper", PRNumber: 42, ObservedHeadSHA: "head-1",
		ExpectedHeadSHA: "head-1", SourceFingerprint: fingerprint,
		Reasons: []Reason{{Code: ReasonProviderStateUnavailable, Subject: "codex_review"}},
		Evidence: Evidence{
			CodexReview: &CodexReviewEvidence{RequiredHeadSHA: "head-1", CurrentHeadValid: false},
		},
		EvaluatedAt: fixture.now.Format(time.RFC3339Nano),
	})

	// While no review event has appeared, discovery skips cheaply (no forge
	// round trips) because re-evaluating would reach the same conclusion.
	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.Evaluated != 0 || first.Skipped != 1 {
		t.Fatalf("first discovery = %d evaluated / %d skipped, want 0 / 1 (skip while waiting for review)", first.Evaluated, first.Skipped)
	}

	// Once the durable review event appears, discovery must re-evaluate rather
	// than reuse the provider-block report for up to maxSkipAge.
	seedReviewerReviewEvent(t, fixture, "head-1", "APPROVE", "reviewer-loop", 1)
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 || !second.Reports[0].Eligible {
		t.Fatalf("second discovery = %#v, want re-evaluated eligible report", second)
	}
}

// A provider-block report with a nil CodexReview projection (the pre-fix shape)
// is not recognised as awaiting review. This documents why the placeholder is
// necessary: without it, the report is cached for up to maxSkipAge.
func TestReportAwaitsCurrentHeadReviewMissesNilProjectionProviderBlock(t *testing.T) {
	report := Report{
		Reasons:  []Reason{{Code: ReasonProviderStateUnavailable, Subject: "codex_review"}},
		Evidence: Evidence{CodexReview: nil},
	}
	if reportAwaitsCurrentHeadReview(report) {
		t.Fatalf("reportAwaitsCurrentHeadReview() = true for nil-projection provider block, want false (placeholder required)")
	}
}
