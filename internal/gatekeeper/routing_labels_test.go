package gatekeeper

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
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
	runner := routingRunner(fixture, config.GatekeeperTrustAdvise)
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
	evaluateRoutingReport(t, routingRunner(fixture, config.GatekeeperTrustAdvise))

	if len(fixture.github.labelAdds) != 0 || len(fixture.github.labelRemoves) != 0 {
		t.Fatalf("routing labels changed across a head race: adds=%#v removes=%#v", fixture.github.labelAdds, fixture.github.labelRemoves)
	}
}

func TestRoutingLabelFailureDoesNotLoseDurableReport(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.labelErr = errors.New("label permission denied")
	report := evaluateRoutingReport(t, routingRunner(fixture, config.GatekeeperTrustAuto))

	if !report.Eligible {
		t.Fatalf("report = %#v, want eligible despite projection failure", report)
	}
	events, err := fixture.repos.Events.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	found := false
	for _, event := range events {
		if event.EventType == GateReportEventType {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("label projection failure discarded the durable gate report")
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
