package worktreecleanup

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestPlanDryRunRetentionAndMaxPerTick(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repos := openRepos(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		seedWorktree(t, repos, storage.WorktreeRecord{
			ID:           fmt.Sprintf("wt_%d", i),
			ProjectID:    "project_1",
			RepoPath:     "/repo",
			WorktreePath: filepath.Join("/worktrees", fmt.Sprintf("wt_%d", i)),
			Branch:       fmt.Sprintf("feature/%d", i),
			Status:       "active",
			CreatedAt:    iso(now.Add(-20 * 24 * time.Hour)),
			UpdatedAt:    iso(now.Add(-20 * 24 * time.Hour)),
		})
		seedLoop(t, repos, fmt.Sprintf("loop_%d", i), "project_1", "completed", now.Add(-20*24*time.Hour))
		checkpoint := fmt.Sprintf(`{"worktree":{"id":"wt_%d","path":%q}}`, i, filepath.Join("/worktrees", fmt.Sprintf("wt_%d", i)))
		seedRun(t, repos, fmt.Sprintf("run_%d", i), fmt.Sprintf("loop_%d", i), "success", checkpoint, now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour))
	}

	service := Service{Repos: repos, Now: func() time.Time { return now }}
	plan, err := service.Plan(ctx, PlanOptions{Config: cleanupConfig(7, 2, true), DryRun: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	if plan.Scanned != 3 || plan.Candidates != 3 || plan.WouldClean != 2 || plan.Skipped != 1 || plan.Failed != 0 {
		t.Fatalf("Plan() summary = %#v, want scanned=3 candidates=3 would=2 skipped=1 failed=0", plan)
	}
	if got := countDecision(plan, DecisionWouldClean); got != 2 {
		t.Fatalf("would-clean count = %d, want 2", got)
	}
	if got := countReason(plan, "max_per_tick"); got != 1 {
		t.Fatalf("max_per_tick skips = %d, want 1", got)
	}
}

func TestPlanProtectsActiveRecoverableAndRunningReferences(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repos := openRepos(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	statuses := []string{"queued", "running", "waiting", "paused", "failed", "interrupted"}
	for i, status := range statuses {
		id := fmt.Sprintf("wt_%s", status)
		path := filepath.Join("/worktrees", id)
		seedWorktree(t, repos, storage.WorktreeRecord{ID: id, ProjectID: "project_1", RepoPath: "/repo", WorktreePath: path, Branch: "feature/" + status, Status: "active", CreatedAt: iso(now.Add(-30 * 24 * time.Hour)), UpdatedAt: iso(now.Add(-30 * 24 * time.Hour))})
		loopID := fmt.Sprintf("loop_%d", i)
		seedLoop(t, repos, loopID, "project_1", status, now.Add(-30*24*time.Hour))
		seedRun(t, repos, "run_status_"+status, loopID, "success", fmt.Sprintf(`{"worktree":{"id":%q,"path":%q}}`, id, path), now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))
	}
	seedWorktree(t, repos, storage.WorktreeRecord{ID: "wt_running_run", ProjectID: "project_1", RepoPath: "/repo", WorktreePath: "/worktrees/wt_running_run", Branch: "feature/run", Status: "active", CreatedAt: iso(now.Add(-30 * 24 * time.Hour)), UpdatedAt: iso(now.Add(-30 * 24 * time.Hour))})
	seedLoop(t, repos, "loop_completed_running_run", "project_1", "completed", now.Add(-30*24*time.Hour))
	seedRun(t, repos, "run_running", "loop_completed_running_run", "running", `{"worktree":{"id":"wt_running_run","path":"/worktrees/wt_running_run"}}`, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))
	seedQueue(t, repos, "queue_active", "loop_completed_running_run", "running", `{"worktreeId":"wt_running_run","worktreePath":"/worktrees/wt_running_run"}`, now.Add(-30*24*time.Hour))

	service := Service{Repos: repos, Now: func() time.Time { return now }}
	plan, err := service.Plan(ctx, PlanOptions{Config: cleanupConfig(7, 10, true), DryRun: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.WouldClean != 0 || plan.Skipped != 7 {
		t.Fatalf("Plan() = %#v, want all protected/skipped", plan)
	}
	for _, reason := range []string{"loop_queued", "loop_running", "loop_waiting", "loop_paused", "loop_failed", "loop_interrupted", "run_running"} {
		if countReason(plan, reason) == 0 {
			t.Fatalf("Plan() missing skip reason %q: %#v", reason, plan.Items)
		}
	}
}

func TestPlanAppliesRetentionBeforeCompletedWorkIsEligible(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repos := openRepos(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	seedWorktree(t, repos, storage.WorktreeRecord{ID: "wt_recent", ProjectID: "project_1", RepoPath: "/repo", WorktreePath: "/worktrees/recent", Branch: "feature/recent", Status: "active", CreatedAt: iso(now.Add(-2 * 24 * time.Hour)), UpdatedAt: iso(now.Add(-2 * 24 * time.Hour))})
	seedLoop(t, repos, "loop_recent", "project_1", "completed", now.Add(-2*24*time.Hour))
	seedRun(t, repos, "run_recent", "loop_recent", "success", `{"worktree":{"id":"wt_recent","path":"/worktrees/recent"}}`, now.Add(-2*24*time.Hour), now.Add(-2*24*time.Hour))

	service := Service{Repos: repos, Now: func() time.Time { return now }}
	plan, err := service.Plan(ctx, PlanOptions{Config: cleanupConfig(7, 10, true), DryRun: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.WouldClean != 0 || countReason(plan, "retention") != 1 {
		t.Fatalf("Plan() = %#v, want retention skip", plan)
	}
}

func TestPlanSkipsCheckpointParseFailuresConservatively(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repos := openRepos(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	seedWorktree(t, repos, storage.WorktreeRecord{ID: "wt_bad_checkpoint", ProjectID: "project_1", RepoPath: "/repo", WorktreePath: "/worktrees/bad", Branch: "feature/bad", Status: "active", CreatedAt: iso(now.Add(-30 * 24 * time.Hour)), UpdatedAt: iso(now.Add(-30 * 24 * time.Hour))})
	seedLoop(t, repos, "loop_bad", "project_1", "completed", now.Add(-30*24*time.Hour))
	seedRun(t, repos, "run_bad", "loop_bad", "success", `{`, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour))

	service := Service{Repos: repos, Now: func() time.Time { return now }}
	plan, err := service.Plan(ctx, PlanOptions{Config: cleanupConfig(7, 10, true), DryRun: true})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if plan.WouldClean != 0 || countReason(plan, "checkpoint_parse_failed") != 1 {
		t.Fatalf("Plan() = %#v, want checkpoint parse skip", plan)
	}
}

func TestPlanReportsOrphansAndRequiresIncludeOrphans(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repos := openRepos(t)
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	seedWorktree(t, repos, storage.WorktreeRecord{ID: "wt_orphan", ProjectID: "project_1", RepoPath: "/repo", WorktreePath: "/worktrees/orphan", Branch: "feature/orphan", Status: "active", CreatedAt: iso(now.Add(-30 * 24 * time.Hour)), UpdatedAt: iso(now.Add(-30 * 24 * time.Hour))})

	service := Service{Repos: repos, Now: func() time.Time { return now }}
	plan, err := service.Plan(ctx, PlanOptions{Config: cleanupConfig(7, 10, false), DryRun: true})
	if err != nil {
		t.Fatalf("Plan(includeOrphans=false) error = %v", err)
	}
	if plan.WouldClean != 0 || plan.Summary.Orphans != 1 || countReason(plan, "orphan") != 1 {
		t.Fatalf("Plan(includeOrphans=false) = %#v, want orphan reported but skipped", plan)
	}

	plan, err = service.Plan(ctx, PlanOptions{Config: cleanupConfig(7, 10, true), DryRun: true})
	if err != nil {
		t.Fatalf("Plan(includeOrphans=true) error = %v", err)
	}
	if plan.WouldClean != 1 {
		t.Fatalf("Plan(includeOrphans=true) = %#v, want orphan candidate", plan)
	}
}

func openRepos(t *testing.T) *storage.Repositories {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "runtime.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project 1", RepoPath: "/repo", CreatedAt: iso(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), UpdatedAt: iso(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}
	return repos
}

func cleanupConfig(retentionDays, maxPerTick int, includeOrphans bool) config.WorktreeCleanupConfig {
	return config.WorktreeCleanupConfig{Enabled: true, Interval: "24h", RetentionDays: retentionDays, MaxPerTick: maxPerTick, IncludeOrphans: includeOrphans, DryRun: true}
}

func seedWorktree(t *testing.T, repos *storage.Repositories, record storage.WorktreeRecord) {
	t.Helper()
	if err := repos.Worktrees.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Worktrees.Upsert(%s) error = %v", record.ID, err)
	}
}

func seedLoop(t *testing.T, repos *storage.Repositories, id, projectID, status string, updatedAt time.Time) {
	t.Helper()
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: id, Seq: time.Now().UnixNano(), ProjectID: projectID, Type: "worker", TargetType: "issue", Status: status, CreatedAt: iso(updatedAt), UpdatedAt: iso(updatedAt)}); err != nil {
		t.Fatalf("Loops.Upsert(%s) error = %v", id, err)
	}
}

func seedRun(t *testing.T, repos *storage.Repositories, id, loopID, status, checkpoint string, startedAt, updatedAt time.Time) {
	t.Helper()
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: id, LoopID: loopID, Status: status, CheckpointJSON: &checkpoint, StartedAt: iso(startedAt), CreatedAt: iso(startedAt), UpdatedAt: iso(updatedAt)}); err != nil {
		t.Fatalf("Runs.Upsert(%s) error = %v", id, err)
	}
}

func seedQueue(t *testing.T, repos *storage.Repositories, id, loopID, status, payload string, updatedAt time.Time) {
	t.Helper()
	projectID := "project_1"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: id, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "issue", TargetID: "issue:1", DedupeKey: id, Priority: 100, Status: status, AvailableAt: iso(updatedAt), MaxAttempts: 3, PayloadJSON: &payload, CreatedAt: iso(updatedAt), UpdatedAt: iso(updatedAt)}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}

func iso(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func countDecision(plan Plan, decision Decision) int {
	count := 0
	for _, item := range plan.Items {
		if item.Decision == decision {
			count++
		}
	}
	return count
}

func countReason(plan Plan, reason string) int {
	count := 0
	for _, item := range plan.Items {
		for _, part := range strings.Split(item.Reason, ",") {
			if part == reason {
				count++
			}
		}
	}
	return count
}
