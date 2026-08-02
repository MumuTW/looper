package fixer

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/MumuTW/looper/internal/fixer/workflow"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/storage"
)

// This file covers the operator-retry escape from a fixer run parked for manual
// intervention: which parks it may rewrite to restart_from_discover, which it must
// leave alone, and that a rewritten checkpoint actually starts at discover on the
// replacement claim.

type parkedRunSeed struct {
	loopID      string
	runID       string
	seq         int64
	prNumber    int64
	loopStatus  string
	runStatus   string
	currentStep FixerStep
	lastStep    FixerStep
	checkpoint  fixerCheckpoint
}

// seedParkedRun writes the loop and run records a retry-escape test needs and
// returns the encoded checkpoint so callers can assert pre-retry behavior against
// the same bytes that were persisted.
func seedParkedRun(t *testing.T, fixture *runnerFixture, seed parkedRunSeed) string {
	t.Helper()
	repo := "acme/looper"
	prNumber := seed.prNumber
	loopTarget := "pr:acme/looper:" + strconv.FormatInt(prNumber, 10)
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: seed.loopID, Seq: seed.seq, ProjectID: "project_1", Type: "fixer",
		TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber,
		Status: seed.loopStatus, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", seed.loopID, err)
	}
	checkpointJSON := runpipe.MustMarshalJSON(seed.checkpoint)
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: seed.runID, LoopID: seed.loopID, Status: seed.runStatus,
		CurrentStep: runpipe.StringPtr(string(seed.currentStep)), LastCompletedStep: runpipe.StringPtr(string(seed.lastStep)),
		CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(%s) error = %v", seed.runID, err)
	}
	return checkpointJSON
}

// invalidCompletionCheckpoint is the park this escape exists for: the agent
// completed but returned no valid structured result.
func invalidCompletionCheckpoint(t *testing.T, nowISO, worktreeName, branch, summary string) fixerCheckpoint {
	t.Helper()
	return fixerCheckpoint{
		ResumePolicy: loops.ResumePolicyManualIntervention,
		Worktree:     &checkpointWorktree{Path: filepath.Join(t.TempDir(), worktreeName), Branch: branch, PreparedAt: nowISO},
		Repair:       &checkpointRepair{Summary: summary, ParseStatus: "", CompletedAt: nowISO},
	}
}

func resumePolicyOf(t *testing.T, fixture *runnerFixture, runID string) string {
	t.Helper()
	persisted, err := fixture.repos.Runs.GetByID(context.Background(), runID)
	if err != nil || persisted == nil {
		t.Fatalf("Runs.GetByID(%s) = (%#v, %v)", runID, persisted, err)
	}
	return parseCheckpoint(persisted.CheckpointJSON).ResumePolicy
}

// assertNoEscape asserts the mark is a no-op and the stored policy is unchanged —
// the shape every "must keep its park" case shares.
func assertNoEscape(t *testing.T, fixture *runnerFixture, seed parkedRunSeed, why string) {
	t.Helper()
	seedParkedRun(t, fixture, seed)
	rewrote, err := MarkInvalidCompletionRunRestartFromDiscover(context.Background(), fixture.repos, seed.loopID, fixture.nowISO())
	if err != nil || rewrote {
		t.Fatalf("MarkInvalidCompletionRunRestartFromDiscover() = (%v, %v), want no rewrite: %s", rewrote, err, why)
	}
	if got := resumePolicyOf(t, fixture, seed.runID); got != seed.checkpoint.ResumePolicy {
		t.Fatalf("ResumePolicy = %q, want %q preserved: %s", got, seed.checkpoint.ResumePolicy, why)
	}
}

