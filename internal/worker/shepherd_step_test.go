package worker

import "testing"

// Stage A (inert surface): stepShepherd is OFF the linear workerStepSequence so
// the normal impl flow still ends at open-pr (inert). stepsFrom special-cases it
// to just [stepShepherd] — never defaulting to the whole sequence (the v1 bug
// where forcing an unknown start step re-ran the worker from prepare-work) — and
// nextWorkerStep=="" so a shepherd run does exactly one pass per enqueue.

func TestStepShepherdIsTerminalSelfRepeating(t *testing.T) {
	for _, s := range workerStepSequence {
		if s == stepShepherd {
			t.Fatalf("stepShepherd must NOT be in the linear workerStepSequence (breaks inert normal flow): %v", workerStepSequence)
		}
	}
	if got := workerStepSequence[len(workerStepSequence)-1]; got != stepOpenPR {
		t.Fatalf("normal flow must still end at open-pr, got %v", got)
	}
	if got := stepsFrom(stepShepherd); len(got) != 1 || got[0] != stepShepherd {
		t.Fatalf("stepsFrom(stepShepherd) = %v, want [shepherd]", got)
	}
	if got := nextWorkerStep(stepShepherd); got != "" {
		t.Fatalf("nextWorkerStep(stepShepherd) = %q, want \"\"", got)
	}
	// the normal flow's last step still resolves without shepherd tacked on
	if got := stepsFrom(stepOpenPR); len(got) != 1 || got[0] != stepOpenPR {
		t.Fatalf("stepsFrom(open-pr) = %v, want [open-pr] (shepherd not appended)", got)
	}
}
