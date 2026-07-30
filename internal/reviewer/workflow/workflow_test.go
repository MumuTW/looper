package workflow_test

import (
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/reviewer/workflow"
)

func TestSequence(t *testing.T) {
	t.Parallel()

	want := []workflow.Step{
		workflow.StepDiscover,
		workflow.StepFilter,
		workflow.StepClaim,
		workflow.StepSnapshot,
		workflow.StepWorktree,
		workflow.StepThreadResolution,
		workflow.StepReview,
		workflow.StepPublish,
	}
	got := workflow.Sequence()
	if len(got) != len(want) {
		t.Fatalf("Sequence() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sequence()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		start workflow.Step
		want  []workflow.Step
	}{
		{"discover returns full sequence", workflow.StepDiscover, workflow.Sequence()},
		{"claim returns tail from claim", workflow.StepClaim, []workflow.Step{workflow.StepClaim, workflow.StepSnapshot, workflow.StepWorktree, workflow.StepThreadResolution, workflow.StepReview, workflow.StepPublish}},
		{"publish returns just the last step", workflow.StepPublish, []workflow.Step{workflow.StepPublish}},
		{"empty start defaults to full sequence", workflow.Step(""), workflow.Sequence()},
		{"unrecognized start defaults to full sequence", workflow.Step("bogus"), workflow.Sequence()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := workflow.From(tt.start)
			if len(got) != len(tt.want) {
				t.Fatalf("From(%q) = %v, want %v", tt.start, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("From(%q)[%d] = %q, want %q", tt.start, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		step workflow.Step
		want workflow.Step
	}{
		{"discover advances to filter", workflow.StepDiscover, workflow.StepFilter},
		{"filter advances to claim", workflow.StepFilter, workflow.StepClaim},
		{"claim advances to snapshot", workflow.StepClaim, workflow.StepSnapshot},
		{"snapshot advances to worktree", workflow.StepSnapshot, workflow.StepWorktree},
		{"worktree advances to thread_resolution", workflow.StepWorktree, workflow.StepThreadResolution},
		{"thread_resolution advances to review", workflow.StepThreadResolution, workflow.StepReview},
		{"review advances to publish", workflow.StepReview, workflow.StepPublish},
		{"publish is terminal", workflow.StepPublish, workflow.Step("")},
		{"empty step is unrecognized", workflow.Step(""), workflow.Step("")},
		{"unrecognized step yields empty", workflow.Step("bogus"), workflow.Step("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workflow.Next(tt.step); got != tt.want {
				t.Errorf("Next(%q) = %q, want %q", tt.step, got, tt.want)
			}
		})
	}
}

// TestShouldRestartFromDiscover migrates
// TestShouldRestartFromDiscoverForAgentNativePreflightFailures and
// TestShouldRestartFromDiscoverForThreadResolutionHeadChange from
// internal/reviewer/runner_test.go, which called the moved shouldRestartFromDiscover
// helper directly, plus additional cases covering the status and step gates.
func TestShouldRestartFromDiscover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  string
		step    workflow.Step
		summary string
		want    bool
	}{
		{"failed review head changed before publish", "failed", workflow.StepReview, "PR head changed before publish", true},
		{"failed review request removed before publish", "failed", workflow.StepReview, "review request removed before publish", true},
		{"interrupted publish head changed while running", "interrupted", workflow.StepPublish, "PR head changed while reviewer was running", true},
		{"failed thread_resolution PR changed during reconciliation", "failed", workflow.StepThreadResolution, "PR changed during thread reconciliation", true},
		{"interrupted thread_resolution PR changed during reconciliation", "interrupted", workflow.StepThreadResolution, "PR changed during thread reconciliation", true},
		{"completed status never restarts", "completed", workflow.StepReview, "PR head changed before publish", false},
		{"running status never restarts", "running", workflow.StepReview, "PR head changed before publish", false},
		{"step outside publish/review/thread_resolution never restarts", "failed", workflow.StepClaim, "PR head changed before publish", false},
		{"worktree step never restarts", "failed", workflow.StepWorktree, "review request removed before publish", false},
		{"unrelated summary never restarts", "failed", workflow.StepReview, "some other failure entirely", false},
		{"empty summary never restarts", "failed", workflow.StepReview, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workflow.ShouldRestartFromDiscover(tt.status, tt.step, tt.summary); got != tt.want {
				t.Errorf("ShouldRestartFromDiscover(%q, %q, %q) = %v, want %v", tt.status, tt.step, tt.summary, got, tt.want)
			}
		})
	}
}

func TestPlanResume(t *testing.T) {
	t.Parallel()

	falseCallback := func(workflow.Step) bool { return false }
	trueCallback := func(workflow.Step) bool { return true }
	panicCallback := func(workflow.Step) bool { panic("NeedsEligibilityRediscovery should not be called") }

	tests := []struct {
		name string
		in   workflow.ResumeInput
		want workflow.ResumePlan
	}{
		{
			name: "no latest run starts fresh at discover",
			in: workflow.ResumeInput{
				HasLatestRun:                false,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "latest run completed (not failed/interrupted) starts fresh at discover",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "completed",
				LastCompletedStep:           workflow.StepPublish,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "failed with explicit restart_from_discover policy restarts at discover but is not resumed",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepWorktree,
				FailedStep:                  workflow.StepReview,
				CheckpointResumePolicy:      loops.ResumePolicyRestartFromDiscover,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				StickySnapshot:      true,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "failed with a shouldRestartFromDiscover-triggering summary restarts at discover",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepThreadResolution,
				FailedStep:                  workflow.StepReview,
				FailureSummary:              "PR head changed before publish: expected abc, got def",
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				StickySnapshot:      true,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "interrupted with a shouldRestartFromDiscover-triggering summary restarts at discover",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "interrupted",
				FailedStep:                  workflow.StepThreadResolution,
				FailureSummary:              "PR changed during thread reconciliation",
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				StickySnapshot:      true,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "failed rerun_review on a non-manual loop restarts at discover",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepPublish,
				FailedStep:                  workflow.StepPublish,
				CheckpointResumePolicy:      workflow.ResumePolicyRerunReview,
				ManualLoop:                  false,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				StickySnapshot:      true,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "failed rerun_review on a manual loop resumes at review and carries the checkpoint",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepSnapshot,
				FailedStep:                  workflow.StepPublish,
				CheckpointResumePolicy:      workflow.ResumePolicyRerunReview,
				ManualLoop:                  true,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:               workflow.StepReview,
				Resumed:                 true,
				StickySnapshot:          true,
				CarryCheckpoint:         true,
				InitialResumePolicy:     loops.ResumePolicyAdvanceFromCheckpoint,
				ClearWorktreePreparedAt: true,
				CarryLastCompletedStep:  true,
			},
		},
		{
			name: "failed mid-sequence advances to the step after the last completed one",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepSnapshot,
				FailedStep:                  workflow.StepWorktree,
				NeedsEligibilityRediscovery: falseCallback,
			},
			want: workflow.ResumePlan{
				StartStep:              workflow.StepWorktree,
				Resumed:                true,
				StickySnapshot:         true,
				CarryCheckpoint:        true,
				InitialResumePolicy:    loops.ResumePolicyAdvanceFromCheckpoint,
				CarryLastCompletedStep: true,
			},
		},
		{
			name: "interrupted mid-sequence advances to the step after the last completed one",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "interrupted",
				LastCompletedStep:           workflow.StepClaim,
				FailedStep:                  workflow.StepSnapshot,
				NeedsEligibilityRediscovery: falseCallback,
			},
			want: workflow.ResumePlan{
				StartStep:              workflow.StepSnapshot,
				Resumed:                true,
				StickySnapshot:         true,
				CarryCheckpoint:        true,
				InitialResumePolicy:    loops.ResumePolicyAdvanceFromCheckpoint,
				CarryLastCompletedStep: true,
			},
		},
		{
			name: "mid-sequence advance to review clears worktree preparedAt",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepThreadResolution,
				FailedStep:                  workflow.StepReview,
				NeedsEligibilityRediscovery: falseCallback,
			},
			want: workflow.ResumePlan{
				StartStep:               workflow.StepReview,
				Resumed:                 true,
				StickySnapshot:          true,
				CarryCheckpoint:         true,
				InitialResumePolicy:     loops.ResumePolicyAdvanceFromCheckpoint,
				ClearWorktreePreparedAt: true,
				CarryLastCompletedStep:  true,
			},
		},
		{
			name: "failed with no completed step at all starts fresh at discover",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.Step(""),
				FailedStep:                  workflow.StepClaim,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				StickySnapshot:      true,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "eligibility-rediscovery override flips a mid-sequence resume back to discover",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepSnapshot,
				FailedStep:                  workflow.StepWorktree,
				NeedsEligibilityRediscovery: trueCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				StickySnapshot:      true,
				InitialResumePolicy: loops.ResumePolicyReplayStep,
			},
		},
		{
			name: "eligibility-rediscovery override is skipped for manual loops even if it would fire",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepSnapshot,
				FailedStep:                  workflow.StepWorktree,
				ManualLoop:                  true,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:              workflow.StepWorktree,
				Resumed:                true,
				StickySnapshot:         true,
				CarryCheckpoint:        true,
				InitialResumePolicy:    loops.ResumePolicyAdvanceFromCheckpoint,
				CarryLastCompletedStep: true,
			},
		},
		{
			name: "nil eligibility-rediscovery callback is treated as false",
			in: workflow.ResumeInput{
				HasLatestRun:      true,
				LatestStatus:      "failed",
				LastCompletedStep: workflow.StepSnapshot,
				FailedStep:        workflow.StepWorktree,
			},
			want: workflow.ResumePlan{
				StartStep:              workflow.StepWorktree,
				Resumed:                true,
				StickySnapshot:         true,
				CarryCheckpoint:        true,
				InitialResumePolicy:    loops.ResumePolicyAdvanceFromCheckpoint,
				CarryLastCompletedStep: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := workflow.PlanResume(tt.in)
			if got != tt.want {
				t.Errorf("PlanResume(%+v) =\n  %+v\nwant\n  %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNextResumePolicyOnFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		failureKind string
		current     string
		want        string
	}{
		// retryable_after_resume: advances to advance_from_checkpoint unless
		// current is already restart_from_discover or rerun_review (sticky).
		{"retryable-after-resume from empty advances", loops.FailureKindRetryableAfterResume, "", loops.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-after-resume from advance stays advance", loops.FailureKindRetryableAfterResume, loops.ResumePolicyAdvanceFromCheckpoint, loops.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-after-resume from replay advances", loops.FailureKindRetryableAfterResume, loops.ResumePolicyReplayStep, loops.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-after-resume from restart_from_discover is sticky", loops.FailureKindRetryableAfterResume, loops.ResumePolicyRestartFromDiscover, loops.ResumePolicyRestartFromDiscover},
		{"retryable-after-resume from rerun_review is sticky", loops.FailureKindRetryableAfterResume, workflow.ResumePolicyRerunReview, workflow.ResumePolicyRerunReview},
		{"retryable-after-resume from manual_intervention advances", loops.FailureKindRetryableAfterResume, loops.ResumePolicyManualIntervention, loops.ResumePolicyAdvanceFromCheckpoint},

		// manual_intervention: always wins outright, regardless of current.
		{"manual-intervention from empty", loops.FailureKindManualIntervention, "", loops.ResumePolicyManualIntervention},
		{"manual-intervention from advance", loops.FailureKindManualIntervention, loops.ResumePolicyAdvanceFromCheckpoint, loops.ResumePolicyManualIntervention},
		{"manual-intervention from replay", loops.FailureKindManualIntervention, loops.ResumePolicyReplayStep, loops.ResumePolicyManualIntervention},
		{"manual-intervention from restart_from_discover", loops.FailureKindManualIntervention, loops.ResumePolicyRestartFromDiscover, loops.ResumePolicyManualIntervention},
		{"manual-intervention from manual_intervention", loops.FailureKindManualIntervention, loops.ResumePolicyManualIntervention, loops.ResumePolicyManualIntervention},
		{"manual-intervention from rerun_review", loops.FailureKindManualIntervention, workflow.ResumePolicyRerunReview, loops.ResumePolicyManualIntervention},

		// any other failure kind: replay_step only fills an empty current;
		// otherwise current is preserved untouched.
		{"retryable-transient from empty defaults to replay", "retryable_transient", "", loops.ResumePolicyReplayStep},
		{"retryable-transient preserves advance", "retryable_transient", loops.ResumePolicyAdvanceFromCheckpoint, loops.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-transient preserves replay", "retryable_transient", loops.ResumePolicyReplayStep, loops.ResumePolicyReplayStep},
		{"retryable-transient preserves restart_from_discover", "retryable_transient", loops.ResumePolicyRestartFromDiscover, loops.ResumePolicyRestartFromDiscover},
		{"retryable-transient preserves manual_intervention", "retryable_transient", loops.ResumePolicyManualIntervention, loops.ResumePolicyManualIntervention},
		{"retryable-transient preserves rerun_review", "retryable_transient", workflow.ResumePolicyRerunReview, workflow.ResumePolicyRerunReview},
		{"non-retryable from empty defaults to replay", "non_retryable", "", loops.ResumePolicyReplayStep},
		{"non-retryable preserves current", "non_retryable", loops.ResumePolicyAdvanceFromCheckpoint, loops.ResumePolicyAdvanceFromCheckpoint},
		{"empty failure kind from empty defaults to replay", "", "", loops.ResumePolicyReplayStep},
		{"empty failure kind preserves current", "", loops.ResumePolicyManualIntervention, loops.ResumePolicyManualIntervention},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := workflow.NextResumePolicyOnFailure(tt.failureKind, tt.current); got != tt.want {
				t.Errorf("NextResumePolicyOnFailure(%q, %q) = %q, want %q", tt.failureKind, tt.current, got, tt.want)
			}
		})
	}
}

func TestPreferInMemoryCheckpoint(t *testing.T) {
	t.Parallel()

	policies := []string{"", loops.ResumePolicyAdvanceFromCheckpoint, loops.ResumePolicyReplayStep, loops.ResumePolicyRestartFromDiscover, loops.ResumePolicyManualIntervention, workflow.ResumePolicyRerunReview}
	for _, policy := range policies {
		for _, markerMiss := range []bool{false, true} {
			want := policy == workflow.ResumePolicyRerunReview || markerMiss
			got := workflow.PreferInMemoryCheckpoint(policy, markerMiss)
			if got != want {
				t.Errorf("PreferInMemoryCheckpoint(%q, %v) = %v, want %v", policy, markerMiss, got, want)
			}
		}
	}
}

func TestCarryRestartFromDiscover(t *testing.T) {
	t.Parallel()

	policies := []string{"", loops.ResumePolicyAdvanceFromCheckpoint, loops.ResumePolicyReplayStep, loops.ResumePolicyRestartFromDiscover, loops.ResumePolicyManualIntervention, workflow.ResumePolicyRerunReview}
	for _, policy := range policies {
		want := policy == loops.ResumePolicyRestartFromDiscover
		got := workflow.CarryRestartFromDiscover(policy)
		if got != want {
			t.Errorf("CarryRestartFromDiscover(%q) = %v, want %v", policy, got, want)
		}
	}
}
