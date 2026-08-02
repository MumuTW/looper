package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func routingRunner(fixture *gatekeeperFixture, trust config.GatekeeperTrustLevel) *Runner {
	return New(Options{
		Repos:  fixture.repos,
		GitHub: fixture.github,
		Now:    func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool {
			return fixture.policyPermits
		},
		TrustForProject: func(string) config.GatekeeperTrustLevel {
			return trust
		},
	})
}

func evaluateRoutingReport(t *testing.T, runner *Runner) Report {
	t.Helper()
	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	return report
}

func TestAutoTrustRoutesEligiblePullRequestThroughMergify(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	report := evaluateRoutingReport(t, routingRunner(fixture, config.GatekeeperTrustAuto))

	if !report.Eligible {
		t.Fatalf("report = %#v, want eligible", report)
	}
	if len(fixture.github.labelAdds) != 1 || !slices.Equal(fixture.github.labelAdds[0].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label adds = %#v, want one %s route", fixture.github.labelAdds, labels.AutoMerge)
	}
	if len(fixture.github.labelRemoves) != 1 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removes = %#v, want stale %s removal", fixture.github.labelRemoves, labels.NeedsHumanReview)
	}
	if events, err := fixture.repos.Events.List(context.Background(), 50); err != nil {
		t.Fatalf("Events.List() error = %v", err)
	} else {
		for _, event := range events {
			if event.EventType == MergeOutcomeEventType {
				t.Fatalf("auto trust emitted obsolete direct-merge outcome: %#v", event)
			}
		}
	}
}

func TestAdviseTrustDoesNotRouteEligiblePullRequestToAutoMerge(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	report := evaluateRoutingReport(t, routingRunner(fixture, config.GatekeeperTrustAdvise))

	if !report.Eligible {
		t.Fatalf("report = %#v, want eligible", report)
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("advise label adds = %#v, want no auto-merge route", fixture.github.labelAdds)
	}
	if fixture.github.validateMergifyCalls != 0 {
		t.Fatalf("advise validated Mergify %d times, want no auto-route dependency", fixture.github.validateMergifyCalls)
	}
}

func TestEscalationReasonsRouteToNeedsHumanReview(t *testing.T) {
	for _, reason := range []ReasonCode{ReasonCode(protectedPathTouchedReason), ReasonDiffBudgetExceeded} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			fixture := newGatekeeperFixture(t)
			runner := routingRunner(fixture, config.GatekeeperTrustAdvise)
			report := Report{
				ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
				ObservedHeadSHA: "head-1", Evidence: Evidence{
					FinalObservedHeadSHA:       "head-1",
					PullRequestState:           "OPEN",
					BaseRefName:                "main",
					ReviewDecision:             "APPROVED",
					ProjectPolicyPermitsTarget: true,
				},
				Reasons: []Reason{{Code: reason}},
			}

			if _, err := runner.persist(context.Background(), report); err != nil {
				t.Fatalf("persist() error = %v", err)
			}
			if len(fixture.github.labelRemoves) != 1 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) {
				t.Fatalf("label removes = %#v, want %s removal first", fixture.github.labelRemoves, labels.AutoMerge)
			}
			if len(fixture.github.labelAdds) != 1 || !slices.Equal(fixture.github.labelAdds[0].Labels, []string{labels.NeedsHumanReview}) {
				t.Fatalf("label adds = %#v, want %s route", fixture.github.labelAdds, labels.NeedsHumanReview)
			}
		})
	}
}

func TestMechanicalBlockLeavesNoRoutingLabel(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := routingRunner(fixture, config.GatekeeperTrustAdvise)
	report := Report{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", Evidence: Evidence{FinalObservedHeadSHA: "head-1"},
		Reasons: []Reason{{Code: ReasonCheckPending}},
	}

	if _, err := runner.persist(context.Background(), report); err != nil {
		t.Fatalf("persist() error = %v", err)
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds = %#v, want no routing label", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removes = %#v, want both route labels removed", fixture.github.labelRemoves)
	}
}

