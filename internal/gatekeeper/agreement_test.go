package gatekeeper

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

func TestTerminalAdviseVerdictsRecordCausalAgreementOnce(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github,
		Now:             func() time.Time { return fixture.now },
		TrustForProject: func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAdvise },
	})
	open := Report{
		Version: reportVersion, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", EvaluatedAt: fixture.now.Format(time.RFC3339Nano),
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}
	if _, err := runner.persist(context.Background(), open); err != nil {
		t.Fatalf("persist open advise verdict: %v", err)
	}
	terminal := open
	terminal.EvaluatedAt = fixture.now.Add(time.Minute).Format(time.RFC3339Nano)
	terminal.Evidence.PullRequestState = "CLOSED"
	terminal.Evidence.MergedAt = fixture.now.Add(time.Minute).Format(time.RFC3339Nano)
	if _, err := runner.persist(context.Background(), terminal); err != nil {
		t.Fatalf("persist terminal advise verdict: %v", err)
	}

	agreements, causationIDs := adviceAgreements(t, fixture)
	if len(agreements) != 2 {
		t.Fatalf("agreements = %#v, want both the earlier and terminal advise verdicts resolved", agreements)
	}
	for index, agreement := range agreements {
		if agreement.Outcome != AdviceOutcomeMergedAsIs || !agreement.Agreement || agreement.VerdictHeadSHA != "head-1" || agreement.TerminalHeadSHA != "head-1" || agreement.TerminalAt != terminal.Evidence.MergedAt {
			t.Fatalf("agreement[%d] = %#v", index, agreement)
		}
		if agreement.VerdictEventID == "" || agreement.VerdictEventID != causationIDs[index] {
			t.Fatalf("agreement[%d] causal identity = (%q, %q)", index, agreement.VerdictEventID, causationIDs[index])
		}
	}

	if err := runner.recordTerminalAdviceOutcomes(context.Background(), terminal, causationIDs[1]); err != nil {
		t.Fatalf("replay terminal outcome: %v", err)
	}
	if again, _ := adviceAgreements(t, fixture); len(again) != 2 {
		t.Fatalf("replay duplicated agreement records: %#v", again)
	}
}

func TestClassifyAdviceOutcome(t *testing.T) {
	t.Parallel()
	eligible := Report{Eligible: true, ObservedHeadSHA: "head-1"}
	blocked := Report{Eligible: false, ObservedHeadSHA: "head-1"}
	for _, test := range []struct {
		name      string
		verdict   Report
		terminal  Report
		outcome   AdviceOutcome
		agreement bool
	}{
		{name: "merged as is", verdict: eligible, terminal: Report{Evidence: Evidence{PullRequestState: "CLOSED", MergedAt: "2026-07-30T10:01:00Z", FinalObservedHeadSHA: "head-1"}}, outcome: AdviceOutcomeMergedAsIs, agreement: true},
		{name: "merged after changes", verdict: eligible, terminal: Report{Evidence: Evidence{PullRequestState: "CLOSED", MergedAt: "2026-07-30T10:01:00Z", FinalObservedHeadSHA: "head-2"}}, outcome: AdviceOutcomeMergedAfterChange},
		{name: "blocked verdict overridden", verdict: blocked, terminal: Report{Reasons: []Reason{{Code: ReasonPullRequestNotOpen}}, Evidence: Evidence{PullRequestState: "MERGED", FinalObservedHeadSHA: "head-1"}}, outcome: AdviceOutcomeOverridden},
		{name: "blocked verdict changed then merged", verdict: Report{Eligible: false, ObservedHeadSHA: "head-1"}, terminal: Report{Evidence: Evidence{PullRequestState: "MERGED", FinalObservedHeadSHA: "head-2"}}, outcome: AdviceOutcomeMergedAfterChange},
		{name: "closed blocked verdict agrees", verdict: blocked, terminal: Report{Evidence: Evidence{PullRequestState: "CLOSED"}}, outcome: AdviceOutcomeClosed, agreement: true},
		{name: "closed eligible verdict disagrees", verdict: eligible, terminal: Report{Evidence: Evidence{PullRequestState: "CLOSED"}}, outcome: AdviceOutcomeClosed},
		{name: "held blocked verdict agrees", verdict: blocked, terminal: Report{Evidence: Evidence{PullRequestState: "OPEN", HoldLabels: []string{labels.HoldGlobal}}}, outcome: AdviceOutcomeHeld, agreement: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, agreement := classifyAdviceOutcome(test.verdict, test.terminal)
			if outcome != test.outcome || agreement != test.agreement {
				t.Fatalf("classifyAdviceOutcome() = (%q, %v), want (%q, %v)", outcome, agreement, test.outcome, test.agreement)
			}
		})
	}
}

