package gatekeeper

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
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
		{name: "blocked verdict overridden", verdict: blocked, terminal: Report{Evidence: Evidence{PullRequestState: "MERGED", FinalObservedHeadSHA: "head-1"}}, outcome: AdviceOutcomeOverridden},
		{name: "closed on hold", verdict: eligible, terminal: Report{Evidence: Evidence{PullRequestState: "CLOSED", HoldLabels: []string{"looper:hold"}}}, outcome: AdviceOutcomeHeld},
		{name: "closed", verdict: eligible, terminal: Report{Evidence: Evidence{PullRequestState: "CLOSED"}}, outcome: AdviceOutcomeClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, agreement := classifyAdviceOutcome(test.verdict, test.terminal)
			if outcome != test.outcome || agreement != test.agreement {
				t.Fatalf("classifyAdviceOutcome() = (%q, %v), want (%q, %v)", outcome, agreement, test.outcome, test.agreement)
			}
		})
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
