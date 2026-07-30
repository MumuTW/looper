// Package adopt holds the fixer's dirty-worktree adoption authority: the
// pure gates deciding whether a prepared-but-dirty worktree from a prior
// fixer run may be adopted for the current round, and the owner-token
// encoding that provenance checks compare against. It has no I/O — the
// fixer package owns the provenance disk read, path resolution, and git
// head inspection, and consults this package for the decision. Second
// slice under issue #384; the gate order encodes the takeover-hold
// invariants from the #162 incident line and is pinned by contract tests.
package adopt

import "strings"

// Preflight are the facts known before any git inspection.
type Preflight struct {
	// LoopParked: the loop is in human_takeover or awaiting_human — a
	// human owns the worktree and adoption must never race them.
	LoopParked bool
	// TakeoverResume: a takeover-resume marker is recorded on the loop.
	TakeoverResume bool
	// ExpectedHead is the PR head recorded on the checkpoint detail.
	ExpectedHead string
	// RemoteHead is the head the dirty preparation observed remotely;
	// empty is tolerated (nothing observed), a mismatch refuses.
	RemoteHead string
}

// EligiblePreflight applies every gate that does not require inspecting
// the local worktree, in the pinned order: takeover holds first, then an
// expected head must exist, then a non-empty remote head must match it.
// Only when this passes is the (subprocess-priced) local inspection
// worth running, followed by ConfirmLocalHead.
func EligiblePreflight(f Preflight) bool {
	if f.LoopParked || f.TakeoverResume {
		return false
	}
	expected := strings.TrimSpace(f.ExpectedHead)
	if expected == "" {
		return false
	}
	if remote := strings.TrimSpace(f.RemoteHead); remote != "" && remote != expected {
		return false
	}
	return true
}

// ConfirmLocalHead is the final gate: the local worktree's head must equal
// the expected head exactly — same-head is what makes a dirty adopt safe.
func ConfirmLocalHead(localHead, expectedHead string) bool {
	expected := strings.TrimSpace(expectedHead)
	return expected != "" && strings.TrimSpace(localHead) == expected
}

// OwnerToken encodes run-specific worktree ownership. Its shape is a
// persisted contract: tokens written by earlier runs live in on-disk
// markers and in checkpoints, and provenance compares them verbatim.
func OwnerToken(loopID, runID, preparedAt string) string {
	loopID = strings.TrimSpace(loopID)
	runID = strings.TrimSpace(runID)
	preparedAt = strings.TrimSpace(preparedAt)
	if loopID == "" {
		loopID = "unknown-loop"
	}
	if runID == "" {
		runID = "unknown-run"
	}
	if preparedAt == "" {
		preparedAt = "unknown-time"
	}
	return "fixer:" + loopID + ":" + runID + ":" + preparedAt
}
