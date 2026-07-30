package domain

import "testing"

// TestHumanTakeoverReachableFromEveryNonTerminalStatus holds loopStatusTransitions
// to the claim its comment makes, without re-enumerating the statuses by hand.
// Enumerating is how `interrupted` was missed when takeover reachability was
// widened: the list in the table and the list in the test were both edited, and
// both forgot the same status. Deriving "non-terminal" from the table itself
// means a status added later fails here until someone decides about it.
func TestHumanTakeoverReachableFromEveryNonTerminalStatus(t *testing.T) {
	t.Parallel()

	for from, to := range loopStatusTransitions {
		if from == LoopStatusHumanTakeover {
			continue
		}
		terminal := len(to) == 0
		err := AssertLoopStatusTransition(from, LoopStatusHumanTakeover)
		if terminal && err == nil {
			t.Fatalf("AssertLoopStatusTransition(%s, human_takeover) error = nil; %s is terminal and has nothing left to take over", from, from)
		}
		if !terminal && err != nil {
			t.Fatalf("AssertLoopStatusTransition(%s, human_takeover) error = %v; %s is non-terminal, so `looper takeover` must accept it", from, err, from)
		}
	}
}
