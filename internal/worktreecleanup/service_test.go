package worktreecleanup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestPlanDryRunAppliesRetentionAndMaxPerTickWithoutDeleting(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.addReferencedWorktree(t, "wt_old_1", "loop_old_1", "completed", "success", fixture.daysAgo(10))
	fixture.addReferencedWorktree(t, "wt_old_2", "loop_old_2", "stopped", "success", fixture.daysAgo(9))
	fixture.addReferencedWorktree(t, "wt_old_3", "loop_old_3", "terminated", "cancelled", fixture.daysAgo(8))

	plan := fixture.plan(t, config.WorktreeCleanupConfig{DryRun: true, RetentionDays: 7, MaxPerTick: 2, IncludeOrphans: false})

	if !plan.DryRun {
		t.Fatal("Plan.DryRun = false, want true")
	}
	if plan.Summary.Scanned != 3 || plan.Summary.Candidates != 3 || plan.Summary.WouldClean != 2 || plan.Summary.Skipped != 1 || plan.Summary.Failed != 0 {
		t.Fatalf("Summary = %#v, want scanned=3 candidates=3 wouldClean=2 skipped=1 failed=0", plan.Summary)
	}
	assertDecision(t, plan, "wt_old_1", DecisionWouldClean, "")
	assertDecision(t, plan, "wt_old_2", DecisionWouldClean, "")
	assertDecision(t, plan, "wt_old_3", DecisionSkipped, "max_per_tick")

	for _, id := range []string{"wt_old_1", "wt_old_2"} {
		record, err := fixture.repos.Worktrees.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("Worktrees.GetByID(%s) error = %v", id, err)
		}
		if record == nil || record.CleanedAt != nil {
			t.Fatalf("worktree %s cleaned_at = %v, want untouched dry-run record", id, record)
		}
	}
}

func TestPlanProtectsRecoverableLoopsRunningRunsAndActiveQueueItems(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	for _, status := range []string{"queued", "running", "waiting", "paused", "failed", "interrupted"} {
		id := "wt_" + status
		fixture.addReferencedWorktree(t, id, "loop_"+status, status, "success", fixture.daysAgo(10))
	}
	fixture.addReferencedWorktree(t, "wt_running_run", "loop_running_run", "completed", "running", fixture.daysAgo(10))
	fixture.addReferencedWorktree(t, "wt_active_queue", "loop_active_queue", "completed", "success", fixture.daysAgo(10))
	fixture.addQueueItem(t, "queue_active", "loop_active_queue", "queued", fixture.daysAgo(1))

	plan := fixture.plan(t, config.WorktreeCleanupConfig{DryRun: true, RetentionDays: 7, MaxPerTick: 20, IncludeOrphans: false})

	if plan.Summary.WouldClean != 0 || plan.Summary.Skipped != 8 {
		t.Fatalf("Summary = %#v, want no cleanable protected worktrees", plan.Summary)
	}
	for _, status := range []string{"queued", "running", "waiting", "paused", "failed", "interrupted"} {
		assertDecision(t, plan, "wt_"+status, DecisionSkipped, "protected_loop_status:"+status)
	}
	assertDecision(t, plan, "wt_running_run", DecisionSkipped, "running_run")
	assertDecision(t, plan, "wt_active_queue", DecisionSkipped, "active_queue_item")
}

func TestPlanAppliesRetentionToTerminalWork(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.addReferencedWorktree(t, "wt_recent", "loop_recent", "completed", "success", fixture.daysAgo(2))
	fixture.addReferencedWorktree(t, "wt_old", "loop_old", "completed", "success", fixture.daysAgo(9))

	plan := fixture.plan(t, config.WorktreeCleanupConfig{DryRun: true, RetentionDays: 7, MaxPerTick: 10, IncludeOrphans: false})

	assertDecision(t, plan, "wt_recent", DecisionSkipped, "retention")
	assertDecision(t, plan, "wt_old", DecisionWouldClean, "")
}

func TestPlanSkipsAffectedCandidatesOnCheckpointParseFailure(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.addWorktree(t, "wt_bad_project", fixture.daysAgo(10))
	fixture.addLoop(t, "loop_bad", "completed", fixture.daysAgo(10))
	bad := "{not-json"
	fixture.addRun(t, "run_bad", "loop_bad", "success", &bad, fixture.daysAgo(10))

	plan := fixture.plan(t, config.WorktreeCleanupConfig{DryRun: true, RetentionDays: 7, MaxPerTick: 10, IncludeOrphans: true})

	if plan.Summary.Failed != 1 || len(plan.Failed) != 1 {
		t.Fatalf("Failed = %#v / summary %#v, want one parse failure", plan.Failed, plan.Summary)
	}
	assertDecision(t, plan, "wt_bad_project", DecisionSkipped, "checkpoint_parse_failed")
}

func TestPlanReportsOrphansButDoesNotSelectThemByDefault(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.addWorktree(t, "wt_orphan", fixture.daysAgo(10))

	plan := fixture.plan(t, config.WorktreeCleanupConfig{DryRun: true, RetentionDays: 7, MaxPerTick: 10, IncludeOrphans: false})

	if plan.Summary.Scanned != 1 || plan.Summary.WouldClean != 0 {
		t.Fatalf("Summary = %#v, want scanned orphan but not selected", plan.Summary)
	}
	assertDecision(t, plan, "wt_orphan", DecisionSkipped, "orphan")
}

