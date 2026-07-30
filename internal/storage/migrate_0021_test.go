package storage

import (
	"context"
	"reflect"
	"testing"
)

// 0004 made (project_id, branch) unique across the whole worktrees table, which
// is exactly what two live generations for one branch would violate. 0021 must
// narrow that to the live generation without ever permitting two live rows.
func TestMigration0021ScopesWorktreeBranchUniquenessToTheLiveGeneration(t *testing.T) {
	t.Parallel()

	if len(EmbeddedMigrations) < 21 || EmbeddedMigrations[20].ID != "0021_worktree_generation" {
		t.Fatalf("EmbeddedMigrations[20] = %#v, want 0021_worktree_generation", EmbeddedMigrations[20])
	}

	ctx := context.Background()
	db := openTestSQLiteDB(t)
	seedRunner := NewMigrationRunner(db, MigrationRunnerOptions{Migrations: EmbeddedMigrations[:20]})
	if _, err := seedRunner.RunPending(ctx); err != nil {
		t.Fatalf("seed RunPending() error = %v", err)
	}

	now := "2026-07-30T12:00:00.000Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO projects (id, name, repo_path, created_at, updated_at)
		VALUES ('project_0021', 'Looper', '/tmp/looper', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("insert project error = %v", err)
	}
	// Pre-0021 shape: no generation column yet.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, status, created_at, updated_at)
		VALUES ('wt_gen_1', 'project_0021', '/tmp/looper', '/tmp/wt/looper-fix-pr-42', 'feature/fixer', 'active', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("insert pre-0021 worktree error = %v", err)
	}

	migrationRunner := NewMigrationRunner(db, MigrationRunnerOptions{Migrations: EmbeddedMigrations[:21]})
	result, err := migrationRunner.RunPending(ctx)
	if err != nil {
		t.Fatalf("RunPending() applying 0021 error = %v", err)
	}
	if !reflect.DeepEqual(result.AppliedIDs, []string{"0021_worktree_generation"}) {
		t.Fatalf("RunPending().AppliedIDs = %v, want [0021_worktree_generation]", result.AppliedIDs)
	}

	repos := NewRepositories(db)
	existing, err := repos.Worktrees.GetByID(ctx, "wt_gen_1")
	if err != nil || existing == nil {
		t.Fatalf("GetByID() = %#v, %v", existing, err)
	}
	if existing.Generation != 1 || existing.RetiredAt != nil {
		t.Fatalf("migrated row = %#v, want generation 1 and no retirement", existing)
	}

	// A second live generation for the same branch is still rejected.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, status, created_at, updated_at, generation)
		VALUES ('wt_gen_2_live', 'project_0021', '/tmp/looper', '/tmp/wt/looper-fix-pr-42-g2', 'feature/fixer', 'active', ?, ?, 2)
	`, now, now); err == nil {
		t.Fatal("insert of a second live generation succeeded, want the partial unique index to reject it")
	}

	// Retiring the first makes room for the next generation.
	if err := repos.Worktrees.Retire(ctx, "wt_gen_1", now); err != nil {
		t.Fatalf("Retire() error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, status, created_at, updated_at, generation)
		VALUES ('wt_gen_2_live', 'project_0021', '/tmp/looper', '/tmp/wt/looper-fix-pr-42-g2', 'feature/fixer', 'active', ?, ?, 2)
	`, now, now); err != nil {
		t.Fatalf("insert of generation 2 after retirement error = %v", err)
	}

	live, err := repos.Worktrees.GetByBranch(ctx, "project_0021", "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if live == nil || live.ID != "wt_gen_2_live" {
		t.Fatalf("GetByBranch() = %#v, want the live generation only", live)
	}
	next, err := repos.Worktrees.NextGenerationForBranch(ctx, "project_0021", "feature/fixer")
	if err != nil {
		t.Fatalf("NextGenerationForBranch() error = %v", err)
	}
	if next != 3 {
		t.Fatalf("NextGenerationForBranch() = %d, want 3", next)
	}
	retired, err := repos.Worktrees.ListRetired(ctx)
	if err != nil {
		t.Fatalf("ListRetired() error = %v", err)
	}
	if len(retired) != 1 || retired[0].ID != "wt_gen_1" {
		t.Fatalf("ListRetired() = %#v, want the retired generation", retired)
	}
}
