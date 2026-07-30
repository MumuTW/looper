package convergence

import (
	"fmt"
	"testing"

	"github.com/nexu-io/looper/internal/reviewitem"
)

func TestLongProductiveHistoryKeepsRunning(t *testing.T) {
	policy := DefaultPolicy()
	state := State{}
	for round := 1; round <= 10; round++ {
		decision, err := Evaluate(state, Round{Items: []Item{{
			ID:       fmt.Sprintf("item-%02d", round),
			Severity: reviewitem.SeverityNonBlocking,
			Status:   ItemStatusOpen,
		}}}, policy)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != ActionContinue || !decision.Productive {
			t.Fatalf("round %d decision = %#v, want productive continuation", round, decision)
		}
		state = decision.State
	}
	if state.TotalRounds != 10 || state.ConsecutiveUnproductive != 0 {
		t.Fatalf("state = %#v, want ten productive rounds", state)
	}
}

func TestStallRequiresExplicitUnchangedOpenArtifacts(t *testing.T) {
	policy := DefaultPolicy()
	first, err := Evaluate(State{}, Round{Items: []Item{{ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen}}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	state := first.State
	for round := 1; round <= policy.MaxConsecutiveUnproductive; round++ {
		decision, err := Evaluate(state, Round{Items: []Item{{ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen}}}, policy)
		if err != nil {
			t.Fatal(err)
		}
		if round < policy.MaxConsecutiveUnproductive && decision.Action != ActionContinue {
			t.Fatalf("unproductive round %d action = %s, want continue", round, decision.Action)
		}
		if round == policy.MaxConsecutiveUnproductive && (decision.Action != ActionEscalate || decision.Reason != ReasonStalled) {
			t.Fatalf("terminal decision = %#v, want stalled escalation", decision)
		}
		state = decision.State
	}
}

func TestMissingItemDoesNotManufactureResolution(t *testing.T) {
	policy := DefaultPolicy()
	first, err := Evaluate(State{}, Round{Items: []Item{{ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen}}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := Evaluate(first.State, Round{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Productive || decision.Action != ActionContinue || decision.State.Items["item-1"].Status != ItemStatusOpen {
		t.Fatalf("decision = %#v, missing capture must preserve open item", decision)
	}
}

func TestExplicitResolutionResetsUnproductiveCounter(t *testing.T) {
	policy := DefaultPolicy()
	state := State{ConsecutiveUnproductive: 2, Items: map[string]Item{
		"item-1": {ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
		"item-2": {ID: "item-2", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
	}}
	decision, err := Evaluate(state, Round{Items: []Item{{ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusResolved}}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Productive || decision.State.ConsecutiveUnproductive != 0 || decision.Action != ActionContinue {
		t.Fatalf("decision = %#v, want productive reset with item-2 still open", decision)
	}
}

func TestStuckItemDefersWithoutHoldingRemainingItems(t *testing.T) {
	policy := DefaultPolicy()
	policy.MaxFixerAttemptsPerItem = 2
	state := State{Items: map[string]Item{
		"stuck":     {ID: "stuck", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen, FixerAttempts: 1},
		"remaining": {ID: "remaining", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
	}}
	decision, err := Evaluate(state, Round{Items: []Item{
		{ID: "stuck", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen, FixerResult: FixerResultDeclined},
		{ID: "remaining", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
	}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	stuck := decision.State.Items["stuck"]
	if stuck.Status != ItemStatusDeferred || !stuck.Stuck || decision.Action != ActionContinue {
		t.Fatalf("decision = %#v, want stuck item deferred and remaining item continued", decision)
	}
}

func TestStuckItemStaysDeferredWhileForgeThreadRemainsOpen(t *testing.T) {
	policy := DefaultPolicy()
	state := State{Items: map[string]Item{
		"stuck":     {ID: "stuck", Severity: reviewitem.SeverityBlocking, Status: ItemStatusDeferred, FixerAttempts: 4, Stuck: true},
		"remaining": {ID: "remaining", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
	}}
	decision, err := Evaluate(state, Round{Items: []Item{
		{ID: "stuck", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
		{ID: "remaining", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
	}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if item := decision.State.Items["stuck"]; item.Status != ItemStatusDeferred || !item.Stuck || item.FixerAttempts != 4 {
		t.Fatalf("stuck item = %#v, want durable deferral", item)
	}
	if decision.Action != ActionContinue {
		t.Fatalf("decision = %#v, remaining item should continue", decision)
	}
}

func TestSeverityFloorControlsProductivityAndContinuation(t *testing.T) {
	round := Round{Items: []Item{
		{ID: "blocking", Severity: reviewitem.SeverityBlocking, Status: ItemStatusResolved},
		{ID: "non-blocking", Severity: reviewitem.SeverityNonBlocking, Status: ItemStatusOpen},
		{ID: "nit", Severity: reviewitem.SeverityNit, Status: ItemStatusOpen},
	}}
	for _, tc := range []struct {
		floor      SeverityFloor
		wantAction Action
		wantOpen   int
	}{
		{floor: SeverityFloorBlocking, wantAction: ActionComplete, wantOpen: 0},
		{floor: SeverityFloorNonBlocking, wantAction: ActionContinue, wantOpen: 1},
		{floor: SeverityFloorAll, wantAction: ActionContinue, wantOpen: 2},
	} {
		t.Run(string(tc.floor), func(t *testing.T) {
			policy := DefaultPolicy()
			policy.SeverityFloor = tc.floor
			decision, err := Evaluate(State{}, round, policy)
			if err != nil {
				t.Fatal(err)
			}
			open := decision.State.History[0].OpenItemIDs
			if decision.Action != tc.wantAction || len(open) != tc.wantOpen {
				t.Fatalf("decision = %#v, want action=%s open=%d", decision, tc.wantAction, tc.wantOpen)
			}
		})
	}
}

func TestBlockingFloorDoesNotLetLowerSeverityExtendLoop(t *testing.T) {
	policy := DefaultPolicy()
	policy.SeverityFloor = SeverityFloorBlocking
	decision, err := Evaluate(State{}, Round{Items: []Item{
		{ID: "non-blocking", Severity: reviewitem.SeverityNonBlocking, Status: ItemStatusOpen},
		{ID: "nit", Severity: reviewitem.SeverityNit, Status: ItemStatusOpen},
	}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Productive || decision.Action != ActionComplete || decision.Reason != ReasonSeverityFloorReached {
		t.Fatalf("decision = %#v, lower severities must not extend blocking loop", decision)
	}
}

func TestAbsoluteRoundCeilingHasDistinctReason(t *testing.T) {
	policy := DefaultPolicy()
	policy.MaxTotalRounds = 2
	state := State{TotalRounds: 1, Items: map[string]Item{
		"old": {ID: "old", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
	}}
	decision, err := Evaluate(state, Round{Items: []Item{{ID: "new", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen}}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != ActionEscalate || decision.Reason != ReasonAbsoluteCeiling || !decision.Productive {
		t.Fatalf("decision = %#v, want productive absolute-ceiling escalation", decision)
	}
}

func TestEvaluateRejectsAuthorityDrift(t *testing.T) {
	policy := DefaultPolicy()
	state := State{Items: map[string]Item{
		"item-1": {ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen},
	}}
	_, err := Evaluate(state, Round{Items: []Item{{ID: "item-1", Severity: reviewitem.SeverityNit, Status: ItemStatusOpen}}}, policy)
	if err == nil {
		t.Fatal("expected same-ID severity drift to fail closed")
	}
}
