package dispatch

import (
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/labels"
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
