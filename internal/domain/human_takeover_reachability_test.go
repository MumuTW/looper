package domain

import "testing"

// `looper takeover` must work from whatever status a loop happens to be in when
// an operator reaches for it — the command exists precisely for moments when the
// daemon's state is not what anyone wanted.
//
// This derives the expectation from loopStatusTransitions rather than listing
// statuses by hand. A hand-written list is how LoopStatusInterrupted was missed:
// human_takeover reachability was widened, the table and a hand-enumerated test
// were updated together, and the one status absent from both stayed absent from
// both. A future status added to the table is covered here the moment it exists.
func TestEveryNonTerminalStatusReachesHumanTakeover(t *testing.T) {
	for status, outgoing := range loopStatusTransitions {
		if status == LoopStatusHumanTakeover {
			continue // already there
		}
		err := AssertLoopStatusTransition(status, LoopStatusHumanTakeover)
		if len(outgoing) == 0 {
			// Terminal: no outgoing edges at all, so takeover must be refused.
			if err == nil {
				t.Errorf("AssertLoopStatusTransition(%s, human_takeover) = nil, want refusal from a terminal status", status)
			}
			continue
		}
		if err != nil {
			t.Errorf("AssertLoopStatusTransition(%s, human_takeover) error = %v, want nil: a non-terminal loop must be takeable", status, err)
		}
	}
}

// The complement: handback is the only way back to work. Without this, a lane
// could drive a held loop straight into execution and the hold would mean
// nothing while still appearing to hold.
func TestHumanTakeoverReleasesOnlyThroughQueued(t *testing.T) {
	if err := AssertLoopStatusTransition(LoopStatusHumanTakeover, LoopStatusRunning); err == nil {
		t.Fatal("AssertLoopStatusTransition(human_takeover, running) = nil, want refusal: handback must requeue first")
	}
	if err := AssertLoopStatusTransition(LoopStatusHumanTakeover, LoopStatusQueued); err != nil {
		t.Fatalf("AssertLoopStatusTransition(human_takeover, queued) error = %v, want nil: that is handback", err)
	}
}
