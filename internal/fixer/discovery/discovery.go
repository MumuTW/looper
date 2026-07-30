// Package discovery holds the fixer's discovery eligibility authority: the
// pure decision of whether an open pull request is admissible for a fixer
// round, given the facts the runner has already fetched. It has no I/O —
// the fixer package owns forge queries, lock lookups, and loop-record
// reads, distills them into Facts, and calls into this package for the
// verdict. First extraction under issue #384, continuing #120's
// boundary-at-a-time pattern (workflow #309, reconcile #318, publish #336).
package discovery

import (
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// PR is the candidate's display-independent view: only what eligibility
// consults, so the package stays free of the fixer's forge types.
type PR struct {
	State   string
	IsDraft bool
	Author  string
	Labels  []string
}

// Policy is the discovery policy slice eligibility consults.
type Policy struct {
	IncludeDrafts bool
	AuthorFilter  config.FixerAuthorFilter
	Labels        []string
	LabelMode     config.LabelMode
}

// Facts are the I/O-derived inputs, fetched by the runner before the
// decision: loop-record and lock state this package must not query itself.
type Facts struct {
	// HasManualFollowupLoop: a manual fixer loop is awaiting follow-up on
	// this PR. A manual follow-up outranks draft gating and the author and
	// label filters — the operator already chose this PR — but not the
	// held-lock rules below.
	HasManualFollowupLoop bool
	// LockHeld: an active PR lock exists for this candidate.
	LockHeld bool
	// HasRunningLoop, RunningLoopManual, RunningLoopFollowUpdates describe
	// the running fixer loop found under a held lock; they are consulted
	// only when LockHeld is true.
	HasRunningLoop           bool
	RunningLoopManual        bool
	RunningLoopFollowUpdates bool
}

// Eligible decides whether the candidate is admissible for discovery. The
// precedence is pinned by the contract tests: a non-open PR is never
// eligible; a manual follow-up waives draft gating; a held lock requires a
// running fixer loop, and a manual running loop must have follow-updates
// enabled — these lock rules apply even to manual follow-ups; past the
// lock, a manual follow-up is eligible unconditionally, and everything
// else passes the author filter and label matching.
func Eligible(pr PR, currentUser string, policy Policy, facts Facts) bool {
	if !strings.EqualFold(pr.State, "open") {
		return false
	}
	if !facts.HasManualFollowupLoop && !policy.IncludeDrafts && pr.IsDraft {
		return false
	}
	if facts.LockHeld {
		if !facts.HasRunningLoop {
			return false
		}
		if facts.RunningLoopManual && !facts.RunningLoopFollowUpdates {
			return false
		}
	}
	if facts.HasManualFollowupLoop {
		return true
	}
	if policy.AuthorFilter != config.FixerAuthorFilterAny && !sameLogin(pr.Author, currentUser) {
		return false
	}
	return config.LabelsMatch(pr.Labels, policy.Labels, policy.LabelMode)
}

func sameLogin(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && strings.EqualFold(left, right)
}
