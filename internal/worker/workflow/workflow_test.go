package workflow

import (
	"slices"
	"testing"
)

func TestSequenceIsTheWorkerPipelineInOrder(t *testing.T) {
	t.Parallel()

	want := []Step{
		StepPrepareWork,
		StepPrepareWorktree,
		StepPlan,
		StepExecute,
		StepValidate,
		StepOpenPR,
	}
	if got := Sequence(); !slices.Equal(got, want) {
		t.Fatalf("Sequence() = %v, want %v", got, want)
	}
}

func TestSequenceAndFromReturnCopies(t *testing.T) {
	t.Parallel()

	mutated := Sequence()
	mutated[0] = Step("tampered")
	if got := Sequence()[0]; got != StepPrepareWork {
		t.Fatalf("Sequence()[0] after caller mutation = %q, want %q", got, StepPrepareWork)
	}
	fromMutated := From(StepPlan)
	fromMutated[0] = Step("tampered")
	if got := From(StepPlan)[0]; got != StepPlan {
		t.Fatalf("From(StepPlan)[0] after caller mutation = %q, want %q", got, StepPlan)
	}
}

func TestFrom(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		start Step
		first Step
		count int
	}{
		{name: "first step returns everything", start: StepPrepareWork, first: StepPrepareWork, count: 6},
		{name: "mid-pipeline start", start: StepExecute, first: StepExecute, count: 3},
		{name: "last step returns itself", start: StepOpenPR, first: StepOpenPR, count: 1},
		{name: "empty start defaults to the full sequence", start: "", first: StepPrepareWork, count: 6},
		{name: "unknown start defaults to the full sequence", start: Step("bogus"), first: StepPrepareWork, count: 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := From(tc.start)
			if len(got) != tc.count || got[0] != tc.first {
				t.Fatalf("From(%q) = %v, want %d steps starting at %q", tc.start, got, tc.count, tc.first)
			}
		})
	}
}

func TestNextAndPreviousInvertEachOtherAcrossThePipeline(t *testing.T) {
	t.Parallel()

	steps := Sequence()
	for i, step := range steps {
		wantNext := Step("")
		if i+1 < len(steps) {
			wantNext = steps[i+1]
		}
		if got := Next(step); got != wantNext {
			t.Fatalf("Next(%q) = %q, want %q", step, got, wantNext)
		}
		wantPrevious := Step("")
		if i > 0 {
			wantPrevious = steps[i-1]
		}
		if got := Previous(step); got != wantPrevious {
			t.Fatalf("Previous(%q) = %q, want %q", step, got, wantPrevious)
		}
	}
	if got := Next(Step("bogus")); got != "" {
		t.Fatalf("Next(bogus) = %q, want empty", got)
	}
	if got := Previous(Step("bogus")); got != "" {
		t.Fatalf("Previous(bogus) = %q, want empty", got)
	}
}

func TestParse(t *testing.T) {
	t.Parallel()

	for _, step := range Sequence() {
		if got := Parse(string(step)); got != step {
			t.Fatalf("Parse(%q) = %q, want %q", string(step), got, step)
		}
	}
	if got := Parse("not-a-step"); got != "" {
		t.Fatalf("Parse(not-a-step) = %q, want empty", got)
	}
}

func TestReaches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		start  Step
		target Step
		want   bool
	}{
		{name: "fresh start reaches execute", start: StepPrepareWork, target: StepExecute, want: true},
		{name: "execute start reaches itself", start: StepExecute, target: StepExecute, want: true},
		{name: "validate start does not reach execute", start: StepValidate, target: StepExecute, want: false},
		{name: "open-pr start does not reach execute", start: StepOpenPR, target: StepExecute, want: false},
		{name: "empty start reaches every real step", start: "", target: StepExecute, want: true},
		{name: "unknown start reaches every real step", start: Step("bogus"), target: StepExecute, want: true},
		{name: "real start does not reach unknown target", start: StepPrepareWork, target: Step("bogus"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Reaches(tc.start, tc.target); got != tc.want {
				t.Fatalf("Reaches(%q, %q) = %v, want %v", tc.start, tc.target, got, tc.want)
			}
		})
	}
}

