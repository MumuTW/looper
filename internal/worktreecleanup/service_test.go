package worktreecleanup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestPlanAppliesRetentionAndDoesNotMutateWorktreesInDryRun(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.iso(fixture.now.AddDate(0, 0, -10))
	for _, status := range []string{"completed", "stopped", "terminated"} {
		wtID := "wt_" + status
		loopID := "loop_" + status
		fixture.insertWorktree(wtID, "feature/"+status, old)
		fixture.insertLoop(loopID, status, old)
		fixture.insertRun("run_"+status, loopID, "success", checkpoint(wtID, fixture.path("feature/"+status)), old, &old)
	}

	plan := fixture.plan(t, config.WorktreeCleanupConfig{RetentionDays: 7, MaxPerTick: 10, DryRun: true, IncludeOrphans: true})

	if plan.Summary.Scanned != 3 || plan.Summary.Candidates != 3 || plan.Summary.WouldClean != 3 || plan.Summary.Skipped != 0 || plan.Summary.Failed != 0 {
		t.Fatalf("summary = %#v, want terminal work to become would-clean candidates after retention", plan.Summary)
	}
	for _, item := range plan.Items {
		if item.Action != PlanActionWouldClean {
			t.Fatalf("item action = %q, want would_clean", item.Action)
		}
	}
	stored, err := fixture.repos.Worktrees.GetByID(context.Background(), "wt_completed")
	if err != nil {
		t.Fatalf("Worktrees.GetByID() error = %v", err)
	}
	if stored == nil || stored.Status != "active" || stored.CleanedAt != nil {
		t.Fatalf("stored worktree after dry run = %#v, want unchanged active record", stored)
	}
}

func TestPlanProtectsRecoverableLoopsRunningRunsAndActiveQueueItems(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.iso(fixture.now.AddDate(0, 0, -10))
	for _, status := range []string{"active", "queued", "running", "waiting", "paused", "failed", "interrupted"} {
		id := "wt_" + status
		loopID := "loop_" + status
		fixture.insertWorktree(id, "feature/"+status, old)
		fixture.insertLoop(loopID, status, old)
		fixture.insertRun("run_status_"+status, loopID, "success", checkpoint(id, fixture.path("feature/"+status)), old, &old)
	}
	fixture.insertWorktree("wt_running_run", "feature/run", old)
	fixture.insertLoop("loop_running_run", "completed", old)
	fixture.insertRun("run_running", "loop_running_run", "running", checkpoint("wt_running_run", fixture.path("feature/run")), old, nil)

	fixture.insertWorktree("wt_queue", "feature/queue", old)
	fixture.insertLoop("loop_queue", "completed", old)
	fixture.insertRun("run_queue", "loop_queue", "success", checkpoint("wt_queue", fixture.path("feature/queue")), old, &old)
	fixture.insertQueue("queue_active", "loop_queue", "queued", old)

	plan := fixture.plan(t, config.WorktreeCleanupConfig{RetentionDays: 7, MaxPerTick: 20, DryRun: true, IncludeOrphans: true})
	if plan.Summary.WouldClean != 0 || plan.Summary.Skipped != 9 {
		t.Fatalf("summary = %#v, want all protected worktrees skipped", plan.Summary)
	}
}

func TestPlanSkipsCheckpointParseFailuresConservatively(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.iso(fixture.now.AddDate(0, 0, -10))
	fixture.insertWorktree("wt_bad_checkpoint_project", "feature/bad", old)
	fixture.insertLoop("loop_bad", "completed", old)
	fixture.insertRun("run_bad", "loop_bad", "success", "{", old, &old)

	plan := fixture.plan(t, config.WorktreeCleanupConfig{RetentionDays: 7, MaxPerTick: 10, DryRun: true, IncludeOrphans: true})
	if plan.Summary.WouldClean != 0 || plan.Summary.Skipped != 1 {
		t.Fatalf("summary = %#v, want parse failure to skip affected project candidate", plan.Summary)
	}
	if plan.Items[0].Reason != "checkpoint parse failure in project" {
		t.Fatalf("reason = %q, want checkpoint parse failure", plan.Items[0].Reason)
	}
}

