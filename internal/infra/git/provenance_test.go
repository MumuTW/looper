package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupWorktreeRefusesWithoutProvenanceRepositoryBeforeStartingGit(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")

	startedPath := filepath.Join(t.TempDir(), "git-started")
	t.Setenv("FAKE_GIT_STARTED", startedPath)
	fakeGit := writeFakeGit(t, "#!/bin/sh\ntouch \"$FAKE_GIT_STARTED\"\n")
	gateway := New(Options{GitPath: fakeGit, Now: fixture.now})
	worktreePath := filepath.Join(fixture.worktreeRoot, "external")
	mustMkdirAll(t, fixture.worktreeRoot)

	err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktreePath, Branch: "feature/fixer",
	})
	if err == nil || !strings.Contains(err.Error(), "provenance repository is unavailable") {
		t.Fatalf("CleanupWorktree() error = %v, want unavailable provenance error", err)
	}
	if _, err := os.Stat(startedPath); !os.IsNotExist(err) {
		t.Fatalf("git was started before provenance rejection: stat error = %v", err)
	}
}

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
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
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
		ProjectID:         fixture.projectID,
		RepoPath:          fixture.repoPath,
		WorktreeRoot:      fixture.worktreeRoot,
		WorktreePath:      worktree.WorktreePath,
		Branch:            "feature/fixer",
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
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	// Try to cleanup with a different path (but under the worktree root) and same branch
	fakePath := filepath.Join(fixture.worktreeRoot, "other-path")
	err = gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID:         fixture.projectID,
		RepoPath:          fixture.repoPath,
		WorktreeRoot:      fixture.worktreeRoot,
		WorktreePath:      fakePath,
		Branch:            "feature/fixer",
		ProtectedBranches: []string{"main"},
	})

	if err == nil {
		t.Fatal("expected error when cleaning up worktree with mismatched path, got nil")
	}
	if !strings.Contains(err.Error(), "no looper worktree record found") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// But cleaning up with the correct path should work
	err = gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID:         fixture.projectID,
		RepoPath:          fixture.repoPath,
		WorktreeRoot:      fixture.worktreeRoot,
		WorktreePath:      worktree.WorktreePath,
		Branch:            "feature/fixer",
		ProtectedBranches: []string{"main"},
	})
	if err != nil {
		t.Fatalf("cleanup with correct path should succeed: %v", err)
	}
}

func TestCleanupWorktreeRefusesCleanedRecordAfterExternalReplacement(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath, Branch: "feature/fixer",
	}); err != nil {
		t.Fatalf("initial CleanupWorktree() error = %v", err)
	}

	// A human can later reuse the same branch and directory. The cleaned record
	// is historical evidence, not authority to remove that new checkout.
	runGit(t, fixture.repoPath, "worktree", "add", "--force", worktree.WorktreePath, "feature/fixer")
	err = gateway.CleanupWorktree(ctx, CleanupWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath, Branch: "feature/fixer",
	})
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("CleanupWorktree() error = %v, want inactive provenance rejection", err)
	}
	if _, err := os.Stat(worktree.WorktreePath); err != nil {
		t.Fatalf("external replacement was removed: %v", err)
	}
}
