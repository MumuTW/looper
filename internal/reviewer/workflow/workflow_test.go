package workflow_test

import (
	"testing"

	"github.com/nexu-io/looper/internal/loops/policy"
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
				InitialResumePolicy: policy.ResumePolicyReplayStep,
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
				InitialResumePolicy: policy.ResumePolicyReplayStep,
			},
		},
		{
			name: "failed with explicit restart_from_discover policy restarts at discover but is not resumed",
			in: workflow.ResumeInput{
				HasLatestRun:                true,
				LatestStatus:                "failed",
				LastCompletedStep:           workflow.StepWorktree,
				FailedStep:                  workflow.StepReview,
				CheckpointResumePolicy:      policy.ResumePolicyRestartFromDiscover,
				NeedsEligibilityRediscovery: panicCallback,
			},
			want: workflow.ResumePlan{
				StartStep:           workflow.StepDiscover,
				StickySnapshot:      true,
				InitialResumePolicy: policy.ResumePolicyReplayStep,
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
				InitialResumePolicy: policy.ResumePolicyReplayStep,
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
				InitialResumePolicy: policy.ResumePolicyReplayStep,
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
				InitialResumePolicy: policy.ResumePolicyReplayStep,
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
				InitialResumePolicy:     policy.ResumePolicyAdvanceFromCheckpoint,
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
				InitialResumePolicy:    policy.ResumePolicyAdvanceFromCheckpoint,
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
				InitialResumePolicy:    policy.ResumePolicyAdvanceFromCheckpoint,
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
				InitialResumePolicy:     policy.ResumePolicyAdvanceFromCheckpoint,
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
				InitialResumePolicy: policy.ResumePolicyReplayStep,
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
				InitialResumePolicy: policy.ResumePolicyReplayStep,
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
				InitialResumePolicy:    policy.ResumePolicyAdvanceFromCheckpoint,
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
				InitialResumePolicy:    policy.ResumePolicyAdvanceFromCheckpoint,
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
		{"retryable-after-resume from empty advances", policy.FailureKindRetryableAfterResume, "", policy.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-after-resume from advance stays advance", policy.FailureKindRetryableAfterResume, policy.ResumePolicyAdvanceFromCheckpoint, policy.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-after-resume from replay advances", policy.FailureKindRetryableAfterResume, policy.ResumePolicyReplayStep, policy.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-after-resume from restart_from_discover is sticky", policy.FailureKindRetryableAfterResume, policy.ResumePolicyRestartFromDiscover, policy.ResumePolicyRestartFromDiscover},
		{"retryable-after-resume from rerun_review is sticky", policy.FailureKindRetryableAfterResume, workflow.ResumePolicyRerunReview, workflow.ResumePolicyRerunReview},
		{"retryable-after-resume from manual_intervention advances", policy.FailureKindRetryableAfterResume, policy.ResumePolicyManualIntervention, policy.ResumePolicyAdvanceFromCheckpoint},

		// manual_intervention: always wins outright, regardless of current.
		{"manual-intervention from empty", policy.FailureKindManualIntervention, "", policy.ResumePolicyManualIntervention},
		{"manual-intervention from advance", policy.FailureKindManualIntervention, policy.ResumePolicyAdvanceFromCheckpoint, policy.ResumePolicyManualIntervention},
		{"manual-intervention from replay", policy.FailureKindManualIntervention, policy.ResumePolicyReplayStep, policy.ResumePolicyManualIntervention},
		{"manual-intervention from restart_from_discover", policy.FailureKindManualIntervention, policy.ResumePolicyRestartFromDiscover, policy.ResumePolicyManualIntervention},
		{"manual-intervention from manual_intervention", policy.FailureKindManualIntervention, policy.ResumePolicyManualIntervention, policy.ResumePolicyManualIntervention},
		{"manual-intervention from rerun_review", policy.FailureKindManualIntervention, workflow.ResumePolicyRerunReview, policy.ResumePolicyManualIntervention},

		// any other failure kind: replay_step only fills an empty current;
		// otherwise current is preserved untouched.
		{"retryable-transient from empty defaults to replay", "retryable_transient", "", policy.ResumePolicyReplayStep},
		{"retryable-transient preserves advance", "retryable_transient", policy.ResumePolicyAdvanceFromCheckpoint, policy.ResumePolicyAdvanceFromCheckpoint},
		{"retryable-transient preserves replay", "retryable_transient", policy.ResumePolicyReplayStep, policy.ResumePolicyReplayStep},
		{"retryable-transient preserves restart_from_discover", "retryable_transient", policy.ResumePolicyRestartFromDiscover, policy.ResumePolicyRestartFromDiscover},
		{"retryable-transient preserves manual_intervention", "retryable_transient", policy.ResumePolicyManualIntervention, policy.ResumePolicyManualIntervention},
		{"retryable-transient preserves rerun_review", "retryable_transient", workflow.ResumePolicyRerunReview, workflow.ResumePolicyRerunReview},
		{"non-retryable from empty defaults to replay", "non_retryable", "", policy.ResumePolicyReplayStep},
		{"non-retryable preserves current", "non_retryable", policy.ResumePolicyAdvanceFromCheckpoint, policy.ResumePolicyAdvanceFromCheckpoint},
		{"empty failure kind from empty defaults to replay", "", "", policy.ResumePolicyReplayStep},
		{"empty failure kind preserves current", "", policy.ResumePolicyManualIntervention, policy.ResumePolicyManualIntervention},
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

	policies := []string{"", policy.ResumePolicyAdvanceFromCheckpoint, policy.ResumePolicyReplayStep, policy.ResumePolicyRestartFromDiscover, policy.ResumePolicyManualIntervention, workflow.ResumePolicyRerunReview}
	for _, current := range policies {
		for _, markerMiss := range []bool{false, true} {
			want := current == workflow.ResumePolicyRerunReview || markerMiss
			got := workflow.PreferInMemoryCheckpoint(current, markerMiss)
			if got != want {
				t.Errorf("PreferInMemoryCheckpoint(%q, %v) = %v, want %v", current, markerMiss, got, want)
			}
		}
	}
}

func TestCarryRestartFromDiscover(t *testing.T) {
	t.Parallel()

	policies := []string{"", policy.ResumePolicyAdvanceFromCheckpoint, policy.ResumePolicyReplayStep, policy.ResumePolicyRestartFromDiscover, policy.ResumePolicyManualIntervention, workflow.ResumePolicyRerunReview}
	for _, current := range policies {
		want := current == policy.ResumePolicyRestartFromDiscover
		got := workflow.CarryRestartFromDiscover(current)
		if got != want {
			t.Errorf("CarryRestartFromDiscover(%q) = %v, want %v", current, got, want)
		}
	}
}
