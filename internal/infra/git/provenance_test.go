package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupWorktreeRefusesWithoutProvenanceRecord(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)

	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	// Create a worktree (this creates a WorktreeRecord)
	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:      "feature/fixer",
		BaseBranch:  "main",
		PRNumber:    42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	// Manually delete the WorktreeRecord to simulate an external worktree
	_, err = fixture.coordinator.DB().ExecContext(ctx, "DELETE FROM worktrees WHERE id = ?", worktree.ID)
	if err != nil {
		t.Fatalf("DELETE worktree record: %v", err)
	}

	// Now try to cleanup — should fail because no provenance record
	err = gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath,
		Branch:       "feature/fixer",
		ProtectedBranches: []string{"main"},
	})

	if err == nil {
		t.Fatal("expected error when cleaning up worktree without provenance record, got nil")
	}
	if !strings.Contains(err.Error(), "no looper worktree record found") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCleanupWorktreeRefusesWithMismatchedPath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)

	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	// Create a worktree
	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:      "feature/fixer",
		BaseBranch:  "main",
		PRNumber:    42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	// Try to cleanup with a different path (but under the worktree root) and same branch
	fakePath := filepath.Join(fixture.worktreeRoot, "other-path")
	err = gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: fakePath,
		Branch:       "feature/fixer",
		ProtectedBranches: []string{"main"},
	})

	if err == nil {
		t.Fatal("expected error when cleaning up worktree with mismatched path, got nil")
	}
	if !strings.Contains(err.Error(), "different path") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// But cleaning up with the correct path should work
	err = gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath,
		Branch:       "feature/fixer",
		ProtectedBranches: []string{"main"},
	})
	if err != nil {
		t.Fatalf("cleanup with correct path should succeed: %v", err)
	}
}
