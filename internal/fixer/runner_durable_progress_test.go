package fixer

import (
	"testing"

	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/storage"
)

// Durable progress counts only effects that outlived the run, attributed to the run
// that produced them. The attribution is the whole point: a retry that reports its
// predecessor's push is worse than reporting nothing, because "partial success" then
// stops distinguishing anything.

const (
	runStart = "2026-04-11T12:00:00.000Z"
	before   = "2026-04-11T11:00:00.000Z"
	after    = "2026-04-11T12:30:00.000Z"
)

func TestRefreshOutcomeProgressCountsOnlyThisRunsEffects(t *testing.T) {
	t.Parallel()

	checkpoint := fixerCheckpoint{
		RunStartedAt:     runStart,
		ReconcileCommits: &checkpointReconcileCommits{CompletedAt: after, NewCommitSHAs: []string{"sha1"}},
		Push:             &checkpointPush{Pushed: true, PushedAt: after},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{
			{UpdatedAt: after, ReplyState: "sent", Status: "resolved"},
			{UpdatedAt: after, Status: "agent_declined"},
			{UpdatedAt: before, ReplyState: "sent", Status: "resolved"}, // predecessor's
		}},
	}
	checkpoint.refreshOutcomeProgress()

	got := checkpoint.Outcome.Progress
	if !got.CommitProduced || !got.Pushed {
		t.Fatalf("Progress = %#v, want this run's commit and push counted", got)
	}
	if got.RepliesSent != 1 {
		t.Fatalf("RepliesSent = %d, want 1: the earlier reply belongs to a predecessor", got.RepliesSent)
	}
	if got.ThreadsResolved != 2 {
		t.Fatalf("ThreadsResolved = %d, want 2 (resolved + agent_declined) from this run", got.ThreadsResolved)
	}
	if !checkpoint.Outcome.PartialSuccess {
		t.Fatal("PartialSuccess = false, want true when durable effects exist")
	}
}

func TestRefreshOutcomeProgressExcludesPredecessorEffects(t *testing.T) {
	t.Parallel()

	checkpoint := fixerCheckpoint{
		RunStartedAt:     runStart,
		ReconcileCommits: &checkpointReconcileCommits{CompletedAt: before, NewCommitSHAs: []string{"sha1"}},
		Push:             &checkpointPush{Pushed: true, PushedAt: before},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{
			{UpdatedAt: before, ReplyState: "sent", Status: "resolved"},
		}},
	}
	checkpoint.refreshOutcomeProgress()

	if got := checkpoint.Outcome.Progress; got != (FixerDurableProgress{}) {
		t.Fatalf("Progress = %#v, want empty: every effect predates this run", got)
	}
	if checkpoint.Outcome.PartialSuccess {
		t.Fatal("PartialSuccess = true, want false when this run achieved nothing")
	}
}

func TestOutcomeEvidenceInCurrentRunEdges(t *testing.T) {
	t.Parallel()

	withBaseline := fixerCheckpoint{RunStartedAt: runStart}
	if !withBaseline.outcomeEvidenceInCurrentRun(runStart) {
		t.Fatal("an effect exactly at the run start belongs to the run")
	}
	if withBaseline.outcomeEvidenceInCurrentRun("") {
		t.Fatal("an undated effect must not be credited to the current run")
	}
	if withBaseline.outcomeEvidenceInCurrentRun(before) {
		t.Fatal("an effect before the run start belongs to a predecessor")
	}

	// A checkpoint predating run-start bookkeeping: counting an unattributable
	// effect is better than discarding evidence that something happened.
	legacy := fixerCheckpoint{}
	if !legacy.outcomeEvidenceInCurrentRun(before) {
		t.Fatal("with no baseline the evidence should still count")
	}

	// RunPreStartAt is the fallback baseline.
	preStart := fixerCheckpoint{RunPreStartAt: runStart}
	if preStart.outcomeEvidenceInCurrentRun(before) {
		t.Fatal("RunPreStartAt should act as the baseline when RunStartedAt is unset")
	}
}

// TestMergeProgressFromKeepsNewerInMemoryEvidence covers the case where a run made
// durable changes and then failed before persisting them: the reloaded checkpoint is
// older than reality, and reporting it verbatim would claim zero progress.
func TestMergeProgressFromKeepsNewerInMemoryEvidence(t *testing.T) {
	t.Parallel()

	stored := fixerCheckpoint{
		ReconcileCommits: &checkpointReconcileCommits{CompletedAt: before},
		Push:             &checkpointPush{Pushed: false, PushedAt: before},
	}
	inMemory := fixerCheckpoint{
		ReconcileCommits: &checkpointReconcileCommits{CompletedAt: after, NewCommitSHAs: []string{"sha1"}},
		Push:             &checkpointPush{Pushed: true, PushedAt: after},
		ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{{UpdatedAt: after, ReplyState: "sent"}}},
	}
	stored.mergeProgressFrom(inMemory)

	if stored.ReconcileCommits.CompletedAt != after || !stored.Push.Pushed {
		t.Fatalf("merged = %#v / %#v, want the newer in-memory evidence", stored.ReconcileCommits, stored.Push)
	}
	if stored.ResolvedComments == nil {
		t.Fatal("resolved comments absent from storage should be adopted from memory")
	}

	// The reverse direction must not regress newer stored evidence.
	newerStored := fixerCheckpoint{Push: &checkpointPush{Pushed: true, PushedAt: after}}
	newerStored.mergeProgressFrom(fixerCheckpoint{Push: &checkpointPush{Pushed: false, PushedAt: before}})
	if !newerStored.Push.Pushed {
		t.Fatal("older in-memory evidence must not overwrite newer stored evidence")
	}
}

// TestDeriveRunOutcomeReconstructsHistoricalProgress is the read-path half: a run
// recorded before outcomes existed still reports what it accomplished.
func TestDeriveRunOutcomeReconstructsHistoricalProgress(t *testing.T) {
	t.Parallel()

	checkpointJSON := `{"runStartedAt":"` + runStart + `","fixItems":[{"id":"c1"}],` +
		`"push":{"pushed":true,"pushedAt":"` + after + `"}}`
	run := storage.RunRecord{
		ID: "run_1", LoopID: "loop_1", Status: "failed",
		CurrentStep: runpipe.StringPtr(string(stepResolveComments)), CheckpointJSON: runpipe.StringPtr(checkpointJSON),
		ErrorMessage: runpipe.StringPtr("resolve failed"),
	}

	got := DeriveRunOutcome(run)
	if got == nil {
		t.Fatal("DeriveRunOutcome() = nil, want a derived outcome")
	}
	if !got.Progress.Pushed {
		t.Fatalf("Progress = %#v, want the historical push reconstructed", got.Progress)
	}
	if !got.PartialSuccess {
		t.Fatal("PartialSuccess = false, want true for a failure that had pushed")
	}
	if got.PrimaryFailure == nil || got.PrimaryFailure.Step != string(stepResolveComments) {
		t.Fatalf("PrimaryFailure = %#v, want the failing step preserved", got.PrimaryFailure)
	}
}
