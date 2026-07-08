package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

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

// Stage B (durable-marker control): once $.shepherd.active is set and the
// checkpoint has a PR, the start-step resolver forces stepShepherd REGARDLESS of
// loop.Status or the failed/interrupted resume gate — the v2 bug was keying on
// loop.Status, which the failure path moves to queued/paused. Here the prior run
// FAILED at validate (which would normally resume mid-sequence), yet the marker
// still routes the next run to stepShepherd and carries the PR forward.
func TestCreateRunContextForcesShepherdViaMarker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	meta, err := loops.WriteShepherd(nil, loops.Shepherd{Active: true, Phase: "reviewing"})
	if err != nil {
		t.Fatalf("WriteShepherd() error = %v", err)
	}
	prTargetID := "pr:acme/looper:101"
	prNumber := int64(101)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_shepherd_1", Seq: 7, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTargetID, Repo: stringPtr("acme/looper"), PRNumber: &prNumber, Status: "queued", MetadataJSON: &meta, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpointJSON := mustMarshalJSON(workerCheckpoint{
		Work:           &workerInput{Title: "Worker task", Repo: "acme/looper", IssueNumber: 42, ExecutionMode: "create-pr"},
		ClaimedLockKey: "pr:acme/looper:101",
		Worktree:       &checkpointWorktree{ID: "wt_1", Path: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature"},
		PullRequest:    &checkpointPullPR{Number: 101, URL: "https://example/pr/101"},
	})
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_prev", LoopID: "loop_shepherd_1", Status: "failed", CurrentStep: stringPtr(string(stepValidate)), LastCompletedStep: stringPtr(string(stepExecute)), CheckpointJSON: &checkpointJSON, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	rc, err := runner.createRunContext(context.Background(), loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if rc.StartStep != stepShepherd {
		t.Fatalf("StartStep = %q, want shepherd (durable marker must override a failed-at-validate resume)", rc.StartStep)
	}
	if rc.Checkpoint.PullRequest == nil || rc.Checkpoint.PullRequest.Number != 101 {
		t.Fatalf("shepherd run lost the PR checkpoint: %#v", rc.Checkpoint.PullRequest)
	}
}

// The shepherd prompt MUST forbid the bot from ever merging or self-approving —
// the final merge is a human colleague's action. This is a hard product rule
// (feedback_bot_never_merges_human_merges), guarded here so a prompt edit can't
// silently reintroduce a self-merge.
func TestBuildShepherdPromptForbidsMergeAndSelfApprove(t *testing.T) {
	p := buildShepherdPrompt(workerInput{Repo: "acme/looper"}, 101, false)
	for _, must := range []string{"NEVER merge", "gh pr merge", "auto", "NEVER submit an approving review", "human"} {
		if !strings.Contains(p, must) {
			t.Fatalf("shepherd prompt missing guardrail %q:\n%s", must, p)
		}
	}
	// sanity: it references the actual PR
	if !strings.Contains(p, "acme/looper#101") {
		t.Fatalf("shepherd prompt does not reference the PR: %s", p)
	}
	// session-lost path tells the agent to rebuild from the diff
	if !strings.Contains(buildShepherdPrompt(workerInput{Repo: "a/b"}, 1, true), "could not be recovered") {
		t.Fatal("session-lost prompt missing rebuild-from-diff instruction")
	}
}