func TestOperatorRetryEscapesManualInterventionCheckpoint(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	seed := parkedRunSeed{
		loopID: "loop_retry_escape_manual", runID: "run_parked_resolve_comments", seq: 1, prNumber: 42,
		loopStatus: "queued", runStatus: "failed",
		currentStep: stepResolveComments, lastStep: stepPush,
		checkpoint: invalidCompletionCheckpoint(t, nowISO, "wt-42", "feature/fix-42", "upstream server_error"),
	}
	checkpointJSON := seedParkedRun(t, fixture, seed)

	// Before retry, the parked checkpoint's resume policy is manual_intervention so
	// loops.ShouldRestartFromDiscover returns false and createRunContext would resume
	// at the next step (resolve-comments) where validateFixerResumeCheckpoint re-parks.
	preCheckpoint := parseCheckpoint(&checkpointJSON)
	if loops.ShouldRestartFromDiscover("failed", preCheckpoint.ResumePolicy) {
		t.Fatalf("pre-retry ShouldRestartFromDiscover = true, want false for manual_intervention policy")
	}
	nextStep := workflow.Next(stepPush)
	if preResumeErr := validateFixerResumeCheckpoint(nextStep, preCheckpoint); preResumeErr == nil {
		t.Fatalf("pre-retry validateFixerResumeCheckpoint(%s) = nil, want re-park on invalid repair", nextStep)
	}

	rewrote, err := MarkInvalidCompletionRunRestartFromDiscover(context.Background(), fixture.repos, seed.loopID, nowISO)
	if err != nil || !rewrote {
		t.Fatalf("MarkInvalidCompletionRunRestartFromDiscover() = (%v, %v), want rewrote checkpoint", rewrote, err)
	}
	got := resumePolicyOf(t, fixture, seed.runID)
	if got != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover after operator retry", got)
	}
	// After retry, ShouldRestartFromDiscover returns true so createRunContext starts
	// at discover, escaping the parked downstream step.
	if !loops.ShouldRestartFromDiscover("failed", got) {
		t.Fatalf("post-retry ShouldRestartFromDiscover = false, want true for restart_from_discover policy")
	}

	assertNoEscape(t, fixture, parkedRunSeed{
		loopID: "loop_retry_escape_manual_other", runID: "run_advanced", seq: 2, prNumber: 42,
		loopStatus: "queued", runStatus: "failed",
		currentStep: stepValidate, lastStep: stepPush,
		checkpoint: fixerCheckpoint{ResumePolicy: loops.ResumePolicyAdvanceFromCheckpoint, Repair: &checkpointRepair{ParseStatus: "parsed", Summary: "ok"}},
	}, "a run that is not parked for manual intervention")
}

// TestOperatorRetryRewritesInterruptedManualInterventionCheckpoint covers the
// interrupted predecessor case: the retry API exposes interrupted manual runs as
// retryable, and createRunContext resumes an interrupted predecessor at the same
// downstream step where validateFixerResumeCheckpoint re-parks.
func TestOperatorRetryRewritesInterruptedManualInterventionCheckpoint(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	seed := parkedRunSeed{
		loopID: "loop_retry_escape_interrupted", runID: "run_interrupted_manual", seq: 3, prNumber: 45,
		loopStatus: "paused", runStatus: "interrupted",
		currentStep: stepReconcileCommits, lastStep: stepRepair,
		checkpoint: invalidCompletionCheckpoint(t, nowISO, "wt-45", "feature/fix-45", "interrupted before resolve"),
	}
	seedParkedRun(t, fixture, seed)

	rewrote, err := MarkInvalidCompletionRunRestartFromDiscover(context.Background(), fixture.repos, seed.loopID, nowISO)
	if err != nil || !rewrote {
		t.Fatalf("MarkInvalidCompletionRunRestartFromDiscover() = (%v, %v), want rewrote interrupted manual-intervention checkpoint", rewrote, err)
	}
	got := resumePolicyOf(t, fixture, seed.runID)
	if got != loops.ResumePolicyRestartFromDiscover {
		t.Fatalf("ResumePolicy = %q, want restart_from_discover after operator retry of interrupted run", got)
	}
	if !loops.ShouldRestartFromDiscover("interrupted", got) {
		t.Fatalf("ShouldRestartFromDiscover(interrupted) = false, want true for restart_from_discover policy")
	}

	assertNoEscape(t, fixture, parkedRunSeed{
		loopID: "loop_retry_escape_interrupted_other", runID: "run_interrupted_advance", seq: 4, prNumber: 45,
		loopStatus: "paused", runStatus: "interrupted",
		currentStep: stepValidate, lastStep: stepPush,
		checkpoint: fixerCheckpoint{ResumePolicy: loops.ResumePolicyAdvanceFromCheckpoint, Repair: &checkpointRepair{ParseStatus: "parsed", Summary: "ok"}},
	}, "an interrupted run that is not parked for manual intervention")
}