func TestRepeatedReviewChangesEscalatesOnSecondEvaluation(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := routingRunner(fixture, config.GatekeeperTrustAdvise)
	report := Report{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", Evidence: Evidence{
			FinalObservedHeadSHA:       "head-1",
			PullRequestState:           "OPEN",
			BaseRefName:                "main",
			ReviewDecision:             "APPROVED",
			ProjectPolicyPermitsTarget: true,
		},
		Reasons: []Reason{{Code: ReasonReviewChangesRequested}},
	}
	if _, err := runner.persist(context.Background(), report); err != nil {
		t.Fatalf("first persist() error = %v", err)
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("first label adds = %#v, want no escalation yet", fixture.github.labelAdds)
	}
	if _, err := runner.persist(context.Background(), report); err != nil {
		t.Fatalf("second persist() error = %v", err)
	}
	if len(fixture.github.labelAdds) != 1 || !slices.Equal(fixture.github.labelAdds[0].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("second label adds = %#v, want repeated-review escalation", fixture.github.labelAdds)
	}
}

func TestEligibleThenMechanicalBlockRemovesAutoMergeWithoutEscalation(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := routingRunner(fixture, config.GatekeeperTrustAuto)
	eligible := Report{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ObservedHeadSHA: "head-1", Evidence: Evidence{
		FinalObservedHeadSHA:       "head-1",
		PullRequestState:           "OPEN",
		BaseRefName:                "main",
		ReviewDecision:             "APPROVED",
		ProjectPolicyPermitsTarget: true,
	}}
	if _, err := runner.persist(context.Background(), eligible); err != nil {
		t.Fatalf("eligible persist() error = %v", err)
	}
	blocked := eligible
	blocked.Reasons = []Reason{{Code: ReasonCheckFailed}}
	if _, err := runner.persist(context.Background(), blocked); err != nil {
		t.Fatalf("blocked persist() error = %v", err)
	}
	if len(fixture.github.labelAdds) != 1 || !slices.Equal(fixture.github.labelAdds[0].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label adds = %#v, want only the initial auto route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 3 || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label removes = %#v, want auto-merge removed on verdict flip", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsSkipWhenHeadMovesBeforeProjection(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.headSHAResponses = []string{"head-1", "head-2"}
	if _, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded after the head moved before projection")
	}

	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds across a head race = %#v, want no new route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals across a head race = %#v, want both stale routes retired", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsSkipWhenReviewStateChangesBeforeProjection(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views == 2 {
			github.detail.ReviewDecision = "CHANGES_REQUESTED"
		}
	}
	if _, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded after review state changed before projection")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds across a review-state race = %#v, want no new route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals across a review-state race = %#v, want both stale routes retired", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsSkipWhenReviewThreadAppearsBeforeProjection(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views == 2 {
			github.threads = []githubinfra.ReviewThread{{ID: "thread-new", IsResolved: false}}
		}
	}
	if _, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded after an unresolved review thread appeared before projection")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds across a review-thread race = %#v, want no new route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals across a review-thread race = %#v, want both stale routes retired", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsRecheckHeadAfterReviewThreadProjection(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	threadReads := 0
	fixture.github.beforeThreads = func(github *fakeGatekeeperGitHub) {
		threadReads++
		if threadReads == 2 {
			github.finalHeadSHA = "head-2"
		}
	}
	if _, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded after the head moved during review-thread revalidation")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds across a post-thread head race = %#v, want no new route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals across a post-thread head race = %#v, want both stale routes retired", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsSkipWhenDiffBudgetBaseMovesBeforeProjection(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.finalBaseSHA = "base-2"
	runner := routingRunner(fixture, config.GatekeeperTrustAuto)
	report := Report{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, Eligible: true,
		ObservedHeadSHA: "head-1",
		Evidence: Evidence{
			FinalObservedHeadSHA:       "head-1",
			PullRequestState:           "OPEN",
			BaseRefName:                "main",
			ReviewDecision:             "APPROVED",
			ProjectPolicyPermitsTarget: true,
			DiffBudget:                 &DiffBudgetEvidence{BaseSHA: "base-1", MaxChangedFiles: 20},
		},
	}
	if _, err := runner.persist(context.Background(), report); err == nil {
		t.Fatal("persist() succeeded after the diff-budget base moved before routing")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds across a diff-budget base race = %#v, want no new route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals across a diff-budget base race = %#v, want both stale routes retired", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsRemoveStaleRouteWhenReviewerConvergenceAdvancesBeforeProjection(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	seedReviewerConvergenceLoop(t, fixture, `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"blocking"},"state":{"totalRounds":4},"action":"complete","reason":"severity_floor_reached","status":"completed","updatedAt":"2026-07-30T10:00:00Z"},"loop":{"lastReviewedHeadSha":"head-1"}}`, 1)
	runner := routingRunner(fixture, config.GatekeeperTrustAuto)
	evaluateRoutingReport(t, runner)
	if len(fixture.github.labelAdds) != 1 || !slices.Equal(fixture.github.labelAdds[0].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("initial label adds = %#v, want auto-merge route", fixture.github.labelAdds)
	}
	var mutateErr error
	views := 0
	fixture.github.beforeView = func(_ *fakeGatekeeperGitHub) {
		views++
		if views != 2 {
			return
		}
		loop, err := fixture.repos.Loops.GetByID(context.Background(), "reviewer-loop")
		if err != nil {
			mutateErr = err
			return
		}
		metadata := `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"blocking"},"state":{"totalRounds":5,"items":{"blocking-1":{"id":"blocking-1","severity":"blocking","status":"open"}}},"action":"continue","reason":"converging","status":"active","updatedAt":"2026-07-30T10:01:00Z"},"loop":{"lastReviewedHeadSha":"head-1"}}`
		loop.MetadataJSON = &metadata
		loop.UpdatedAt = fixture.now.Add(time.Second).Format(time.RFC3339Nano)
		mutateErr = fixture.repos.Loops.Upsert(context.Background(), *loop)
	}
	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded after Reviewer convergence advanced before projection")
	}
	if mutateErr != nil {
		t.Fatalf("mutate Reviewer convergence state: %v", mutateErr)
	}
	if len(fixture.github.labelAdds) != 1 {
		t.Fatalf("routing label adds across a Reviewer convergence race = %#v, want only initial route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 3 || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[2].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("routing label cleanup across a Reviewer convergence race = %#v, want both stale routes removed", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsBindEmptyReviewEvidenceToCurrentState(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.ReviewDecision = ""
	fixture.github.protection.HasRequiredReviews = false
	fixture.github.protection.RequiredApprovingReviewCount = 0
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views == 2 {
			github.detail.ReviewDecision = "CHANGES_REQUESTED"
		}
	}
	if _, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded after an empty-to-changes-requested review transition")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds = %#v, want no route across review transition", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals across a review transition = %#v, want both stale routes retired", fixture.github.labelRemoves)
	}
}

func TestAutoTrustRequiresMergifyRoutingContract(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.validateMergifyErr = errors.New(".mergify.yml is missing")
	if _, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded without a valid Mergify routing contract")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds = %#v, want no route before dependency validation", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label removes = %#v, want stale auto-merge route retired on invalid contract", fixture.github.labelRemoves)
	}
}

