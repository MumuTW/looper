package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	for _, reason := range []ReasonCode{ReasonCode(protectedPathTouchedReason), ReasonCode(diffBudgetExceededReason)} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			fixture := newGatekeeperFixture(t)
			runner := routingRunner(fixture, config.GatekeeperTrustAdvise)
			report := Report{
				ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
				ObservedHeadSHA: "head-1", Evidence: Evidence{FinalObservedHeadSHA: "head-1"},
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
		ObservedHeadSHA: "head-1", Evidence: Evidence{FinalObservedHeadSHA: "head-1"},
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
	eligible := Report{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ObservedHeadSHA: "head-1", Evidence: Evidence{FinalObservedHeadSHA: "head-1"}}
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

	if len(fixture.github.labelAdds) != 0 || len(fixture.github.labelRemoves) != 0 {
		t.Fatalf("routing labels changed across a head race: adds=%#v removes=%#v", fixture.github.labelAdds, fixture.github.labelRemoves)
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
	if len(fixture.github.labelAdds) != 0 || len(fixture.github.labelRemoves) != 0 {
		t.Fatalf("routing labels changed across a review-state race: adds=%#v removes=%#v", fixture.github.labelAdds, fixture.github.labelRemoves)
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
		t.Fatal("label projection failure discarded the durable gate report")
	}
	if retryMarker.SourceFingerprint != "" || !hasReason(retryMarker, ReasonRoutingProjectionFailed) {
		t.Fatalf("retry marker = %#v, want empty fingerprint and routing failure reason", retryMarker)
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
