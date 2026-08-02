package triager

import (
	"strings"
	"sync/atomic"
)

// A configured-admission source needs one repository-visibility read plus two
// ViewIssue/timeline pairs before its v3 report can be persisted. Fair turns
// therefore reserve room for five actual forge reads rather than assuming the
// two-read legacy path.
const pendingForgeReadsPerUsefulTurn = 5

// PendingForgeReadBudget charges actual pending-state forge calls across one
// scheduler tick. Optional per-project allowances keep a serial first project
// from consuming the entire shared budget.
type PendingForgeReadBudget struct {
	remaining  atomic.Int64
	allowances map[string]*atomic.Int64
}

// NewPendingForgeReadBudget creates a shared budget without project quotas.
// Direct Runner callers use nil instead, which preserves the same local cap.
func NewPendingForgeReadBudget(limit int) *PendingForgeReadBudget {
	budget := &PendingForgeReadBudget{}
	budget.remaining.Store(int64(max(limit, 0)))
	return budget
}

func newFairPendingForgeReadBudget(limit int, projectIDs []string, start int) (*PendingForgeReadBudget, int) {
	budget := NewPendingForgeReadBudget(limit)
	if len(projectIDs) == 0 || limit <= 0 {
		return budget, 0
	}
	budget.allowances = make(map[string]*atomic.Int64, len(projectIDs))
	slots := min(len(projectIDs), limit/pendingForgeReadsPerUsefulTurn)
	if slots == 0 {
		slots = min(len(projectIDs), limit)
	}
	start %= len(projectIDs)
	selected := make([]string, 0, slots)
	for i := 0; i < slots; i++ {
		selected = append(selected, projectIDs[(start+i)%len(projectIDs)])
	}
	for i, projectID := range selected {
		allowance := limit / slots
		if i < limit%slots {
			allowance++
		}
		value := &atomic.Int64{}
		value.Store(int64(allowance))
		budget.allowances[normalizePendingBudgetProject(projectID)] = value
	}
	return budget, (start + slots) % len(projectIDs)
}

func (b *PendingForgeReadBudget) Reserve(projectID string) bool {
	if b == nil {
		return true
	}
	var allowance *atomic.Int64
	if b.allowances != nil {
		allowance = b.allowances[normalizePendingBudgetProject(projectID)]
		if allowance == nil || !reserveAtomic(allowance) {
			return false
		}
	}
	if reserveAtomic(&b.remaining) {
		return true
	}
	if allowance != nil {
		allowance.Add(1)
	}
	return false
}

func (b *PendingForgeReadBudget) Remaining() int {
	if b == nil {
		return 0
	}
	return int(b.remaining.Load())
}

func reserveAtomic(value *atomic.Int64) bool {
	for {
		remaining := value.Load()
		if remaining <= 0 {
			return false
		}
		if value.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
}

func normalizePendingBudgetProject(projectID string) string {
	return strings.ToLower(strings.TrimSpace(projectID))
}
