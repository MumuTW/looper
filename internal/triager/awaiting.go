package triager

import (
	"time"
)

const (
	// awaitingRecheckBudget bounds how many quiet awaiting-confirmation sources are
	// re-verified per tick. Each re-verification costs a ViewIssue and a
	// ListIssueTimeline, so the uncapped behaviour made this lane's cost grow
	// linearly with the number of issues waiting on a human — permanently, since a
	// source waiting for confirmation never resolves on its own.
	//
	// Sources whose issue was touched in this tick's update window are always
	// re-verified and do not consume this budget; the budget exists only so a
	// confirmation that arrived during a gap in that window is still noticed.
	awaitingRecheckBudget = 3

	// awaitingConfirmationTTL is how long a source may wait for a human before it is
	// retired. Without it the awaiting set only grows: nothing in the system ever
	// removes an entry, so every issue that ever asked for confirmation is
	// re-verified forever. A retired source is re-enrolled if its issue sees new
	// activity, so this loses nothing except the unbounded backlog.
	awaitingConfirmationTTL = 7 * 24 * time.Hour

	RetirementReasonConfirmationTimeout = "confirmation_timeout"
)

// awaitsHumanConfirmation reports whether this source is parked on a human, which
// is what makes re-verifying it every tick pointless: the answer can only change
// when someone comments on the issue.
func awaitsHumanConfirmation(state *sourceState) bool {
	return state != nil && state.report != nil && state.report.Policy.Action == ActionAwaitHuman
}

// awaitingExpired reports whether a source has waited past awaitingConfirmationTTL.
func awaitingExpired(state *sourceState, now time.Time) bool {
	if !awaitsHumanConfirmation(state) {
		return false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, state.report.CreatedAt)
	if err != nil {
		return false
	}
	return now.UTC().Sub(createdAt.UTC()) >= awaitingConfirmationTTL
}

// selectAwaitingRechecks decides which quiet awaiting-confirmation sources to
// re-verify this tick.
//
// pending arrives oldest-first, so taking from the front round-robins over the
// backlog: every source is eventually re-verified even if its issue never appears
// in the update window, and no tick pays more than the budget.
func selectAwaitingRechecks(pending []*sourceState, touched map[int64]struct{}, budget int) map[string]struct{} {
	selected := make(map[string]struct{})
	if budget <= 0 {
		return selected
	}
	for _, state := range pending {
		if len(selected) >= budget {
			break
		}
		if !awaitsHumanConfirmation(state) {
			continue
		}
		if _, ok := touched[state.enrollment.IssueNumber]; ok {
			// Touched sources are re-verified unconditionally, so they must not spend
			// budget meant for the quiet ones.
			continue
		}
		selected[state.enrollment.IdempotencyKey] = struct{}{}
	}
	return selected
}

// shouldProcessAwaiting reports whether an awaiting-confirmation source is worth a
// forge round trip this tick.
func shouldProcessAwaiting(state *sourceState, touched map[int64]struct{}, rechecks map[string]struct{}) bool {
	if _, ok := touched[state.enrollment.IssueNumber]; ok {
		return true
	}
	_, selected := rechecks[state.enrollment.IdempotencyKey]
	return selected
}
