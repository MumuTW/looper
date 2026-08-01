package fixer

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/storage"
)

func TestNextRoundBudgetChargesOnlyMovedHead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		previous    roundBudgetState
		hasPrevious bool
		headSHA     string
		wantRounds  int
		wantCharged bool
	}{
		{name: "first round", headSHA: "head-1", wantRounds: 1, wantCharged: true},
		{
			name:        "same head is the same round",
			previous:    roundBudgetState{Rounds: 2, LastHeadSHA: "head-2", FirstRoundAt: "t0"},
			hasPrevious: true,
			headSHA:     "head-2",
			wantRounds:  2,
		},
		{
			name:        "moved head is another round",
			previous:    roundBudgetState{Rounds: 2, LastHeadSHA: "head-2", FirstRoundAt: "t0"},
			hasPrevious: true,
			headSHA:     "head-3",
			wantRounds:  3,
			wantCharged: true,
		},
		{
			name:        "unknown head is never charged",
			previous:    roundBudgetState{Rounds: 2, LastHeadSHA: "head-2", FirstRoundAt: "t0"},
			hasPrevious: true,
			headSHA:     "  ",
			wantRounds:  2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			state, charged := nextRoundBudget(test.previous, test.hasPrevious, test.headSHA, "t1")
			if charged != test.wantCharged || state.Rounds != test.wantRounds {
				t.Fatalf("nextRoundBudget() = (%#v, %v), want rounds %d charged %v", state, charged, test.wantRounds, test.wantCharged)
			}
			if test.wantCharged && test.hasPrevious && state.FirstRoundAt != test.previous.FirstRoundAt {
				t.Fatalf("FirstRoundAt = %q, want preserved %q", state.FirstRoundAt, test.previous.FirstRoundAt)
			}
		})
	}
}

func TestNewDefaultsMaxFixerRounds(t *testing.T) {
	t.Parallel()
	runner := New(Options{})
	if runner.maxFixerRounds != maxFixerRoundsPerPullRequest {
		t.Fatalf("maxFixerRounds = %d, want %d", runner.maxFixerRounds, maxFixerRoundsPerPullRequest)
	}
	custom := New(Options{MaxFixerRoundsPerPullRequest: 3})
	if custom.maxFixerRounds != 3 {
		t.Fatalf("maxFixerRounds = %d, want 3", custom.maxFixerRounds)
	}
}

// seedRoundBudgetLoop creates a queued fixer loop for one pull request.
func seedRoundBudgetLoop(t *testing.T, fixture *runnerFixture, id, repo string, prNumber int64) storage.LoopRecord {
	t.Helper()
	loopTarget := buildPullRequestTargetID(repo, prNumber)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: id, Seq: 901, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &loopTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return loop
}

// TestRoundBudgetParksProductiveNonConvergingLoop is the PR #495 shape: every
// round pushes a new head and draws fresh review, so no existing gate fires.
func TestRoundBudgetParksProductiveNonConvergingLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(495)
	seedRoundBudgetLoop(t, fixture, "loop_round_budget", repo, prNumber)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, MaxFixerRoundsPerPullRequest: 3, Logger: fixture.logger, Now: fixture.now})

	project := storage.ProjectRecord{ID: "project_1"}
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", ThreadFingerprint: "fingerprint-1"}}
	heads := []string{"head-1", "head-2", "head-3"}
	for _, head := range heads {
		result, err := runner.ensureLoopForPullRequest(context.Background(), project, repo, prNumber, head, "fix-hash-"+head, "state-"+head, fixItems, []string{"t1"})
		if err != nil {
			t.Fatalf("head %s ensureLoopForPullRequest() error = %v", head, err)
		}
		if result.skipped || result.record.Status != "queued" {
			t.Fatalf("head %s = %#v, want queued within budget", head, result)
		}
	}

	result, err := runner.ensureLoopForPullRequest(context.Background(), project, repo, prNumber, "head-4", "fix-hash-4", "state-4", fixItems, []string{"t1"})
	if err != nil {
		t.Fatalf("over-budget ensureLoopForPullRequest() error = %v", err)
	}
	if !result.skipped {
		t.Fatalf("over-budget result = %#v, want skipped", result)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), "loop_round_budget")
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	if persisted.Status != "paused" {
		t.Fatalf("loop status = %q, want paused", persisted.Status)
	}
	metadata := parseJSONObject(persisted.MetadataJSON)
	if reason, _ := stringFromAny(metadata["pauseReason"]); reason != roundBudgetPauseReason {
		t.Fatalf("pauseReason = %q, want %q", reason, roundBudgetPauseReason)
	}
	state, ok := parseRoundBudgetState(metadata)
	if !ok || state.Rounds != 4 {
		t.Fatalf("round budget = (%#v, %v), want 4 rounds recorded", state, ok)
	}
}

