package triager

import (
	"sync"
	"sync/atomic"
	"testing"
)

// The budget is shared by every project in a tick and projects are discovered
// concurrently, so a check-then-decrement grants more than the limit whenever two
// callers read the same remaining count. High contention across many rounds is
// what makes that lost update reproducible rather than luck-dependent.
func TestDecisionBudgetReserveNeverExceedsLimitUnderContention(t *testing.T) {
	t.Parallel()
	const (
		limit      = 3
		goroutines = 64
		rounds     = 200
	)
	for round := 0; round < rounds; round++ {
		budget := NewDecisionBudget(limit)
		var granted atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				<-start
				if budget.Reserve() {
					granted.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := int(granted.Load()); got != limit {
			t.Fatalf("round %d: reservations granted = %d, want exactly %d", round, got, limit)
		}
		if remaining := budget.Remaining(); remaining != 0 {
			t.Fatalf("round %d: remaining = %d, want 0 after the limit is exhausted", round, remaining)
		}
	}
}

func TestDecisionBudgetNilIsUncapped(t *testing.T) {
	t.Parallel()
	var budget *DecisionBudget
	for i := 0; i < 3; i++ {
		if !budget.Reserve() {
			t.Fatalf("Reserve() on a nil budget = false, want true (nil means uncapped)")
		}
	}
}

func TestDecisionBudgetNonPositiveLimitGrantsNothing(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		if NewDecisionBudget(limit).Reserve() {
			t.Fatalf("NewDecisionBudget(%d).Reserve() = true, want false", limit)
		}
	}
}