func TestPlanReportsOrphansButDefaultDoesNotSelectThem(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.iso(fixture.now.AddDate(0, 0, -10))
	fixture.insertWorktree("wt_orphan", "feature/orphan", old)

	plan := fixture.plan(t, config.WorktreeCleanupConfig{RetentionDays: 7, MaxPerTick: 10, DryRun: true, IncludeOrphans: false})
	if plan.Summary.Scanned != 1 || plan.Summary.Candidates != 0 || plan.Summary.WouldClean != 0 || plan.Summary.Skipped != 1 {
		t.Fatalf("summary = %#v, want orphan reported but skipped", plan.Summary)
	}
	if plan.Items[0].Reason != "orphan worktree" {
		t.Fatalf("reason = %q, want orphan worktree", plan.Items[0].Reason)
	}
}

func TestPlanHonorsMaxPerTick(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	old := fixture.iso(fixture.now.AddDate(0, 0, -10))
	for _, suffix := range []string{"a", "b", "c"} {
		wtID := "wt_" + suffix
		loopID := "loop_" + suffix
		fixture.insertWorktree(wtID, "feature/"+suffix, old)
		fixture.insertLoop(loopID, "completed", old)
		fixture.insertRun("run_"+suffix, loopID, "success", checkpoint(wtID, fixture.path("feature/"+suffix)), old, &old)
	}

	plan := fixture.plan(t, config.WorktreeCleanupConfig{RetentionDays: 7, MaxPerTick: 2, DryRun: true, IncludeOrphans: false})
	if plan.Summary.Candidates != 3 || plan.Summary.WouldClean != 2 || plan.Summary.Skipped != 1 {
		t.Fatalf("summary = %#v, want three candidates limited to two would-clean items", plan.Summary)
	}
}

type fixture struct {
	t       *testing.T
	repos   *storage.Repositories
	service Service
	now     time.Time
	root    string
	seq     int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	root := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
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
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	base := "main"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: filepath.Join(root, "repo"), BaseBranch: &base, CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	return fixture{
		t:       t,
		repos:   repos,
		service: Service{Repos: repos, Now: func() time.Time { return now }},
		now:     now,
		root:    root,
	}
}

func (f *fixture) plan(t *testing.T, cfg config.WorktreeCleanupConfig) Plan {
	t.Helper()
	plan, err := f.service.Plan(context.Background(), PlanOptions{Config: cfg})
	if err != nil {
		t.Fatalf("Service.Plan() error = %v", err)
	}
	return plan
}

func (f *fixture) insertWorktree(id, branch, at string) {
	f.t.Helper()
	if err := f.repos.Worktrees.Upsert(context.Background(), storage.WorktreeRecord{ID: id, ProjectID: "project_1", RepoPath: filepath.Join(f.root, "repo"), WorktreePath: f.path(branch), Branch: branch, Status: "active", CreatedAt: at, UpdatedAt: at}); err != nil {
		f.t.Fatalf("Worktrees.Upsert() error = %v", err)
	}
}

func (f *fixture) insertLoop(id, status, at string) {
	f.t.Helper()
	f.seq++
	if err := f.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: id, Seq: f.seq, ProjectID: "project_1", Type: "worker", TargetType: "issue", Status: status, CreatedAt: at, UpdatedAt: at}); err != nil {
		f.t.Fatalf("Loops.Upsert() error = %v", err)
	}
}

func (f *fixture) insertRun(id, loopID, status, checkpointJSON, at string, endedAt *string) {
	f.t.Helper()
	if err := f.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: id, LoopID: loopID, Status: status, CheckpointJSON: &checkpointJSON, StartedAt: at, EndedAt: endedAt, CreatedAt: at, UpdatedAt: at}); err != nil {
		f.t.Fatalf("Runs.Upsert() error = %v", err)
	}
}

func (f *fixture) insertQueue(id, loopID, status, at string) {
	f.t.Helper()
	if err := f.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: id, ProjectID: stringPtr("project_1"), LoopID: &loopID, Type: "worker", TargetType: "issue", TargetID: "issue:acme/looper:1", DedupeKey: id, Priority: 1, Status: status, AvailableAt: at, MaxAttempts: 3, CreatedAt: at, UpdatedAt: at}); err != nil {
		f.t.Fatalf("Queue.Upsert() error = %v", err)
	}
}

func (f *fixture) path(branch string) string {
	return filepath.Join(f.root, "worktrees", branch)
}

func (f *fixture) iso(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func checkpoint(id, path string) string {
	return `{"worktree":{"id":"` + id + `","path":"` + path + `"}}`
}

func stringPtr(value string) *string {
	return &value
}
