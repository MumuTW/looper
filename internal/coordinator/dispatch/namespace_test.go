package dispatch

import (
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/labels"
)

func TestCustomNamespaceDispatchReadsOwnLabelAndWritesOwnTrigger(t *testing.T) {
	namespace := labels.NewNamespace("team.looper:")
	cfg := Config{Mode: ModeHumanGated, TriagedLabel: "triaged", Namespace: namespace, PlannerTriggerLabels: []string{namespace.PlanTrigger()}, SlashCommands: []string{"/plan"}}
	action := Decide(Issue{Number: 1, Labels: []string{"triaged", namespace.DispatchPlan()}, Comments: []Comment{{ID: 1, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, cfg, time.Now(), nil)
	if len(action.TriggerLabels) != 1 || action.TriggerLabels[0] != namespace.PlanTrigger() {
		t.Fatalf("custom dispatch action = %#v, want namespaced trigger", action)
	}
	foreign := Decide(Issue{Number: 2, Labels: []string{"triaged", "dispatch/plan"}, Comments: []Comment{{ID: 2, Author: "octo", HasWriteAccess: true, Body: "/plan"}}}, cfg, time.Now(), nil)
	if !foreign.NoOp || foreign.ReactionContent != ReactionFailure {
		t.Fatalf("foreign dispatch action = %#v, want fail-closed no-op", foreign)
	}
}

func TestCustomNamespaceAutonomousDispatchRejectsBareLegacyLabel(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	namespace := labels.NewNamespace("team.looper:")
	cfg := Config{
		Mode:                 ModeAutonomous,
		TriagedLabel:         "team.looper:triaged",
		Namespace:            namespace,
		AutonomousDelay:      time.Minute,
		PlannerTriggerLabels: []string{namespace.PlanTrigger()},
		WorkerTriggerLabels:  []string{namespace.WorkerReadyTrigger()},
	}
	action := Decide(Issue{Number: 3, Labels: []string{cfg.TriagedLabel, "dispatch/plan"}, TriagedAt: now.Add(-2 * time.Minute)}, cfg, now, nil)
	if !action.NoOp || len(action.TriggerLabels) != 0 {
		t.Fatalf("bare custom dispatch action = %#v, want no-op", action)
	}
}
