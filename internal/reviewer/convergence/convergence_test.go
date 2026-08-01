package convergence

import (
	"fmt"
	"testing"

	"github.com/MumuTW/looper/internal/reviewitem"
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

func TestEvaluateDoesNotCountRepeatedFixerEvidence(t *testing.T) {
	policy := DefaultPolicy()
	policy.MaxFixerAttemptsPerItem = 2
	state := State{}
	item := Item{ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen, FixerResult: FixerResultDeclined, FixerAttemptKey: "attempt-1"}
	first, err := Evaluate(state, Round{Items: []Item{item}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Evaluate(first.State, Round{Items: []Item{item}}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.State.Items["item-1"].FixerAttempts; got != 1 {
		t.Fatalf("repeated fixer evidence counted %d attempts, want 1", got)
	}
	if second.State.Items["item-1"].Stuck {
		t.Fatal("repeated fixer evidence marked item stuck")
	}
}

func TestStateValidateRejectsMalformedPersistedState(t *testing.T) {
	policy := DefaultPolicy()
	validItem := Item{ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen, FixerAttempts: 1}
	cases := map[string]State{
		"negative total rounds":             {TotalRounds: -1, ConsecutiveUnproductive: 0},
		"negative consecutive unproductive": {TotalRounds: 1, ConsecutiveUnproductive: -1},
		"empty item id":                     {Items: map[string]Item{"": {ID: "", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen}}},
		"unknown severity":                  {Items: map[string]Item{"item-1": {ID: "item-1", Severity: "critical", Status: ItemStatusOpen}}},
		"unknown item status":               {Items: map[string]Item{"item-1": {ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: "wontfix"}}},
		"unknown fixer result":              {Items: map[string]Item{"item-1": {ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen, FixerResult: "retry"}}},
		"negative fixer attempts":           {Items: map[string]Item{"item-1": {ID: "item-1", Severity: reviewitem.SeverityBlocking, Status: ItemStatusOpen, FixerAttempts: -1}}},
		"negative round number":             {History: []RoundSummary{{Number: -1}}},
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			if err := state.Validate(); err == nil {
				t.Fatalf("State.Validate() = nil, want error for %s", name)
			}
		})
	}
	// A well-formed state (and the policy it pairs with) must still validate.
	good := State{TotalRounds: 1, ConsecutiveUnproductive: 0, Items: map[string]Item{"item-1": validItem}, History: []RoundSummary{{Number: 1}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("State.Validate() = %v, want nil for well-formed state", err)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("Policy.Validate() = %v, want nil", err)
	}
}

func TestEnumValidity(t *testing.T) {
	for _, a := range []Action{"", ActionContinue, ActionComplete, ActionEscalate} {
		if !a.Valid() {
			t.Fatalf("Action(%q).Valid() = false, want true", a)
		}
	}
	if bad := Action("retry"); bad.Valid() {
		t.Fatalf("Action(%q).Valid() = true, want false", bad)
	}
	for _, r := range []Reason{"", ReasonConverging, ReasonSeverityFloorReached, ReasonStalled, ReasonAbsoluteCeiling} {
		if !r.Valid() {
			t.Fatalf("Reason(%q).Valid() = false, want true", r)
		}
	}
	if bad := Reason("bored"); bad.Valid() {
		t.Fatalf("Reason(%q).Valid() = true, want false", bad)
	}
	for _, s := range []string{"", string(StatusActive), string(StatusAwaitingHuman), string(StatusCompleted)} {
		if !ValidStatus(s) {
			t.Fatalf("ValidStatus(%q) = false, want true", s)
		}
	}
	if ValidStatus("paused") {
		t.Fatal("ValidStatus(\"paused\") = true, want false")
	}
}
