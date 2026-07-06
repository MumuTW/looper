package dispatch

import (
	"testing"
	"time"
)

func TestAutoLabelDispatchesWithoutHumanCommandUnderHumanGatedMode(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	// looper:auto on a triaged, plan-dispatched issue proceeds on its own even under
	// the human-gated coordinator and with NO /plan comment — the label IS the opt-in.
	action := Decide(Issue{
		Number:    1,
		Labels:    []string{AutoLabel, "triaged", DispatchPlan},
		TriagedAt: now.Add(-time.Minute),
	}, testConfig(), now, nil)
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:plan" || action.AssignTo != "octocat" {
		t.Fatalf("action = %#v, want autonomous planner dispatch", action)
	}
}

func TestAutoLabelIgnoresTheAutonomousCooloff(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	// Even freshly triaged (well within the 30m cool-off that gates fully-autonomous
	// mode), looper:auto dispatches immediately — the human already committed.
	action := Decide(Issue{
		Number:    1,
		Labels:    []string{AutoLabel, "triaged", DispatchImplement},
		TriagedAt: now.Add(-1 * time.Second),
	}, testConfig(), now, nil)
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != "looper:worker-ready" {
		t.Fatalf("action = %#v, want immediate worker dispatch", action)
	}
}

func TestWithoutAutoLabelHumanGatedStillNeedsCommand(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	// The same issue WITHOUT looper:auto is a no-op under human-gated mode until a
	// /plan or /implement command arrives — the opt-in must be explicit.
	action := Decide(Issue{
		Number:    1,
		Labels:    []string{"triaged", DispatchPlan},
		TriagedAt: now.Add(-time.Minute),
	}, testConfig(), now, nil)
	if !action.NoOp || len(action.TriggerLabels) != 0 {
		t.Fatalf("action = %#v, want no-op without looper:auto or a command", action)
	}
}

func TestAutoLabelStillRespectsTriageAndHold(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	// Not yet triaged → looper:auto waits (nothing to dispatch to).
	if a := Decide(Issue{Number: 1, Labels: []string{AutoLabel, DispatchPlan}, TriagedAt: now.Add(-time.Minute)}, testConfig(), now, nil); !a.NoOp {
		t.Fatalf("untriaged looper:auto = %#v, want no-op", a)
	}
	// On hold → looper:auto still holds.
	if a := Decide(Issue{Number: 1, Labels: []string{AutoLabel, "triaged", DispatchPlan, "looper:hold"}, TriagedAt: now.Add(-time.Minute)}, testConfig(), now, nil); !a.NoOp {
		t.Fatalf("held looper:auto = %#v, want no-op", a)
	}
}