func TestObserveDemotionRetriesRouteCleanupAfterProjectionFailure(t *testing.T) {
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
	if _, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial auto discovery() error = %v", err)
	}
	trust = config.GatekeeperTrustObserve
	fixture.github.labelErr = errors.New("label permission denied")
	if _, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err == nil {
		t.Fatal("demotion discovery succeeded despite route cleanup failure")
	}
	fixture.github.labelErr = nil
	second, err := runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("retry demotion discovery() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 {
		t.Fatalf("retry demotion discovery = %d evaluated / %d skipped, want 1 / 0", second.Evaluated, second.Skipped)
	}
	if len(fixture.github.labelRemoves) < 2 || !slices.Equal(fixture.github.labelRemoves[len(fixture.github.labelRemoves)-2].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label removals after retry = %#v, want auto-merge cleanup retried", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelFailureDoesNotLoseDurableReport(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.labelErr = errors.New("label permission denied")
	report, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err == nil {
		t.Fatal("EvaluatePullRequest() succeeded despite label projection failure")
	}
	if !report.Eligible {
		t.Fatalf("returned report = %#v, want the durable gate report despite projection failure", report)
	}
	events, err := fixture.repos.Events.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	found := 0
	var retryMarker Report
	for _, event := range events {
		if event.EventType == GateReportEventType {
			found++
			var candidate Report
			if err := json.Unmarshal([]byte(event.PayloadJSON), &candidate); err != nil {
				t.Fatalf("decode gate report: %v", err)
			}
			if candidate.SourceFingerprint == "" && hasReason(candidate, ReasonRoutingProjectionFailed) {
				retryMarker = candidate
			}
		}
	}
	if found != 2 {
		t.Fatalf("gate report events = %d, want 2 (durable report plus retry marker)", found)
	}
	if !hasReason(retryMarker, ReasonRoutingProjectionFailed) {
		t.Fatalf("retry marker = %#v, want a routing-projection failure reason", retryMarker)
	}
}