func TestPlanCanIncludeOrphans(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	fixture.addWorktree(t, "wt_orphan", fixture.daysAgo(10))

	plan := fixture.plan(t, config.WorktreeCleanupConfig{DryRun: true, RetentionDays: 7, MaxPerTick: 10, IncludeOrphans: true})

	assertDecision(t, plan, "wt_orphan", DecisionWouldClean, "")
}

type fixture struct {
	repos *storage.Repositories
	now   time.Time
	seq   int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Migrations: storage.EmbeddedMigrations,
		BackupDir:  filepath.Join(root, "backups"),
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if err := coordinator.Close(); err != nil {
			t.Fatalf("coordinator.Close() error = %v", err)
		}
	})
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	nowISO := formatTestTime(now)
	baseBranch := "main"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID:         "project_1",
		Name:       "Project 1",
		RepoPath:   "/repos/project_1",
		BaseBranch: &baseBranch,
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	return &fixture{repos: repos, now: now}
}

func (f *fixture) plan(t *testing.T, cfg config.WorktreeCleanupConfig) Plan {
	t.Helper()
	plan, err := (&Service{Repos: f.repos, Config: cfg, Now: func() time.Time { return f.now }}).Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	return plan
}

func (f *fixture) addReferencedWorktree(t *testing.T, worktreeID, loopID, loopStatus, runStatus string, at time.Time) {
	t.Helper()
	f.addWorktree(t, worktreeID, at)
	f.addLoop(t, loopID, loopStatus, at)
	checkpoint := `{"worktree":{"id":"` + worktreeID + `","path":"/tmp/` + worktreeID + `","branch":"branch-` + worktreeID + `"}}`
	f.addRun(t, "run_"+worktreeID, loopID, runStatus, &checkpoint, at)
}

func (f *fixture) addWorktree(t *testing.T, id string, at time.Time) {
	t.Helper()
	atISO := formatTestTime(at)
	baseBranch := "main"
	if err := f.repos.Worktrees.Upsert(context.Background(), storage.WorktreeRecord{
		ID:           id,
		ProjectID:    "project_1",
		RepoPath:     "/repos/project_1",
		WorktreePath: "/tmp/" + id,
		Branch:       "branch-" + id,
		BaseBranch:   &baseBranch,
		Status:       "active",
		CreatedAt:    atISO,
		UpdatedAt:    atISO,
	}); err != nil {
		t.Fatalf("Worktrees.Upsert(%s) error = %v", id, err)
	}
}

func (f *fixture) addLoop(t *testing.T, id, status string, at time.Time) {
	t.Helper()
	f.seq++
	atISO := formatTestTime(at)
	if err := f.repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         id,
		Seq:        f.seq,
		ProjectID:  "project_1",
		Type:       "worker",
		TargetType: "project",
		TargetID:   stringPtr("project_1"),
		Status:     status,
		LastRunAt:  &atISO,
		CreatedAt:  atISO,
		UpdatedAt:  atISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", id, err)
	}
}

func (f *fixture) addRun(t *testing.T, id, loopID, status string, checkpoint *string, at time.Time) {
	t.Helper()
	atISO := formatTestTime(at)
	if err := f.repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID:              id,
		LoopID:          loopID,
		Status:          status,
		CheckpointJSON:  checkpoint,
		StartedAt:       atISO,
		LastHeartbeatAt: &atISO,
		EndedAt:         &atISO,
		CreatedAt:       atISO,
		UpdatedAt:       atISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(%s) error = %v", id, err)
	}
}

func (f *fixture) addQueueItem(t *testing.T, id, loopID, status string, at time.Time) {
	t.Helper()
	atISO := formatTestTime(at)
	if err := f.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID:          id,
		ProjectID:   stringPtr("project_1"),
		LoopID:      &loopID,
		Type:        "worker",
		TargetType:  "project",
		TargetID:    "project_1",
		DedupeKey:   id,
		Priority:    1,
		Status:      status,
		AvailableAt: atISO,
		Attempts:    0,
		MaxAttempts: 3,
		CreatedAt:   atISO,
		UpdatedAt:   atISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}

func (f *fixture) daysAgo(days int) time.Time {
	return f.now.Add(-time.Duration(days) * 24 * time.Hour)
}

func assertDecision(t *testing.T, plan Plan, worktreeID, decision, reason string) {
	t.Helper()
	for _, item := range plan.Items {
		if item.Worktree.ID != worktreeID {
			continue
		}
		if item.Decision != decision {
			t.Fatalf("%s decision = %q, want %q (item %#v)", worktreeID, item.Decision, decision, item)
		}
		if reason != "" && item.Reason != reason {
			t.Fatalf("%s reason = %q, want %q", worktreeID, item.Reason, reason)
		}
		if reason == "" && item.Reason != "" {
			t.Fatalf("%s reason = %q, want empty", worktreeID, item.Reason)
		}
		return
	}
	ids := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		ids = append(ids, item.Worktree.ID)
	}
	t.Fatalf("worktree %s not found in plan items: %s", worktreeID, strings.Join(ids, ", "))
}

func formatTestTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func stringPtr(value string) *string {
	return &value
}