// TestRoundBudgetPauseSurvivesNewFeedback pins the asymmetry against the sibling
// gates: a new head resumes a zero-progress or failure-streak pause because it is
// new evidence, but here a steady supply of new heads is the condition being
// stopped, so it must not unpark the loop.
func TestRoundBudgetPauseSurvivesNewFeedback(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(496)
	seedRoundBudgetLoop(t, fixture, "loop_round_budget_stuck", repo, prNumber)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, MaxFixerRoundsPerPullRequest: 1, Logger: fixture.logger, Now: fixture.now})

	project := storage.ProjectRecord{ID: "project_1"}
	fixItems := []FixItem{{Type: "comment", ID: "c1", ThreadID: "t1", ThreadFingerprint: "fingerprint-1"}}
	if _, err := runner.ensureLoopForPullRequest(context.Background(), project, repo, prNumber, "head-1", "hash-1", "state-1", fixItems, []string{"t1"}); err != nil {
		t.Fatalf("first round error = %v", err)
	}
	if _, err := runner.ensureLoopForPullRequest(context.Background(), project, repo, prNumber, "head-2", "hash-2", "state-2", fixItems, []string{"t1"}); err != nil {
		t.Fatalf("second round error = %v", err)
	}

	// Fresh head and fresh fix-items state: new evidence by every other gate's
	// definition, and still not a reason to keep going.
	result, err := runner.ensureLoopForPullRequest(context.Background(), project, repo, prNumber, "head-3", "hash-3", "state-3", fixItems, []string{"t1"})
	if err != nil {
		t.Fatalf("post-pause ensureLoopForPullRequest() error = %v", err)
	}
	if result.record.Status != "paused" {
		t.Fatalf("status = %q, want still paused", result.record.Status)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), "loop_round_budget_stuck")
	if err != nil || persisted == nil || persisted.Status != "paused" {
		t.Fatalf("persisted loop = (%#v, %v), want paused", persisted, err)
	}
}

// TestClearRoundBudgetResetsConvergedPullRequest covers the only automatic reset:
// the pull request ran out of fix items, so the loop demonstrably converged.
func TestClearRoundBudgetResetsConvergedPullRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(497)
	loop := seedRoundBudgetLoop(t, fixture, "loop_round_budget_converged", repo, prNumber)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, MaxFixerRoundsPerPullRequest: 2, Logger: fixture.logger, Now: fixture.now})

	updated, err := runner.mergeLoopMetadata(context.Background(), loop, map[string]any{
		"fixerRoundBudget": roundBudgetState{Rounds: 3, LastHeadSHA: "head-3", FirstRoundAt: fixture.nowISO(), RecordedAt: fixture.nowISO()},
		"pauseReason":      roundBudgetPauseReason,
	})
	if err != nil {
		t.Fatalf("mergeLoopMetadata() error = %v", err)
	}
	if _, err := runner.updateLoop(context.Background(), updated, func(mutated *storage.LoopRecord) {
		mutated.Status = "paused"
		mutated.NextRunAt = nil
	}); err != nil {
		t.Fatalf("updateLoop() error = %v", err)
	}

	if err := runner.clearRoundBudgetForPullRequest(context.Background(), "project_1", repo, prNumber); err != nil {
		t.Fatalf("clearRoundBudgetForPullRequest() error = %v", err)
	}

	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	metadata := parseJSONObject(persisted.MetadataJSON)
	if _, ok := parseRoundBudgetState(metadata); ok {
		t.Fatalf("round budget survived reset: %v", metadata)
	}
	if reason, _ := stringFromAny(metadata["pauseReason"]); reason != "" {
		t.Fatalf("pauseReason = %q, want cleared", reason)
	}
	if persisted.Status != "queued" {
		t.Fatalf("status = %q, want queued", persisted.Status)
	}
}

// TestClearRoundBudgetLeavesForeignPause makes the reset narrow: converging is
// evidence about this gate only, so another gate's pause stays put.
func TestClearRoundBudgetLeavesForeignPause(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(498)
	loop := seedRoundBudgetLoop(t, fixture, "loop_round_budget_foreign", repo, prNumber)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})

	updated, err := runner.mergeLoopMetadata(context.Background(), loop, map[string]any{
		"fixerRoundBudget": roundBudgetState{Rounds: 2, LastHeadSHA: "head-2", RecordedAt: fixture.nowISO()},
		"pauseReason":      failureStreakPauseReason,
	})
	if err != nil {
		t.Fatalf("mergeLoopMetadata() error = %v", err)
	}
	if _, err := runner.updateLoop(context.Background(), updated, func(mutated *storage.LoopRecord) {
		mutated.Status = "paused"
	}); err != nil {
		t.Fatalf("updateLoop() error = %v", err)
	}

	if err := runner.clearRoundBudgetForPullRequest(context.Background(), "project_1", repo, prNumber); err != nil {
		t.Fatalf("clearRoundBudgetForPullRequest() error = %v", err)
	}

	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v), want loop", persisted, err)
	}
	metadata := parseJSONObject(persisted.MetadataJSON)
	if _, ok := parseRoundBudgetState(metadata); ok {
		t.Fatalf("round budget survived reset: %v", metadata)
	}
	if reason, _ := stringFromAny(metadata["pauseReason"]); reason != failureStreakPauseReason {
		t.Fatalf("pauseReason = %q, want %q preserved", reason, failureStreakPauseReason)
	}
	if persisted.Status != "paused" {
		t.Fatalf("status = %q, want still paused", persisted.Status)
	}
}