func TestRoutingLabelFailureStillReconcilesVerdictComment(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.labelErr = errors.New("label permission denied")
	runner := routingRunner(fixture, config.GatekeeperTrustAdvise)
	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded despite label projection failure")
	}
	// The routing projection failed, but the verdict comment lifecycle is
	// independent of the label projection: an advise project must still publish
	// its promised verdict.
	if len(fixture.github.createdBodies) != 1 || !strings.Contains(fixture.github.createdBodies[0], VerdictCommentMarker) {
		t.Fatalf("verdict comments after routing failure = %#v, want one owned verdict", fixture.github.createdBodies)
	}
}

func TestRoutingLabelFailureForcesRetryOnUnchangedDiscovery(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{{
		Number: 42, State: "OPEN", HeadSHA: "head-1", BaseRefName: "main", ReviewDecision: "APPROVED",
		UpdatedAt: "2026-07-30T09:00:00Z",
	}}
	fixture.github.labelErr = errors.New("label permission denied")
	runner := routingRunner(fixture, config.GatekeeperTrustAuto)
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err == nil {
		t.Fatal("first discovery succeeded despite label projection failure")
	}
	fixture.github.labelErr = nil
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second discovery() error = %v", err)
	}
	if second.Evaluated != 1 || second.Skipped != 0 {
		t.Fatalf("second discovery = %d evaluated / %d skipped, want retry evaluation", second.Evaluated, second.Skipped)
	}
	if len(fixture.github.labelAdds) != 1 || !slices.Equal(fixture.github.labelAdds[0].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label adds after retry = %#v, want auto-merge route", fixture.github.labelAdds)
	}
}

type perPullRequestRoutingGitHub struct {
	*fakeGatekeeperGitHub
	failPR int64
}

func (g *perPullRequestRoutingGitHub) ViewPullRequestForGatekeeper(ctx context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	detail, err := g.fakeGatekeeperGitHub.ViewPullRequestForGatekeeper(ctx, input)
	if err != nil {
		return detail, err
	}
	detail.Number = input.PRNumber
	detail.HeadSHA = fmt.Sprintf("head-%d", input.PRNumber)
	return detail, nil
}

func (g *perPullRequestRoutingGitHub) ViewPullRequestMergeWatch(ctx context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	detail, err := g.fakeGatekeeperGitHub.ViewPullRequestMergeWatch(ctx, input)
	if err != nil {
		return detail, err
	}
	detail.Number = input.PRNumber
	detail.HeadSHA = fmt.Sprintf("head-%d", input.PRNumber)
	return detail, nil
}

func (g *perPullRequestRoutingGitHub) GetPullRequestHeadSHA(_ context.Context, input githubinfra.ViewPullRequestInput) (string, error) {
	return fmt.Sprintf("head-%d", input.PRNumber), nil
}

func (g *perPullRequestRoutingGitHub) GetPullRequestHeadAndBaseSHA(_ context.Context, input githubinfra.ViewPullRequestInput) (string, string, error) {
	return fmt.Sprintf("head-%d", input.PRNumber), g.finalBaseSHA, nil
}