func TestDecideResume(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                string
		status              string
		lastCompleted       Step
		manualHold          bool
		restartFromDiscover bool
		replayExecute       bool
		want                ResumeDecision
	}{
		{
			name:   "no predecessor starts fresh",
			status: "",
			want:   ResumeDecision{Mode: ResumeModeFresh, StartStep: StepPrepareWork},
		},
		{
			name:                "success starts fresh even when signals fire",
			status:              "success",
			lastCompleted:       StepExecute,
			restartFromDiscover: true,
			replayExecute:       true,
			want:                ResumeDecision{Mode: ResumeModeFresh, StartStep: StepPrepareWork},
		},
		{
			name:          "manual hold parks the continuation",
			status:        "failed",
			lastCompleted: StepExecute,
			manualHold:    true,
			replayExecute: true,
			want: ResumeDecision{
				Mode:                ResumeModeFresh,
				StartStep:           StepPrepareWork,
				StickyAgentSnapshot: true,
			},
		},
		{
			name:   "no completed step means nothing to continue",
			status: "interrupted",
			want: ResumeDecision{
				Mode:                ResumeModeFresh,
				StartStep:           StepPrepareWork,
				StickyAgentSnapshot: true,
			},
		},
		{
			name:                "restart beats replay and advance",
			status:              "failed",
			lastCompleted:       StepValidate,
			restartFromDiscover: true,
			replayExecute:       true,
			want: ResumeDecision{
				Mode:                ResumeModeRestart,
				StartStep:           StepPrepareWork,
				StickyAgentSnapshot: true,
			},
		},
		{
			name:          "replay-execute beats advance",
			status:        "interrupted",
			lastCompleted: StepValidate,
			replayExecute: true,
			want: ResumeDecision{
				Mode:                ResumeModeReplayExecute,
				StartStep:           StepExecute,
				Resumed:             true,
				StickyAgentSnapshot: true,
				LastCompletedStep:   StepPlan,
			},
		},
		{
			name:          "failed with advance target continues past it",
			status:        "failed",
			lastCompleted: StepPlan,
			want: ResumeDecision{
				Mode:                ResumeModeAdvance,
				StartStep:           StepExecute,
				Resumed:             true,
				StickyAgentSnapshot: true,
				LastCompletedStep:   StepPlan,
			},
		},
		{
			name:          "completed final step has no advance target and restarts fresh but sticky",
			status:        "failed",
			lastCompleted: StepOpenPR,
			want: ResumeDecision{
				Mode:                ResumeModeFresh,
				StartStep:           StepPrepareWork,
				StickyAgentSnapshot: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecideResume(tc.status, tc.lastCompleted, tc.manualHold, tc.restartFromDiscover, tc.replayExecute)
			if got != tc.want {
				t.Fatalf("DecideResume(%q, %q, %t, %t, %t) = %+v, want %+v",
					tc.status, tc.lastCompleted, tc.manualHold, tc.restartFromDiscover, tc.replayExecute, got, tc.want)
			}
		})
	}
}

// Resumed is definitionally "starts past prepare-work on a continuable
// predecessor" — the invariant run-record consumers (sticky snapshots,
// LastCompletedStep persistence, resume policy) rely on.
func TestDecideResumeInvariants(t *testing.T) {
	t.Parallel()

	statuses := []string{"", "running", "success", "failed", "interrupted"}
	lastCompleteds := append(Sequence(), Step(""))
	for _, status := range statuses {
		for _, lastCompleted := range lastCompleteds {
			for _, hold := range []bool{false, true} {
				for _, restart := range []bool{false, true} {
					for _, replay := range []bool{false, true} {
						got := DecideResume(status, lastCompleted, hold, restart, replay)
						if got.Resumed != (got.StartStep != StepPrepareWork) {
							t.Fatalf("DecideResume(%q, %q, %t, %t, %t) = %+v: Resumed must equal StartStep != prepare-work",
								status, lastCompleted, hold, restart, replay, got)
						}
						if got.Resumed && !got.StickyAgentSnapshot {
							t.Fatalf("DecideResume(%q, %q, %t, %t, %t) = %+v: a resumed run must inherit the agent snapshot",
								status, lastCompleted, hold, restart, replay, got)
						}
						if (got.LastCompletedStep != "") != got.Resumed {
							t.Fatalf("DecideResume(%q, %q, %t, %t, %t) = %+v: LastCompletedStep is recorded exactly for resumed runs",
								status, lastCompleted, hold, restart, replay, got)
						}
						if hold && got.Mode != ResumeModeFresh {
							t.Fatalf("DecideResume(%q, %q, %t, %t, %t) = %+v: a manual hold must never continue",
								status, lastCompleted, hold, restart, replay, got)
						}
					}
				}
			}
		}
	}
}
