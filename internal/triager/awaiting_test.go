package triager

import (
	"fmt"
	"testing"
	"time"
)

func awaitingState(key string, issueNumber int64, createdAt time.Time) *sourceState {
	return &sourceState{
		enrollment: Enrollment{IdempotencyKey: key, IssueNumber: issueNumber},
		report: &Report{
			Policy:    PolicyDecision{Action: ActionAwaitHuman},
			CreatedAt: createdAt.Format(time.RFC3339Nano),
		},
	}
}

func routableState(key string, issueNumber int64) *sourceState {
	return &sourceState{
		enrollment: Enrollment{IdempotencyKey: key, IssueNumber: issueNumber},
		report:     &Report{Policy: PolicyDecision{Action: ActionRoutePlanner}},
	}
}

// A source waiting on a human can only change when someone touches its issue, and
// the update-window search already tells us which issues those are — for free.
func TestShouldProcessAwaitingFollowsTheUpdateWindow(t *testing.T) {
	t.Parallel()
	state := awaitingState("k1", 42, time.Now())
	touched := map[int64]struct{}{42: {}}

	if !shouldProcessAwaiting(state, touched, nil) {
		t.Fatal("touched issue was not processed; a confirmation would be ignored")
	}
	if shouldProcessAwaiting(state, map[int64]struct{}{}, nil) {
		t.Fatal("untouched issue was processed; that is the per-tick cost this removes")
	}
	if !shouldProcessAwaiting(state, map[int64]struct{}{}, map[string]struct{}{"k1": {}}) {
		t.Fatal("recheck-selected source was not processed; the catch-up path is dead")
	}
}

// The catch-up budget is what stops a confirmation being missed forever if it
// lands during a gap in the update window. It must be bounded, must skip sources
// already covered by the window, and must round-robin so no source starves.
func TestSelectAwaitingRechecksIsBoundedAndFair(t *testing.T) {
	t.Parallel()
	now := time.Now()
	pending := []*sourceState{}
	for i := 1; i <= 10; i++ {
		pending = append(pending, awaitingState(fmt.Sprintf("k%d", i), int64(i), now))
	}

	selected, cursor := selectAwaitingRechecks(pending, map[int64]struct{}{}, 3, awaitingRecheckCursor{})
	if len(selected) != 3 {
		t.Fatalf("selected = %d, want 3 (the budget)", len(selected))
	}
	// pending is oldest-first, so the front of the queue is taken.
	for _, key := range []string{"k1", "k2", "k3"} {
		if _, ok := selected[key]; !ok {
			t.Fatalf("selected = %v, want the oldest sources so none starve", selected)
		}
	}

	// Sources the update window already covers must not consume the budget meant
	// for quiet ones.
	withTouched, _ := selectAwaitingRechecks(pending, map[int64]struct{}{1: {}, 2: {}}, 3, awaitingRecheckCursor{})
	for _, key := range []string{"k1", "k2"} {
		if _, ok := withTouched[key]; ok {
			t.Fatalf("touched source %s consumed recheck budget", key)
		}
	}
	if len(withTouched) != 3 {
		t.Fatalf("selected with touched = %d, want 3 quiet sources", len(withTouched))
	}

	if got, _ := selectAwaitingRechecks(pending, map[int64]struct{}{}, 0, awaitingRecheckCursor{}); len(got) != 0 {
		t.Fatalf("selected with zero budget = %d, want 0", len(got))
	}
	next, _ := selectAwaitingRechecks(pending, map[int64]struct{}{}, 3, cursor)
	for _, key := range []string{"k4", "k5", "k6"} {
		if _, ok := next[key]; !ok {
			t.Fatalf("second selection = %v, want cursor to advance through %s", next, key)
		}
	}
}

// Only awaiting-confirmation sources are throttled; anything else still runs every
// tick, because its outcome does not depend on a human.
func TestSelectAwaitingRechecksIgnoresNonAwaitingSources(t *testing.T) {
	t.Parallel()
	pending := []*sourceState{routableState("k1", 1), awaitingState("k2", 2, time.Now())}
	selected, _ := selectAwaitingRechecks(pending, map[int64]struct{}{}, 3, awaitingRecheckCursor{})
	if _, ok := selected["k1"]; ok {
		t.Fatal("a routable source was selected for awaiting recheck")
	}
	if _, ok := selected["k2"]; !ok {
		t.Fatal("the awaiting source was not selected")
	}
	if shouldProcessAwaiting(routableState("k1", 1), map[int64]struct{}{}, nil) {
		// shouldProcessAwaiting is only consulted for awaiting sources, but keep the
		// contract explicit: it makes no claim about others.
		t.Log("shouldProcessAwaiting is only meaningful for awaiting sources")
	}
}

// Without a ceiling the awaiting set only grows, because nothing else removes an
// entry. Durations are literals rather than awaitingConfirmationTTL ± something:
// expressing them in terms of the constant makes the test pass for any value.
func TestAwaitingExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)

	if awaitingExpired(awaitingState("k", 1, now.Add(-6*24*time.Hour)), now) {
		t.Fatal("6 days old expired; want still waiting")
	}
	if !awaitingExpired(awaitingState("k", 1, now.Add(-8*24*time.Hour)), now) {
		t.Fatal("8 days old did not expire; the awaiting set stays unbounded")
	}
	if awaitingExpired(routableState("k", 1), now) {
		t.Fatal("a routable source expired; only awaiting sources have a ceiling")
	}
	unparseable := awaitingState("k", 1, now)
	unparseable.report.CreatedAt = "not-a-time"
	if awaitingExpired(unparseable, now) {
		t.Fatal("unreadable timestamp expired a source; retirement must never rest on unreadable evidence")
	}
}