func (g *perPullRequestRoutingGitHub) AddPullRequestLabels(ctx context.Context, input githubinfra.PullRequestLabelsInput) error {
	if input.PRNumber == g.failPR {
		return errors.New("label permission denied for first PR")
	}
	return g.fakeGatekeeperGitHub.AddPullRequestLabels(ctx, input)
}

func TestDiscoverPullRequestsContinuesAfterRoutingFailure(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.HeadSHA = "head-42"
	fixture.github.mergeable.HeadSHA = "head-42"
	fixture.github.finalHeadSHA = "head-42"
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{
		{Number: 42, State: "OPEN", HeadSHA: "head-42", BaseRefName: "main", ReviewDecision: "APPROVED", UpdatedAt: "2026-07-30T09:00:00Z"},
		{Number: 43, State: "OPEN", HeadSHA: "head-43", BaseRefName: "main", ReviewDecision: "APPROVED", UpdatedAt: "2026-07-30T09:00:00Z"},
	}
	seedReviewerReviewEventForPR(t, fixture, "project_1", 42, "head-42", "APPROVE", "reviewer-loop", 1, true)
	seedReviewerReviewEventForPR(t, fixture, "project_1", 43, "head-43", "APPROVE", "reviewer-loop", 2, true)
	github := &perPullRequestRoutingGitHub{fakeGatekeeperGitHub: fixture.github, failPR: 42}
	runner := New(Options{
		Repos: fixture.repos, GitHub: github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
	})
	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err == nil {
		t.Fatal("DiscoverPullRequests() error = nil, want aggregated routing failure")
	}
	if result.Evaluated != 2 || result.Skipped != 0 || len(result.Reports) != 2 {
		t.Fatalf("result = %#v, want both PRs evaluated despite first routing failure", result)
	}
	if len(fixture.github.labelAdds) != 1 || fixture.github.labelAdds[0].PRNumber != 43 {
		t.Fatalf("label adds = %#v, want second PR routed after first failure", fixture.github.labelAdds)
	}
}

func TestRoutingLabelsAreReconciledAgainAfterExternalRemoval(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := routingRunner(fixture, config.GatekeeperTrustAuto)
	evaluateRoutingReport(t, runner)
	evaluateRoutingReport(t, runner)

	if len(fixture.github.labelAdds) != 2 {
		t.Fatalf("label adds = %#v, want auto-merge reapplied on each evaluation", fixture.github.labelAdds)
	}
}

func TestRoutingLabelsBlockedByDoNotMergeVetoLabel(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.Labels = []string{labels.DoNotMerge}
	report, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible {
		t.Fatalf("report = %#v, want ineligible under do-not-merge veto", report)
	}
	if !hasReason(report, ReasonDoNotMerge) {
		t.Fatalf("report reasons = %v, want %s", reasonCodes(report.Reasons), ReasonDoNotMerge)
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds = %#v, want no auto-merge route under veto", fixture.github.labelAdds)
	}
}

func TestRoutingLabelsRetireRouteWhenDoNotMergeVetoAppearsBeforeProjection(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views == 2 {
			github.detail.Labels = []string{labels.DoNotMerge}
		}
	}
	if _, err := routingRunner(fixture, config.GatekeeperTrustAuto).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil {
		t.Fatal("EvaluatePullRequest() succeeded after a do-not-merge veto appeared before projection")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds across a veto race = %#v, want no new route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals across a veto race = %#v, want both stale routes retired", fixture.github.labelRemoves)
	}
}

func TestRoutingLabelsFailClosedOnMissingPullRequestStateEvidence(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := routingRunner(fixture, config.GatekeeperTrustAuto)
	report := Report{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, Eligible: true,
		ObservedHeadSHA: "head-1",
		Evidence: Evidence{
			FinalObservedHeadSHA: "head-1",
		},
	}
	if _, err := runner.persist(context.Background(), report); err == nil {
		t.Fatal("persist() succeeded for an add plan without pull-request state evidence")
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds = %#v, want fail-closed add plan to publish no route", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 || !slices.Equal(fixture.github.labelRemoves[0].Labels, []string{labels.AutoMerge}) || !slices.Equal(fixture.github.labelRemoves[1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label removals = %#v, want both stale routes retired on fail-closed evidence", fixture.github.labelRemoves)
	}
}
