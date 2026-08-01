package workflow

import (
	"slices"
	"testing"
)

func TestSequenceIsTheFixerPipelineInOrder(t *testing.T) {
	t.Parallel()

	want := []Step{
		StepDiscoverPR,
		StepClaimPR,
		StepCollectFixes,
		StepPrepareWorktree,
		StepRepair,
		StepReconcileCommits,
		StepValidate,
		StepPush,
		StepResolveComments,
		StepRecheck,
	}
	if got := Sequence(); !slices.Equal(got, want) {
		t.Fatalf("Sequence() = %v, want %v", got, want)
	}
}

func TestSequenceReturnsACopy(t *testing.T) {
	t.Parallel()

	mutated := Sequence()
	mutated[0] = Step("tampered")
	if got := Sequence()[0]; got != StepDiscoverPR {
		t.Fatalf("Sequence()[0] after caller mutation = %q, want %q", got, StepDiscoverPR)
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
		{name: "first step returns everything", start: StepDiscoverPR, first: StepDiscoverPR, count: 10},
		{name: "mid-pipeline start", start: StepValidate, first: StepValidate, count: 4},
		{name: "last step returns itself", start: StepRecheck, first: StepRecheck, count: 1},
		{name: "empty start defaults to the full sequence", start: "", first: StepDiscoverPR, count: 10},
		{name: "unknown start defaults to the full sequence", start: Step("bogus"), first: StepDiscoverPR, count: 10},
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

func TestFromReturnsACopy(t *testing.T) {
	t.Parallel()

	mutated := From(StepPush)
	mutated[0] = Step("tampered")
	if got := From(StepPush)[0]; got != StepPush {
		t.Fatalf("From(StepPush)[0] after caller mutation = %q, want %q", got, StepPush)
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
	if got := Parse(""); got != "" {
		t.Fatalf("Parse(empty) = %q, want empty", got)
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
		{name: "fresh start reaches repair", start: StepDiscoverPR, target: StepRepair, want: true},
		{name: "prepare-worktree start reaches repair", start: StepPrepareWorktree, target: StepRepair, want: true},
		{name: "repair start reaches itself", start: StepRepair, target: StepRepair, want: true},
		{name: "validate start does not reach repair", start: StepValidate, target: StepRepair, want: false},
		{name: "push start does not reach repair", start: StepPush, target: StepRepair, want: false},
		{name: "recheck start does not reach repair", start: StepRecheck, target: StepRepair, want: false},
		{name: "empty start reaches every real step", start: "", target: StepRepair, want: true},
		{name: "unknown start reaches every real step", start: Step("bogus"), target: StepRepair, want: true},
		{name: "real start does not reach unknown target", start: StepDiscoverPR, target: Step("bogus"), want: false},
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
		restartFromDiscover bool
		resumeFromPrepare   bool
		want                ResumeDecision
	}{
		{
			name:   "no predecessor starts fresh",
			status: "",
			want:   ResumeDecision{Mode: ResumeModeFresh, StartStep: StepDiscoverPR},
		},
		{
			name:                "success starts fresh even when restart signals fire",
			status:              "success",
			lastCompleted:       StepRepair,
			restartFromDiscover: true,
			resumeFromPrepare:   true,
			want:                ResumeDecision{Mode: ResumeModeFresh, StartStep: StepDiscoverPR},
		},
		{
			name:          "failed with advance target continues past it",
			status:        "failed",
			lastCompleted: StepRepair,
			want: ResumeDecision{
				Mode:                ResumeModeAdvance,
				StartStep:           StepReconcileCommits,
				Resumed:             true,
				StickyAgentSnapshot: true,
				LastCompletedStep:   StepRepair,
			},
		},
		{
			name:              "prepare rewind beats advancing",
			status:            "interrupted",
			lastCompleted:     StepValidate,
			resumeFromPrepare: true,
			want: ResumeDecision{
				Mode:                ResumeModeResumePrepare,
				StartStep:           StepPrepareWorktree,
				Resumed:             true,
				StickyAgentSnapshot: true,
				LastCompletedStep:   StepCollectFixes,
			},
		},
		{
			name:                "discover restart beats prepare rewind and advancing",
			status:              "failed",
			lastCompleted:       StepValidate,
			restartFromDiscover: true,
			resumeFromPrepare:   true,
			want: ResumeDecision{
				Mode:                ResumeModeRestartDiscover,
				StartStep:           StepDiscoverPR,
				StickyAgentSnapshot: true,
			},
		},
		{
			name:          "completed final step has no advance target and restarts fresh but sticky",
			status:        "failed",
			lastCompleted: StepRecheck,
			want: ResumeDecision{
				Mode:                ResumeModeFresh,
				StartStep:           StepDiscoverPR,
				StickyAgentSnapshot: true,
			},
		},
		{
			name:   "interrupted before any completed step restarts fresh but sticky",
			status: "interrupted",
			want: ResumeDecision{
				Mode:                ResumeModeFresh,
				StartStep:           StepDiscoverPR,
				StickyAgentSnapshot: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecideResume(tc.status, tc.lastCompleted, tc.restartFromDiscover, tc.resumeFromPrepare)
			if got != tc.want {
				t.Fatalf("DecideResume(%q, %q, %t, %t) = %+v, want %+v",
					tc.status, tc.lastCompleted, tc.restartFromDiscover, tc.resumeFromPrepare, got, tc.want)
			}
		})
	}
}

// A resumed decision never starts at discover and a non-resumed decision
// always does — Resumed is definitionally "starts past discover on a
// continuable predecessor", and this invariant is what run-record consumers
// (sticky snapshots, LastCompletedStep persistence) rely on.
func TestDecideResumeResumedMeansPastDiscover(t *testing.T) {
	t.Parallel()

	statuses := []string{"", "running", "success", "failed", "interrupted", "completed"}
	lastCompleteds := append(Sequence(), Step(""))
	for _, status := range statuses {
		for _, lastCompleted := range lastCompleteds {
			for _, restart := range []bool{false, true} {
				for _, prepare := range []bool{false, true} {
					got := DecideResume(status, lastCompleted, restart, prepare)
					if got.Resumed != (got.StartStep != StepDiscoverPR) {
						t.Fatalf("DecideResume(%q, %q, %t, %t) = %+v: Resumed must equal StartStep != discover",
							status, lastCompleted, restart, prepare, got)
					}
					if got.Resumed && !got.StickyAgentSnapshot {
						t.Fatalf("DecideResume(%q, %q, %t, %t) = %+v: a resumed run must inherit the agent snapshot",
							status, lastCompleted, restart, prepare, got)
					}
					if (got.LastCompletedStep != "") != got.Resumed {
						t.Fatalf("DecideResume(%q, %q, %t, %t) = %+v: LastCompletedStep is recorded exactly for resumed runs",
							status, lastCompleted, restart, prepare, got)
					}
				}
			}
		}
	}
}
