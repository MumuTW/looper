package gatekeeper

import (
	"context"
	"reflect"
	"testing"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
)

func TestEvaluatePullRequestBlocksOnReviewerConvergenceFloor(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":7,"consecutiveUnproductive":2,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"open"},"nit-1":{"id":"nit-1","severity":"nit","status":"open"}}},"action":"continue","reason":"converging","status":"active"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want convergence blocker", report)
	}
	if report.Evidence.ReviewerConvergence == nil {
		t.Fatal("report has no reviewer convergence evidence")
	}
	evidence := report.Evidence.ReviewerConvergence
	if evidence.TotalRounds != 7 || evidence.ConsecutiveUnproductive != 2 || !reflect.DeepEqual(evidence.OpenItemIDs, []string{"blocking-1"}) {
		t.Fatalf("convergence evidence = %#v, want floor-qualified state", evidence)
	}
}

func TestEvaluatePullRequestConvergenceFloorIgnoresBelowFloorItems(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"blocking"},"state":{"totalRounds":2,"items":{"nit-1":{"id":"nit-1","severity":"nit","status":"open"}}},"action":"complete","reason":"severity_floor_reached","status":"completed"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want eligible below blocking floor", report)
	}
	if report.Evidence.ReviewerConvergence == nil || len(report.Evidence.ReviewerConvergence.OpenItemIDs) != 0 {
		t.Fatalf("convergence evidence = %#v, want no floor-qualified open items", report.Evidence.ReviewerConvergence)
	}
}

func TestEvaluatePullRequestBlocksAwaitingHumanConvergence(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"blocking"},"state":{"totalRounds":4},"action":"escalate","reason":"absolute_round_ceiling","status":"awaiting_human"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want awaiting-human blocker", report)
	}
}

func TestEvaluatePullRequestUsesNewestReviewerLoopMetadata(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":3,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"open"}}},"action":"escalate","reason":"stalled","status":"awaiting_human"}}`, 1)
	// The newer loop is terminal without metadata: it can no longer deliver a
	// decision, so its absence hides the stale blocker rather than blocking
	// forever. An active newer loop (queued/running) is pending instead — see
	// TestEvaluatePullRequestBlocksOnQueuedReviewerLoop.
	seedReviewerLoopWithStatus(t, fixture, `{}`, 2, "failed")

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || report.Evidence.ReviewerConvergence != nil || hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want newer metadata to replace stale blocker", report)
	}
}

func TestDiscoverPullRequestsReevaluatesBlockedConvergence(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []github.PullRequestSummary{{Number: 42, HeadSHA: "head-1", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED"}}
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":3,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"open"}}},"action":"continue","reason":"converging","status":"active"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)

	first, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.Evaluated != 1 || first.Skipped != 0 || first.Reports[0].Eligible {
		t.Fatalf("first discovery = %#v, want evaluated blocker", first)
	}

	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":4,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"resolved"}}},"action":"complete","reason":"severity_floor_reached","status":"completed"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)
	second, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 || !second.Reports[0].Eligible {
		t.Fatalf("second discovery = %#v, want reevaluated eligible report", second)
	}
}

// A Reviewer run that has started but not yet recorded its first round
// persists a convergence policy with an empty state and no action. Zero known
// open items must not be read as convergence: the absence of an authoritative
// ActionComplete decision is pending, so auto mode cannot merge while the
// Reviewer agent is still running.
func TestEvaluatePullRequestBlocksConvergenceBeforeFirstRound(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":0},"status":"active","updatedAt":"2026-07-30T10:00:00Z"}}`, 1)

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want convergence blocker before first round", report)
	}
	if report.Evidence.ReviewerConvergence == nil || report.Evidence.ReviewerConvergence.Action != "" {
		t.Fatalf("convergence evidence = %#v, want empty action (no authoritative decision yet)", report.Evidence.ReviewerConvergence)
	}
}

// After the Reviewer converges on head A, its loop retains that convergence
// state while recording the reviewed commit as lastReviewedHeadSha. If head B
// is pushed before the follow-up review finishes, the PR SHA changed but the
// persisted clean decision still belongs to A; Gatekeeper must fail closed for
// head B rather than authorizing an unreviewed head.
func TestEvaluatePullRequestBlocksConvergenceForStaleReviewedHead(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":4,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"resolved"}}},"action":"complete","reason":"severity_floor_reached","status":"active","updatedAt":"2026-07-30T10:00:00Z"},"loop":{"lastReviewedHeadSha":"head-A"}}`, 1)

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want convergence blocker for unreviewed head", report)
	}
	if report.Evidence.ReviewerConvergence == nil || !report.Evidence.ReviewerConvergence.HeadStale {
		t.Fatalf("convergence evidence = %#v, want headStale against observed head-1", report.Evidence.ReviewerConvergence)
	}
}

