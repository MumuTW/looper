package triager

import "sync/atomic"

// ReadBudget is a tick-wide atomic reservation for pending-source forge reads.
// It is separate from DecisionBudget because confirming or replaying a durable
// report consumes GitHub capacity without invoking the LLM.
type ReadBudget struct{ remaining atomic.Int64 }

func NewReadBudget(limit int) *ReadBudget {
	budget := &ReadBudget{}
	budget.remaining.Store(int64(limit))
	return budget
}

func (b *ReadBudget) Reserve(reads int) bool {
	if b == nil {
		return true
	}
	if reads <= 0 {
		return true
	}
	for {
		remaining := b.remaining.Load()
		if remaining < int64(reads) {
			return false
		}
		if b.remaining.CompareAndSwap(remaining, remaining-int64(reads)) {
			return true
		}
	}
}

func (b *ReadBudget) Remaining() int {
	if b == nil {
		return 0
	}
	return int(b.remaining.Load())
}