func TestTerminalAdviceOutcomesAreProjectScoped(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github,
		Now:             func() time.Time { return fixture.now },
		TrustForProject: func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAdvise },
	})
	open := Report{
		Version: reportVersion, Mode: string(config.GatekeeperTrustAdvise), ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		Eligible: true, ObservedHeadSHA: "head-1", EvaluatedAt: fixture.now.Format(time.RFC3339Nano),
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}
	if _, err := runner.persist(context.Background(), open); err != nil {
		t.Fatalf("persist project-1 verdict: %v", err)
	}
	project2 := "project_2"
	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: project2, Name: project2, RepoPath: t.TempDir(), CreatedAt: fixture.now.Format(time.RFC3339Nano), UpdatedAt: fixture.now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("create project-2: %v", err)
	}
	entityType := "pull_request"
	entityID := "acme/looper#42"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		ID: "project-2-verdict", EventType: GateReportEventType, ProjectID: &project2,
		EntityType: &entityType, EntityID: &entityID, Payload: func() Report {
			copy := open
			copy.ProjectID = project2
			return copy
		}(), CreatedAt: fixture.now.Add(time.Second),
	}); err != nil {
		t.Fatalf("persist project-2 verdict: %v", err)
	}
	terminal := open
	terminal.Mode = string(config.GatekeeperTrustObserve)
	terminal.Evidence.PullRequestState = "CLOSED"
	terminal.Evidence.ClosedAt = fixture.now.Add(time.Minute).Format(time.RFC3339Nano)
	if err := runner.recordTerminalAdviceOutcomes(context.Background(), terminal, "terminal-project-1"); err != nil {
		t.Fatalf("record terminal outcomes: %v", err)
	}
	agreements, _ := adviceAgreements(t, fixture)
	if len(agreements) != 1 || agreements[0].ProjectID != "project_1" {
		t.Fatalf("agreements = %#v, want only project-1 outcome", agreements)
	}
}

func TestTerminalAdviceOutcomesUseAClosureEpoch(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := New(Options{Repos: fixture.repos, Now: func() time.Time { return fixture.now }})
	verdict := Report{Version: reportVersion, Mode: string(config.GatekeeperTrustAdvise), ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, Eligible: true, ObservedHeadSHA: "head-1"}
	closed := Report{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, Evidence: Evidence{PullRequestState: "CLOSED", ClosedAt: "2026-07-30T10:01:00Z", FinalObservedHeadSHA: "head-1"}}
	merged := Report{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, Evidence: Evidence{PullRequestState: "MERGED", MergedAt: "2026-07-30T10:02:00Z", FinalObservedHeadSHA: "head-1"}}
	if err := runner.appendAdviceAgreement(context.Background(), closed, "verdict-1", verdict); err != nil {
		t.Fatalf("record closed epoch: %v", err)
	}
	if err := runner.appendAdviceAgreement(context.Background(), merged, "verdict-1", verdict); err != nil {
		t.Fatalf("record merged epoch: %v", err)
	}
	agreements, _ := adviceAgreements(t, fixture)
	if len(agreements) != 2 || agreements[0].TerminalEpoch == agreements[1].TerminalEpoch || agreements[0].Outcome != AdviceOutcomeClosed || agreements[1].Outcome != AdviceOutcomeMergedAsIs {
		t.Fatalf("agreements = %#v, want distinct closed and merged epochs", agreements)
	}
}

func TestTerminalAdvicePersistenceRollsBackReportAndAgreementTogether(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github,
		Now:             func() time.Time { return fixture.now },
		TrustForProject: func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAdvise },
	})
	open := Report{
		Version: reportVersion, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", EvaluatedAt: fixture.now.Format(time.RFC3339Nano),
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}
	if _, err := runner.persist(context.Background(), open); err != nil {
		t.Fatalf("persist open verdict: %v", err)
	}
	events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("list open events: %v", err)
	}
	var openVerdictID string
	for _, event := range events {
		if event.EventType == GateReportEventType {
			openVerdictID = event.ID
		}
	}
	if openVerdictID == "" {
		t.Fatal("open verdict event was not persisted")
	}
	terminal := open
	terminal.Evidence.PullRequestState = "MERGED"
	terminal.Evidence.MergedAt = fixture.now.Add(time.Minute).Format(time.RFC3339Nano)
	terminal.Evidence.FinalObservedHeadSHA = "head-1"
	entityType := "pull_request"
	entityID := "acme/looper#42"
	projectID := "project_1"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		ID: agreementEventID(openVerdictID, terminal), EventType: "test.agreement_id_conflict", ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, Payload: map[string]string{"reason": "force rollback"}, CreatedAt: fixture.now,
	}); err != nil {
		t.Fatalf("seed agreement ID conflict: %v", err)
	}
	if _, err := runner.persist(context.Background(), terminal); err == nil {
		t.Fatal("persist terminal verdict succeeded despite agreement append conflict")
	}
	events, err = fixture.repos.Events.ListByEntity(context.Background(), "pull_request", entityID)
	if err != nil {
		t.Fatalf("list rolled-back events: %v", err)
	}
	gateReports := 0
	agreements := 0
	for _, event := range events {
		switch event.EventType {
		case GateReportEventType:
			gateReports++
		case AdviceAgreementEventType:
			agreements++
		}
	}
	if gateReports != 1 || agreements != 0 {
		t.Fatalf("event counts after rollback = gate reports %d, agreements %d; want 1, 0", gateReports, agreements)
	}
}

func adviceAgreements(t *testing.T, fixture *gatekeeperFixture) ([]AdviceAgreement, []string) {
	t.Helper()
	events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	agreements := []AdviceAgreement{}
	causationIDs := []string{}
	for _, event := range events {
		if event.EventType != AdviceAgreementEventType {
			continue
		}
		var agreement AdviceAgreement
		if err := json.Unmarshal([]byte(event.PayloadJSON), &agreement); err != nil {
			t.Fatalf("decode agreement event: %v", err)
		}
		if event.CausationID == nil {
			t.Fatal("agreement event missing causation id")
		}
		agreements = append(agreements, agreement)
		causationIDs = append(causationIDs, *event.CausationID)
	}
	return agreements, causationIDs
}
