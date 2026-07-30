package storage

import (
	"context"
	"reflect"
	"testing"
)

// 0004 made (project_id, branch) unique across the whole worktrees table. 0021
// replaces it with uniqueness over the live generation OF ONE CHECKOUT, which
// has to be right in both directions: two generations of one checkout may never
// be live at once, and two different checkouts of the same branch must be able
// to be.
func TestMigration0021KeysLiveWorktreeUniquenessToTheCheckout(t *testing.T) {
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
	// Pre-0021 shape: no generation and no checkout key yet.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, status, created_at, updated_at)
		VALUES ('wt_gen_1', 'project_0021', '/tmp/looper', '/tmp/wt/looper-fix-p-pr-42-detached', 'feature/fixer', 'active', ?, ?)
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
	// The backfill derives the checkout key from the existing directory name.
	if existing.CheckoutKey != "looper-fix-p-pr-42-detached" {
		t.Fatalf("migrated row checkout key = %q, want the generation-1 directory name", existing.CheckoutKey)
	}

	// A second live generation OF THE SAME CHECKOUT is rejected.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, status, created_at, updated_at, generation, checkout_key)
		VALUES ('wt_gen_2_live', 'project_0021', '/tmp/looper', '/tmp/wt/looper-fix-p-pr-42-detached+g2', 'feature/fixer', 'active', ?, ?, 2, 'looper-fix-p-pr-42-detached')
	`, now, now); err == nil {
		t.Fatal("insert of a second live generation succeeded, want the partial unique index to reject it")
	}

	// A DIFFERENT checkout of the SAME branch is allowed: the attached planner
	// checkout and the detached PR checkout legitimately coexist, and keying
	// this by branch is what collapsed them into one row.
	if err := repos.Worktrees.Upsert(ctx, WorktreeRecord{
		ID: "wt_attached", ProjectID: "project_0021", RepoPath: "/tmp/looper",
		WorktreePath: "/tmp/wt/feature-fixer", Branch: "feature/fixer", Status: "active",
		CreatedAt: now, UpdatedAt: now, Generation: 1, CheckoutKey: "feature-fixer",
	}); err != nil {
		t.Fatalf("Upsert(second live checkout of the same branch) error = %v", err)
	}
	attached, err := repos.Worktrees.GetLiveByCheckout(ctx, "project_0021", "feature-fixer")
	if err != nil || attached == nil || attached.ID != "wt_attached" {
		t.Fatalf("GetLiveByCheckout(attached) = %#v, %v, want the attached checkout", attached, err)
	}
	detached, err := repos.Worktrees.GetLiveByCheckout(ctx, "project_0021", "looper-fix-p-pr-42-detached")
	if err != nil || detached == nil || detached.ID != "wt_gen_1" {
		t.Fatalf("GetLiveByCheckout(detached) = %#v, %v, want the detached checkout", detached, err)
	}

	// Retiring the first makes room for the next generation of that checkout.
	if err := repos.Worktrees.Retire(ctx, "wt_gen_1", now); err != nil {
		t.Fatalf("Retire() error = %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, status, created_at, updated_at, generation, checkout_key)
		VALUES ('wt_gen_2_live', 'project_0021', '/tmp/looper', '/tmp/wt/looper-fix-p-pr-42-detached+g2', 'feature/fixer', 'active', ?, ?, 2, 'looper-fix-p-pr-42-detached')
	`, now, now); err != nil {
		t.Fatalf("insert of generation 2 after retirement error = %v", err)
	}

	live, err := repos.Worktrees.GetLiveByCheckout(ctx, "project_0021", "looper-fix-p-pr-42-detached")
	if err != nil {
		t.Fatalf("GetLiveByCheckout() error = %v", err)
	}
	if live == nil || live.ID != "wt_gen_2_live" {
		t.Fatalf("GetLiveByCheckout() = %#v, want the live generation only", live)
	}
	next, err := repos.Worktrees.NextGenerationForCheckout(ctx, "project_0021", "looper-fix-p-pr-42-detached")
	if err != nil {
		t.Fatalf("NextGenerationForCheckout() error = %v", err)
	}
	if next != 3 {
		t.Fatalf("NextGenerationForCheckout() = %d, want 3", next)
	}
	// The attached checkout's generations are counted separately.
	nextAttached, err := repos.Worktrees.NextGenerationForCheckout(ctx, "project_0021", "feature-fixer")
	if err != nil {
		t.Fatalf("NextGenerationForCheckout(attached) error = %v", err)
	}
	if nextAttached != 2 {
		t.Fatalf("NextGenerationForCheckout(attached) = %d, want 2", nextAttached)
	}
	retired, err := repos.Worktrees.ListRetired(ctx)
	if err != nil {
		t.Fatalf("ListRetired() error = %v", err)
	}
	if len(retired) != 1 || retired[0].ID != "wt_gen_1" {
		t.Fatalf("ListRetired() = %#v, want the retired generation", retired)
	}
}

// The generation suffix must not be forgeable by a branch name, or generation 2
// of `feature` and generation 1 of a branch that sanitizes to `feature+g2`
// would name the same directory.
func TestCheckoutKeyFromPathSplitsOnlyTheGenerationSuffix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		want string
	}{
		{"/wt/feature-fixer", "feature-fixer"},
		{"/wt/feature-fixer+g2", "feature-fixer"},
		{"/wt/feature-fixer+g17", "feature-fixer"},
		// `-g2` is a legal sanitized branch name, so it is never a suffix.
		{"/wt/feature-g2", "feature-g2"},
		{"/wt/looper-fix-p-pr-42-detached+g3", "looper-fix-p-pr-42-detached"},
		// Not a number after the separator: not a generation.
		{"/wt/feature+gamma", "feature+gamma"},
		{"", ""},
	}
	for _, testCase := range cases {
		if got := CheckoutKeyFromPath(testCase.path); got != testCase.want {
			t.Fatalf("CheckoutKeyFromPath(%q) = %q, want %q", testCase.path, got, testCase.want)
		}
	}
}
