package triager

import "sync/atomic"

// DecisionBudget caps how many LLM decisions triager discovery may launch in one
// scheduler tick. One budget is shared by every project discovered in that tick.
//
// It replaces a raw *int because project discovery runs concurrently: a plain
// check-then-decrement let every project observe the same remaining count and
// launch a decision, so the tick-wide cap was exceeded by up to one decision per
// project — and the unsynchronized read/write was a data race besides. Reserving
// is therefore a single atomic operation rather than a check followed by a
// decrement.
//
// A nil *DecisionBudget means uncapped, preserving the old nil-pointer contract.
type DecisionBudget struct {
	remaining atomic.Int64
}

// NewDecisionBudget returns a budget allowing limit decisions. A limit of zero or
// less allows none.
func NewDecisionBudget(limit int) *DecisionBudget {
	budget := &DecisionBudget{}
	budget.remaining.Store(int64(limit))
	return budget
}

// Reserve claims one decision and reports whether the caller may proceed. It is
// safe for concurrent use, and never reserves more than the configured limit in
// total no matter how many callers race.
func (b *DecisionBudget) Reserve() bool {
	if b == nil {
		return true
	}
	for {
		remaining := b.remaining.Load()
		if remaining <= 0 {
			return false
		}
		if b.remaining.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
}

// Remaining reports how many decisions are still available. It is a point-in-time
// observation: concurrent callers may consume the result before it is acted on,
// so never gate a decision on it — call Reserve instead.
func (b *DecisionBudget) Remaining() int {
	if b == nil {
		return 0
	}
	return int(b.remaining.Load())
}
