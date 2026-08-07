package gatekeeper

import (
	"context"
	"testing"
	"time"
)

func TestAdviceAgreementsStayWithinTheCurrentTerminalEpoch(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := fixture.runner()
	ctx := context.Background()
	entityID := "acme/looper#42"

	firstVerdict := Report{
		Version: reportVersion, Mode: "advise", Eligible: true,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", EvaluatedAt: "2026-07-30T10:00:00Z",
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}
	appendAgreementTestReport(t, runner, entityID, "verdict-1", firstVerdict, time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC))

	firstTerminal := Report{
		Version: reportVersion, Mode: "advise", Eligible: false,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", EvaluatedAt: "2026-07-30T10:05:00Z",
		Reasons:  []Reason{{Code: ReasonPullRequestNotOpen}},
		Evidence: Evidence{PullRequestState: "CLOSED", ClosedAt: "2026-07-30T10:05:00Z", FinalObservedHeadSHA: "head-1"},
	}
	appendAgreementTestReport(t, runner, entityID, "terminal-1", firstTerminal, time.Date(2026, time.July, 30, 10, 5, 0, 0, time.UTC))
	if err := runner.recordTerminalAdviceOutcomes(ctx, firstTerminal, "terminal-1"); err != nil {
		t.Fatalf("first recordTerminalAdviceOutcomes() error = %v", err)
	}

	secondVerdict := firstVerdict
	secondVerdict.ObservedHeadSHA = "head-2"
	secondVerdict.EvaluatedAt = "2026-07-30T10:20:00Z"
	secondVerdict.Evidence = Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-2"}
	appendAgreementTestReport(t, runner, entityID, "verdict-2", secondVerdict, time.Date(2026, time.July, 30, 10, 20, 0, 0, time.UTC))

	secondTerminal := Report{
		Version: reportVersion, Mode: "advise", Eligible: true,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-2", EvaluatedAt: "2026-07-30T10:30:00Z",
		Evidence: Evidence{PullRequestState: "MERGED", MergedAt: "2026-07-30T10:30:00Z", FinalObservedHeadSHA: "head-2"},
	}
	appendAgreementTestReport(t, runner, entityID, "terminal-2", secondTerminal, time.Date(2026, time.July, 30, 10, 30, 0, 0, time.UTC))
	if err := runner.recordTerminalAdviceOutcomes(ctx, secondTerminal, "terminal-2"); err != nil {
		t.Fatalf("second recordTerminalAdviceOutcomes() error = %v", err)
	}

	events, err := fixture.repos.Events.ListByEntity(ctx, "pull_request", entityID)
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	agreementIDs := map[string]int{}
	for _, event := range events {
		if event.EventType == AdviceAgreementEventType {
			causationID := ""
			if event.CausationID != nil {
				causationID = *event.CausationID
			}
			agreementIDs[causationID]++
		}
	}
	if len(agreementIDs) != 4 {
		t.Fatalf("agreement causations = %#v, want verdict-1/terminal-1/verdict-2/terminal-2 only", agreementIDs)
	}
	if agreementIDs["verdict-1"] != 1 || agreementIDs["terminal-1"] != 1 || agreementIDs["verdict-2"] != 1 || agreementIDs["terminal-2"] != 1 {
		t.Fatalf("agreement causations = %#v, want one agreement per verdict in its terminal epoch", agreementIDs)
	}
}

func TestProjectionMarkersAreNotAdviceVerdicts(t *testing.T) {
	retry := Report{Version: reportVersion, Mode: "advise", Reasons: []Reason{{Code: ReasonRoutingProjectionFailed}}}
	if isAdviseVerdict(retry) {
		t.Fatal("routing retry marker was treated as an advice verdict")
	}
	if !previousPublished(retry) {
		t.Fatal("routing retry marker was not retained as published for cleanup recovery")
	}

	pending := retry
	pending.Reasons = nil
	pending.PendingProjection = true
	if isAdviseVerdict(pending) {
		t.Fatal("pending projection was treated as an advice verdict")
	}
	if previousPublished(pending) {
		t.Fatal("pending projection was treated as a published verdict")
	}
}

func appendAgreementTestReport(t *testing.T, runner *Runner, entityID, eventID string, report Report, createdAt time.Time) {
	t.Helper()
	if err := runner.appendGateReportAtWithID(context.Background(), report, "pull_request", entityID, createdAt, eventID); err != nil {
		t.Fatalf("append gate report %s: %v", eventID, err)
	}
}
