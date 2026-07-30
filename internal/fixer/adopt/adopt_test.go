package adopt

import "testing"

func openPreflight() Preflight {
	return Preflight{ExpectedHead: "head1", RemoteHead: "head1"}
}

func TestEligiblePreflightGateOrder(t *testing.T) {
	t.Parallel()

	if !EligiblePreflight(openPreflight()) {
		t.Fatal("clean preflight must pass")
	}

	parked := openPreflight()
	parked.LoopParked = true
	if EligiblePreflight(parked) {
		t.Fatal("a parked loop must never adopt: a human owns the worktree")
	}

	takeover := openPreflight()
	takeover.TakeoverResume = true
	if EligiblePreflight(takeover) {
		t.Fatal("a takeover-resume marker must block adoption")
	}

	noHead := openPreflight()
	noHead.ExpectedHead = "  "
	if EligiblePreflight(noHead) {
		t.Fatal("no expected head means nothing safe to match against")
	}

	// The remote-head asymmetry: absence is tolerated, disagreement is not.
	remoteEmpty := openPreflight()
	remoteEmpty.RemoteHead = ""
	if !EligiblePreflight(remoteEmpty) {
		t.Fatal("an unobserved remote head is tolerated")
	}
	remoteMoved := openPreflight()
	remoteMoved.RemoteHead = "head2"
	if EligiblePreflight(remoteMoved) {
		t.Fatal("a remote head that moved off the expected head must refuse")
	}
}

func TestConfirmLocalHead(t *testing.T) {
	t.Parallel()

	if !ConfirmLocalHead(" head1 ", "head1") {
		t.Fatal("matching local head (whitespace-tolerant) must confirm")
	}
	if ConfirmLocalHead("head2", "head1") {
		t.Fatal("a local head off the expected head must refuse: same-head is what makes a dirty adopt safe")
	}
	if ConfirmLocalHead("", "head1") || ConfirmLocalHead("head1", "") {
		t.Fatal("missing heads must refuse")
	}
}

// The token shape is a persisted contract: tokens written by earlier runs
// live in on-disk markers and checkpoints, and provenance compares them
// verbatim. Changing the shape orphans every worktree prepared before it.
func TestOwnerTokenShapeIsStable(t *testing.T) {
	t.Parallel()

	if got, want := OwnerToken("loop1", "run1", "t1"), "fixer:loop1:run1:t1"; got != want {
		t.Fatalf("OwnerToken() = %q, want %q", got, want)
	}
	if got, want := OwnerToken(" ", "", "  "), "fixer:unknown-loop:unknown-run:unknown-time"; got != want {
		t.Fatalf("OwnerToken(blank) = %q, want %q", got, want)
	}
}