// TestOperatorRetryPreservesParksWithTheirOwnCause is the regression guard for the
// escape's scope. manual_intervention is also the policy for risky conflicts, dirty
// worktrees, and auto-commit-disabled parks; each names a condition the operator
// must clear outside Looper. Rewriting those to restart_from_discover would discard
// the park and rerun discovery while the blocking condition still holds.
func TestOperatorRetryPreservesParksWithTheirOwnCause(t *testing.T) {
	t.Parallel()
	nowISO := newRunnerFixture(t).nowISO()
	for index, testCase := range []struct {
		name   string
		reason checkpointPauseReason
	}{
		{name: "risky_conflict", reason: checkpointPauseReasonRiskyConflict},
		{name: "dirty_worktree", reason: checkpointPauseReasonDirtyWorktree},
		{name: "auto_commit_disabled", reason: checkpointPauseReasonAutoCommitDisabled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			checkpoint := invalidCompletionCheckpoint(t, nowISO, "wt-cause", "feature/fix-cause", "parked for "+testCase.name)
			checkpoint.Pause = newCheckpointPause(testCase.reason, true, "head-1", "state-1", nil)
			assertNoEscape(t, fixture, parkedRunSeed{
				loopID: "loop_park_" + testCase.name, runID: "run_park_" + testCase.name,
				seq: int64(10 + index), prNumber: 50,
				loopStatus: "paused", runStatus: "failed",
				currentStep: stepResolveComments, lastStep: stepPush,
				checkpoint: checkpoint,
			}, "a park whose pause reason names its own cause ("+testCase.name+")")
		})
	}
}

// TestOperatorRetryPreservesPreRepairManualPark guards the missing-record half of
// the predicate. Before repair runs, a nil repair record is simply work that had not
// started, not a broken completion contract, so a manual park there keeps its cause.
func TestOperatorRetryPreservesPreRepairManualPark(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	assertNoEscape(t, fixture, parkedRunSeed{
		loopID: "loop_park_pre_repair", runID: "run_park_pre_repair", seq: 20, prNumber: 51,
		loopStatus: "paused", runStatus: "failed",
		currentStep: stepPrepareWorktree, lastStep: stepCollectFixes,
		checkpoint: fixerCheckpoint{ResumePolicy: loops.ResumePolicyManualIntervention},
	}, "a manual park before repair, where a nil repair record proves nothing about the contract")
}

// TestValidateFixerResumeCheckpointAllowsSkippedRepair covers the skipped-repair
// resume path. Discovery can set SkipReason (ineligible PR, no remaining fix items)
// and runRepairStep then succeeds without creating a repair record, while every
// downstream step short-circuits on SkipReason. Demanding the completion contract
// on resume would park an interrupted skip for manual recovery forever even though
// no repair was ever authorized to run.
func TestValidateFixerResumeCheckpointAllowsSkippedRepair(t *testing.T) {
	t.Parallel()
	skipped := fixerCheckpoint{SkipReason: "no_remaining_fix_items"}
	for _, step := range []FixerStep{stepReconcileCommits, stepValidate, stepPush, stepResolveComments, stepRecheck} {
		if err := validateFixerResumeCheckpoint(step, skipped); err != nil {
			t.Fatalf("validateFixerResumeCheckpoint(%s, skipped) = %v, want nil for a skipped repair", step, err)
		}
	}
	// A non-skipped checkpoint with no repair record must still be rejected.
	if err := validateFixerResumeCheckpoint(stepResolveComments, fixerCheckpoint{}); err == nil {
		t.Fatalf("validateFixerResumeCheckpoint(resolve_comments, empty) = nil, want rejection of the missing contract")
	}
}

// TestProcessClaimedItemCheckpointsManualInterventionParkAtRepair pins the observable
// end state of a missing-completion-contract park: manual_intervention policy, the
// repair record kept as recovery evidence, and the run held at the repair step.
//
// This is an invariant guard, not a regression test. The repair-step checkpoint write
// added alongside it only changes behavior when the caller's completeRun write fails
// transiently, and there is no seam here to inject that failure -- both paths produce
// the same end state on a healthy write. The assertions still fail if the park stops
// preserving the record or stops classifying as manual intervention.
func TestProcessClaimedItemCheckpointsManualInterventionParkAtRepair(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{detail, detail},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
	}
	// Completed status with no parseable structured result: the missing completion
	// contract this park exists for.
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "did some work"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.FailureKind != runpipe.FailureManualIntervention {
		t.Fatalf("result = %#v, want manual-intervention failure for the missing completion contract", result)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
	}
	// The repair-step write is what pins the park to the repair step; without it the
	// run's step never advances past the step that was in progress.
	if got := derefString(run.CurrentStep); got != string(stepRepair) {
		t.Fatalf("run CurrentStep = %q, want %q from the repair-step checkpoint write", got, stepRepair)
	}
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.ResumePolicy != loops.ResumePolicyManualIntervention {
		t.Fatalf("checkpoint.ResumePolicy = %q, want manual_intervention", checkpoint.ResumePolicy)
	}
	if checkpoint.Repair == nil {
		t.Fatalf("checkpoint.Repair = nil, want the repair record persisted as recovery evidence")
	}
}