// The same clean decision is authoritative when the observed head matches the
// reviewed head, so a converged review does not block the head it was reached
// on.
func TestEvaluatePullRequestConvergesForMatchingReviewedHead(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":4,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"resolved"}}},"action":"complete","reason":"severity_floor_reached","status":"active","updatedAt":"2026-07-30T10:00:00Z"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want eligible for the reviewed head", report)
	}
}

func seedReviewerConvergenceLoop(t *testing.T, fixture *gatekeeperFixture, metadata string, seq int64) {
	seedReviewerLoopWithStatus(t, fixture, metadata, seq, "running")
}

func seedReviewerLoopWithStatus(t *testing.T, fixture *gatekeeperFixture, metadata string, seq int64, status string) {
	t.Helper()
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "acme/looper#42"
	nowISO := fixture.now.Format("2006-01-02T15:04:05.999999999Z07:00")
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: "reviewer-loop", Seq: seq, ProjectID: "project_1", Type: string(domain.LoopTypeReviewer),
		TargetType: string(domain.LoopTargetTypePullRequest), TargetID: &targetID, Repo: &repo, PRNumber: &prNumber,
		Status: status, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
}

// A Reviewer loop queued for this PR that has not started running has no
// convergence metadata yet; the review is pending, so the gate — including a
// confirming auto pass — must block instead of merging before the Reviewer
// executes.
func TestEvaluatePullRequestBlocksOnQueuedReviewerLoop(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerLoopWithStatus(t, fixture, `{}`, 1, "queued")

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1", Confirming: true,
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want pending reviewer loop to block the confirming pass", report)
	}
	if report.Evidence.ReviewerConvergence == nil || !report.Evidence.ReviewerConvergence.PendingStart {
		t.Fatalf("convergence evidence = %#v, want synthesized pending-start evidence", report.Evidence.ReviewerConvergence)
	}
	for _, reason := range report.Reasons {
		if reason.Code == ReasonReviewerConvergence && reason.Subject != "loop_pending_start" {
			t.Fatalf("convergence subject = %q, want loop_pending_start", reason.Subject)
		}
	}
}

// A terminal reviewer loop that never wrote convergence metadata hides older
// metadata (hide-stale rule) and produces no block: it can no longer deliver a
// decision, and a pending block would never resolve.
func TestEvaluatePullRequestTerminalReviewerLoopWithoutMetadataDoesNotBlock(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerLoopWithStatus(t, fixture, `{}`, 1, "failed")

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || hasReason(report, ReasonReviewerConvergence) {
		t.Fatalf("report = %#v, want terminal loop without metadata to hide stale state without blocking", report)
	}
}

// The unchanged path must reuse a pending-start report while the reviewer
// stays queued, then re-evaluate as soon as the loop writes its first decision.
func TestConvergenceRevisionsTreatQueuedLoopAsPending(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerLoopWithStatus(t, fixture, `{}`, 1, "queued")

	revisions, err := latestConvergenceRevisions(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestConvergenceRevisions() error = %v", err)
	}
	pending, ok := revisions["acme/looper#42"]
	if !ok {
		t.Fatal("queued reviewer loop absent from convergence revisions, want pending revision")
	}
	if pending != convergenceRevision(ReviewerConvergenceEvidence{PendingStart: true}) {
		t.Fatalf("pending revision = %q, want synthesized pending revision", pending)
	}

	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"blocking"},"state":{"totalRounds":1},"action":"complete","reason":"severity_floor_reached","status":"completed"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)
	revisions, err = latestConvergenceRevisions(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestConvergenceRevisions() after start error = %v", err)
	}
	if revisions["acme/looper#42"] == pending {
		t.Fatal("first persisted convergence decision did not change the revision, want unchanged-path invalidation")
	}
}
