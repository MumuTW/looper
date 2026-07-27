package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func TestGatewayRejectsRepoPathAsMutationWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "prepare", run: func() error {
			_, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "main"})
			return err
		}},
		{name: "commit", run: func() error {
			_, err := gateway.Commit(ctx, CommitInput{RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Message: "test"})
			return err
		}},
		{name: "push", run: func() error {
			return gateway.Push(ctx, PushInput{RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "feature/test"})
		}},
		{name: "cleanup", run: func() error {
			return gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "feature/test"})
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), "must not equal project repo path") {
				t.Fatalf("error = %v, want repo-path safety failure", err)
			}
		})
	}
}

func TestGatewayRejectsMutationWorktreeOutsideRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()
	outsidePath := filepath.Join(fixture.rootDir, "outside-worktree")
	mustMkdirAll(t, outsidePath)

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "prepare", run: func() error {
			_, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Branch: "feature/test"})
			return err
		}},
		{name: "commit", run: func() error {
			_, err := gateway.Commit(ctx, CommitInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Message: "test"})
			return err
		}},
		{name: "push", run: func() error {
			return gateway.Push(ctx, PushInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Branch: "feature/test"})
		}},
		{name: "cleanup", run: func() error {
			return gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: outsidePath, Branch: "feature/test"})
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), "must be under worktree root") {
				t.Fatalf("error = %v, want worktree-root safety failure", err)
			}
		})
	}
}

func TestGatewayCreatesRestoresAndCleansWorktreesWithBranchProtection(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)

	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

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

	restored, err := gateway.RestoreWorktree(ctx, RestoreWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, Branch: "feature/fixer"})
	if err != nil {
		t.Fatalf("RestoreWorktree() error = %v", err)
	}
	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"})
	if err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "hello updated\n")
	inspectBeforeCommit, err := gateway.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.WorktreePath, BaseRef: prepared.HeadSHA})
	if err != nil {
		t.Fatalf("InspectHead(before) error = %v", err)
	}
	globalEmailBefore := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "config", "--global", "--get", "user.email"))
	commitResult, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "fixer: address PR #42 follow-up items"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	inspectAfterCommit, err := gateway.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.WorktreePath, BaseRef: prepared.HeadSHA})
	if err != nil {
		t.Fatalf("InspectHead(after) error = %v", err)
	}

	if got := readFile(t, filepath.Join(worktree.WorktreePath, "README.md")); got != "hello updated\n" {
		t.Fatalf("README.md = %q, want updated contents", got)
	}
	if restored == nil || restored.Branch != "feature/fixer" {
		t.Fatalf("RestoreWorktree() = %#v, want branch feature/fixer", restored)
	}
	if !prepared.Clean {
		t.Fatalf("PrepareWorktree().Clean = false, want true")
	}
	if !inspectBeforeCommit.HasUncommittedChanges {
		t.Fatalf("InspectHead(before).HasUncommittedChanges = false, want true")
	}
	if commitResult.CommitSHA == "" {
		t.Fatal("Commit().CommitSHA = empty, want value")
	}
	if inspectAfterCommit.HasUncommittedChanges {
		t.Fatalf("InspectHead(after).HasUncommittedChanges = true, want false")
	}
	if len(inspectAfterCommit.NewCommitSHAs) != 1 {
		t.Fatalf("InspectHead(after).NewCommitSHAs = %#v, want 1 entry", inspectAfterCommit.NewCommitSHAs)
	}
	commitAuthor := stringsTrimSpace(runGit(t, worktree.WorktreePath, "log", "-1", "--format=%an <%ae>"))
	if commitAuthor != "Looper Test <test@example.com>" {
		t.Fatalf("commit author = %q, want Looper Test <test@example.com>", commitAuthor)
	}
	globalEmailAfter := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "config", "--global", "--get", "user.email"))
	if globalEmailAfter != globalEmailBefore {
		t.Fatalf("global git email changed: before=%q after=%q", globalEmailBefore, globalEmailAfter)
	}

	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	stored, err := fixture.repos.Worktrees.GetByBranch(ctx, fixture.projectID, "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if stored == nil || stored.Status != "cleaned" {
		t.Fatalf("stored worktree after cleanup = %#v, want cleaned", stored)
	}

	err = gateway.AssertWritableBranch("main", []string{"main"})
	var protectedErr *ProtectedBranchError
	if err == nil || !errors.As(err, &protectedErr) {
		t.Fatalf("AssertWritableBranch() error = %v, want *ProtectedBranchError", err)
	}
}

func TestGatewayWorktreeCleanIgnoresIgnoredFiles(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/ignore",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	writeFile(t, filepath.Join(worktree.WorktreePath, ".gitignore"), "*.log\n")
	runGit(t, worktree.WorktreePath, "add", ".gitignore")
	runGit(t, worktree.WorktreePath, "commit", "-m", "ignore logs")
	runGit(t, worktree.WorktreePath, "config", "status.showIgnored", "matching")

	writeFile(t, filepath.Join(worktree.WorktreePath, "debug.log"), "ignored\n")
	clean, err := gateway.WorktreeClean(ctx, worktree.WorktreePath)
	if err != nil {
		t.Fatalf("WorktreeClean(ignored file) error = %v", err)
	}
	if !clean {
		t.Fatal("WorktreeClean(ignored file) = false, want true")
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "note.txt"), "untracked\n")
	clean, err = gateway.WorktreeClean(ctx, worktree.WorktreePath)
	if err != nil {
		t.Fatalf("WorktreeClean(untracked file) error = %v", err)
	}
	if clean {
		t.Fatal("WorktreeClean(untracked file) = true, want false")
	}
}

func TestGatewayWorktreeExcludesBuildArtifactsFromCommits(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/pnpm",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	// The worktree's info/exclude must carry looper's artifact patterns.
	excludeRelPath := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "--git-path", "info/exclude"))
	excludePath := excludeRelPath
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktree.WorktreePath, excludeRelPath)
	}
	excludeContent := readFile(t, excludePath)
	for _, pattern := range []string{".pnpm-store/", "node_modules/", ".turbo/", "dist/", ".next/", ".cache/", "*.log", "/.looper-review-*.json"} {
		if !strings.Contains(excludeContent, "\n"+pattern) && !strings.HasPrefix(excludeContent, pattern) {
			t.Fatalf("info/exclude missing pattern %q; content = %q", pattern, excludeContent)
		}
	}

	// The real-world failure: `git add -A` must NOT stage a 100MB+ .pnpm-store,
	// while ordinary source is still staged.
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, ".pnpm-store", "v3"))
	writeFile(t, filepath.Join(worktree.WorktreePath, ".pnpm-store", "v3", "huge.bin"), "artifact\n")
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, "node_modules"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "node_modules", "dep.js"), "module\n")
	writeFile(t, filepath.Join(worktree.WorktreePath, "app.ts"), "export const x = 1\n")

	runGit(t, worktree.WorktreePath, "add", "-A")
	staged := runGit(t, worktree.WorktreePath, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "app.ts") {
		t.Fatalf("git add -A did not stage source app.ts; staged = %q", staged)
	}
	if strings.Contains(staged, ".pnpm-store") || strings.Contains(staged, "node_modules") {
		t.Fatalf("git add -A staged an excluded build artifact; staged = %q", staged)
	}

	// Idempotent: re-creating (which restores the existing worktree) must not
	// duplicate the exclude patterns.
	if _, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/pnpm",
		BaseBranch:   "main",
	}); err != nil {
		t.Fatalf("CreateWorktree() second call error = %v", err)
	}
	if got := strings.Count(readFile(t, excludePath), ".pnpm-store/"); got != 1 {
		t.Fatalf(".pnpm-store/ appears %d times in info/exclude, want 1 (idempotent)", got)
	}
}

func TestGatewayKeepsPrimaryCheckoutCleanForDetachedFixerWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/fixer",
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch name = %q, want empty", got)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "hello updated\n")
	baseHeadSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "refs/remotes/origin/feature/fixer"))
	repoHeadBefore := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "HEAD"))
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "fixer: address PR #42 follow-up items"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := gateway.Push(ctx, PushInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer", ExpectedRemoteHeadSHA: baseHeadSHA}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "HEAD")); got != repoHeadBefore {
		t.Fatalf("repo HEAD = %q, want %q", got, repoHeadBefore)
	}
	if got := stringsTrimSpace(runGit(t, fixture.repoPath, "status", "--porcelain")); got != "" {
		t.Fatalf("repo status = %q, want empty", got)
	}
	if got := stringsTrimSpace(runGit(t, fixture.repoPath, "diff", "--cached", "--name-only")); got != "" {
		t.Fatalf("repo cached diff = %q, want empty", got)
	}
}

func TestGatewayDetachedWorktreeFallsBackToRemoteOnlyBaseBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	fixture.createUnfetchedRemoteBranch(t, "release/base")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "reviewer/pr-42",
		BaseBranch:   "release/base",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch name = %q, want empty", got)
	}

	remoteBaseSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/release/base"))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if worktreeHeadSHA != remoteBaseSHA {
		t.Fatalf("detached HEAD = %q, want %q", worktreeHeadSHA, remoteBaseSHA)
	}
}

func TestGatewayAttachedWorktreeFallsBackToRemoteOnlyBaseBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	fixture.createUnfetchedRemoteBranch(t, "release/base")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "worker/release-base-sync",
		BaseBranch:   "release/base",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "worker/release-base-sync" {
		t.Fatalf("attached branch name = %q, want worker/release-base-sync", got)
	}

	remoteBaseSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/release/base"))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if worktreeHeadSHA != remoteBaseSHA {
		t.Fatalf("attached HEAD = %q, want %q", worktreeHeadSHA, remoteBaseSHA)
	}
}

func TestGatewayAttachedWorktreeFailsWhenWorkerBranchLookupErrors(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	branch := "worker/offline-main-fallback"
	fixture.createUnfetchedRemoteBranch(t, branch)
	runGit(t, fixture.repoPath, "remote", "set-url", "origin", filepath.Join(fixture.rootDir, "missing-remote.git"))
	gateway := fixture.gateway()

	_, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       branch,
		BaseBranch:   "main",
	})
	if err == nil {
		t.Fatal("CreateWorktree() error = nil, want branch lookup failure")
	}
	if !strings.Contains(err.Error(), "git ls-remote --heads origin "+branch) {
		t.Fatalf("CreateWorktree() error = %v, want worker branch lookup failure", err)
	}
}

func TestGatewayPushesHeadToRequestedRemoteBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "looper/05e7c1d53bba907c",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "hello updated\n")
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "worker: update reused PR branch"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := gateway.Push(ctx, PushInput{WorktreePath: worktree.WorktreePath, Branch: "looper/worker/05e7c1d53bba907c"}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	remoteHeadSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/looper/worker/05e7c1d53bba907c"))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if remoteHeadSHA != worktreeHeadSHA {
		t.Fatalf("remote head = %q, want %q", remoteHeadSHA, worktreeHeadSHA)
	}
}

func TestGatewayCreatesAttachedWorktreeFromRemoteOnlyBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	fixture.createUnfetchedRemoteBranch(t, "looper/715-error-exporting-apresentations-f1db7b9a10e512af")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "looper/715-error-exporting-apresentations-f1db7b9a10e512af",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	remoteHeadSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/looper/715-error-exporting-apresentations-f1db7b9a10e512af"))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if worktreeHeadSHA != remoteHeadSHA {
		t.Fatalf("worktree HEAD = %q, want remote branch head %q", worktreeHeadSHA, remoteHeadSHA)
	}
	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "branch", "--show-current")); got != "looper/715-error-exporting-apresentations-f1db7b9a10e512af" {
		t.Fatalf("branch = %q, want remote-only branch checkout", got)
	}
	if got := stringsTrimSpace(runGitMaybe(t, fixture.repoPath, "show-ref", "--verify", "refs/remotes/origin/looper/715-error-exporting-apresentations-f1db7b9a10e512af")); got == "" {
		t.Fatal("origin remote-tracking branch missing after CreateWorktree()")
	}
}

func TestGatewayCreatesAttachedWorktreeAfterRemoteBranchForcePush(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	branch := "looper/force-pushed-worker-branch"
	fixture.createUnfetchedRemoteBranch(t, branch)
	runGit(t, fixture.repoPath, "fetch", "origin", fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", branch, branch))
	fixture.forcePushRemoteBranch(t, branch, sanitizeBranchName(branch)+"-force.txt", "force-pushed remote change\n")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       branch,
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	remoteHeadSHA := stringsTrimSpace(runGit(t, fixture.remotePath, "rev-parse", "refs/heads/"+branch))
	worktreeHeadSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	if worktreeHeadSHA != remoteHeadSHA {
		t.Fatalf("worktree HEAD = %q, want force-pushed remote branch head %q", worktreeHeadSHA, remoteHeadSHA)
	}
	trackingSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "refs/remotes/origin/"+branch))
	if trackingSHA != remoteHeadSHA {
		t.Fatalf("remote tracking HEAD = %q, want %q", trackingSHA, remoteHeadSHA)
	}
}

func TestGatewayCreatesSeparateDetachedWorktreeForAttachedBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	attached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(attached) error = %v", err)
	}
	if got := stringsTrimSpace(runGit(t, attached.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("attached branch = %q, want feature/fixer", got)
	}

	detached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	if detached.WorktreePath == attached.WorktreePath {
		t.Fatalf("detached worktree path = %q, want separate path from attached worktree", detached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, detached.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch = %q, want empty", got)
	}
	if _, err := os.Stat(attached.WorktreePath); err != nil {
		t.Fatalf("attached worktree missing after detached create: %v", err)
	}
}

func TestGatewayCreatesSeparateAttachedWorktreeForDetachedBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	detached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	if got := stringsTrimSpace(runGit(t, detached.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch = %q, want empty", got)
	}

	attached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(attached) error = %v", err)
	}
	if attached.WorktreePath == detached.WorktreePath {
		t.Fatalf("attached worktree path = %q, want separate path from detached worktree", attached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, attached.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("attached branch = %q, want feature/fixer", got)
	}
	if _, err := os.Stat(detached.WorktreePath); err != nil {
		t.Fatalf("detached worktree missing after attached create: %v", err)
	}
}

func TestGatewayRecreatesBranchNamedWorktreeAsDetachedAtSamePath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	attached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("CreateWorktree(attached) error = %v", err)
	}

	detached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	if detached.WorktreePath != attached.WorktreePath {
		t.Fatalf("detached path = %q, want %q", detached.WorktreePath, attached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, detached.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("detached branch = %q, want empty", got)
	}
}

func TestGatewayRecreatesStoredAttachedWorktreeWhenOnWrongBranch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureAndOtherRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	runGit(t, worktree.WorktreePath, "checkout", "feature/other")

	restored, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(recreated) error = %v", err)
	}
	if restored.WorktreePath != worktree.WorktreePath {
		t.Fatalf("restored path = %q, want %q", restored.WorktreePath, worktree.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, restored.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("restored branch = %q, want feature/fixer", got)
	}
}

func TestGatewayRecreatesStoredAttachedWorktreeWhenDetached(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	runGit(t, worktree.WorktreePath, "checkout", "HEAD")

	restored, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree(recreated) error = %v", err)
	}
	if restored.WorktreePath != worktree.WorktreePath {
		t.Fatalf("restored path = %q, want %q", restored.WorktreePath, worktree.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, restored.WorktreePath, "branch", "--show-current")); got != "feature/fixer" {
		t.Fatalf("restored branch = %q, want feature/fixer", got)
	}
}

func TestGatewayRestoresDetachedWorktreeFromExpectedPathWithoutStoreRow(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)

	detached, err := fixture.gateway().CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(detached) error = %v", err)
	}
	statelessGateway := New(Options{GitPath: "git", Now: fixture.now})

	restored, err := statelessGateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree(restored) error = %v", err)
	}
	if normalizeComparablePath(restored.WorktreePath) != normalizeComparablePath(detached.WorktreePath) {
		t.Fatalf("restored path = %q, want %q", restored.WorktreePath, detached.WorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, restored.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("restored detached branch = %q, want empty", got)
	}
}

func TestGatewayRecreatesWorktreeWhenStoredRowPointsAtDeletedPath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	missingWorktreePath := filepath.Join(fixture.worktreeRoot, "looper-fix-project_1-pr-42-detached")
	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "missing-record", ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: missingWorktreePath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	recreated, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if normalizeComparablePath(recreated.WorktreePath) != normalizeComparablePath(missingWorktreePath) {
		t.Fatalf("recreated path = %q, want %q", recreated.WorktreePath, missingWorktreePath)
	}
	if got := stringsTrimSpace(runGit(t, recreated.WorktreePath, "branch", "--show-current")); got != "" {
		t.Fatalf("recreated detached branch = %q, want empty", got)
	}
}

func TestGatewayIgnoresStoredWorktreesFromDifferentRepoPath(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	otherRepoPath := filepath.Join(fixture.rootDir, "other-repo")
	strayWorktreePath := filepath.Join(fixture.rootDir, "stray-worktree")
	mustMkdirAll(t, otherRepoPath)
	mustMkdirAll(t, strayWorktreePath)
	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "wrong-repo-record", ProjectID: fixture.projectID, RepoPath: otherRepoPath, WorktreePath: strayWorktreePath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if worktree.WorktreePath == strayWorktreePath {
		t.Fatalf("CreateWorktree().WorktreePath = stray path %q, want new worktree", strayWorktreePath)
	}
	stored, err := fixture.repos.Worktrees.GetByBranch(ctx, fixture.projectID, "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if stored == nil || normalizeComparablePath(stored.RepoPath) != normalizeComparablePath(fixture.repoPath) {
		t.Fatalf("stored repo path = %#v, want %q", stored, fixture.repoPath)
	}
}

func TestNormalizeComparablePathTrimsOnlyPrivateSlashPrefix(t *testing.T) {
	t.Parallel()

	if got := normalizeComparablePath("/private/var/tmp/repo"); got != "/var/tmp/repo" {
		t.Fatalf("normalizeComparablePath(/private/var/tmp/repo) = %q, want %q", got, "/var/tmp/repo")
	}
	if got := normalizeComparablePath("/private-repo/worktree"); got != "/private-repo/worktree" {
		t.Fatalf("normalizeComparablePath(/private-repo/worktree) = %q, want %q", got, "/private-repo/worktree")
	}
}

func TestGatewayDoesNotTreatPrimaryCheckoutAsRestorableWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepoWithoutReturningToMain(t)
	gateway := fixture.gateway()

	restored, err := gateway.RestoreWorktree(ctx, RestoreWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, Branch: "feature/fixer", WorktreeRoot: fixture.worktreeRoot})
	if err != nil {
		t.Fatalf("RestoreWorktree() error = %v", err)
	}
	if restored != nil {
		t.Fatalf("RestoreWorktree() = %#v, want nil", restored)
	}
}

func TestGatewayReusesExistingBranchWorktreeRecordWhenRecreatingWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "existing-record", ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: fixture.repoPath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if worktree.ID != "existing-record" {
		t.Fatalf("CreateWorktree().ID = %q, want existing-record", worktree.ID)
	}
	if worktree.WorktreePath == fixture.repoPath {
		t.Fatalf("CreateWorktree().WorktreePath = repo path %q, want separate worktree", fixture.repoPath)
	}
	stored, err := fixture.repos.Worktrees.GetByBranch(ctx, fixture.projectID, "feature/fixer")
	if err != nil {
		t.Fatalf("GetByBranch() error = %v", err)
	}
	if stored == nil || stored.ID != "existing-record" {
		t.Fatalf("stored worktree = %#v, want ID existing-record", stored)
	}
}

func TestGatewayRejectsProtectedBranchWorktreeCreation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)

	_, err := fixture.gateway().CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:         fixture.projectID,
		RepoPath:          fixture.repoPath,
		WorktreeRoot:      fixture.worktreeRoot,
		Branch:            "main",
		BaseBranch:        "main",
		ProtectedBranches: []string{"main"},
	})
	var protectedErr *ProtectedBranchError
	if err == nil || !errors.As(err, &protectedErr) {
		t.Fatalf("CreateWorktree() error = %v, want *ProtectedBranchError", err)
	}
}

func TestGatewayPrepareWorktreeDetectsRemoteHeadChanges(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

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
	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer"})
	if err != nil {
		t.Fatalf("PrepareWorktree(initial) error = %v", err)
	}

	fixture.advanceRemoteBranch(t, "feature/fixer", "remote-update.txt", "changed remotely\n")

	_, err = gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer", ExpectedHeadSHA: prepared.HeadSHA})
	var remoteHeadErr *RemoteHeadChangedError
	if err == nil || !errors.As(err, &remoteHeadErr) {
		t.Fatalf("PrepareWorktree() error = %v, want *RemoteHeadChangedError", err)
	}
	if remoteHeadErr.ExpectedHeadSHA != prepared.HeadSHA {
		t.Fatalf("RemoteHeadChangedError.ExpectedHeadSHA = %q, want %q", remoteHeadErr.ExpectedHeadSHA, prepared.HeadSHA)
	}
}

func TestIsReservedReviewerScratchPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{".looper-review-1048.json", true},
		{".looper-review-.json", true}, // Git * matches empty; classifier must too
		{".looper-review-\u00e9.json", true},
		{`".looper-review-1.json"`, true},
		{"nested/.looper-review-1.json", false},
		{`.looper-review-1\json`, false},
		{".looper-review.json", false},
		{" .looper-review-x.json", false}, // leading whitespace is a different pathname
		{".looper-review-x.json ", false},
		{"app.go", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isReservedReviewerScratchPath(tc.path); got != tc.want {
			t.Fatalf("isReservedReviewerScratchPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Real-Git contract matrix for reserved /.looper-review-*.json namespace.
// Shared fixtures keep edge coverage without per-case setup-heavy tests.
func TestGatewayReservedReviewerScratchContract(t *testing.T) {
	type prepWant struct {
		clean bool
	}
	type commitWant struct {
		include []string
		exclude []string
	}
	type caseSpec struct {
		name            string
		branch          string
		gitignoreNegate bool
		seedFiles       map[string]string // committed on feature before create
		setup           func(t *testing.T, wt string)
		prepare         *prepWant
		checkExclude    bool
		beforeCommit    func(t *testing.T, wt string)
		commit          *commitWant
		keepOnDisk      []string
	}

	cases := []caseSpec{
		{
			name:   "prepare_ignores_scratch_reconciles_exclude",
			branch: "feature/review-scratch",
			setup: func(t *testing.T, wt string) {
				excludePath := worktreeExcludePath(t, wt)
				writeFile(t, excludePath, "# stale exclude without reviewer scratch\n")
				writeFile(t, filepath.Join(wt, ".looper-review-1048.json"), `{"body":"scratch"}`+"\n")
			},
			prepare:      &prepWant{clean: true},
			checkExclude: true,
			keepOnDisk:   []string{".looper-review-1048.json"},
		},
		{
			name:   "prepare_real_dirt_still_dirty",
			branch: "feature/review-real-dirt",
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-1048.json"), `{"body":"scratch"}`+"\n")
				writeFile(t, filepath.Join(wt, "partial-fix.go"), "package main\n")
			},
			prepare: &prepWant{clean: false},
		},
		{
			name:   "prepare_nested_lookalike_dirty",
			branch: "feature/review-nested",
			setup: func(t *testing.T, wt string) {
				mustMkdirAll(t, filepath.Join(wt, "nested"))
				writeFile(t, filepath.Join(wt, "nested", ".looper-review-1.json"), "{}\n")
			},
			prepare: &prepWant{clean: false},
		},
		{
			// Leading/trailing whitespace in -z pathnames must not collapse into
			// the reserved basename; prepare stays dirty and Commit must not rewrite
			// the path when unstaging (reset would miss the real name).
			name:   "whitespace_padded_reserved_lookalike_dirty",
			branch: "feature/review-whitespace-path",
			setup: func(t *testing.T, wt string) {
				name := " .looper-review-x.json"
				writeFile(t, filepath.Join(wt, name), `{"body":"spaced"}`+"\n")
				statusZ := runGit(t, wt, "status", "--porcelain", "-z", "--untracked-files=all")
				if !strings.Contains(statusZ, name) {
					t.Fatalf("expected -z porcelain to preserve leading whitespace pathname; status = %q", statusZ)
				}
			},
			prepare: &prepWant{clean: false},
			commit: &commitWant{
				include: []string{"app.go", " .looper-review-x.json"},
				exclude: []string{},
			},
			keepOnDisk: []string{" .looper-review-x.json"},
		},
		{
			name:            "negation_empty_suffix_prepare_and_commit",
			branch:          "feature/review-negation",
			gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				// Empty suffix matches Git /.looper-review-*.json; must not be dirt/commit.
				writeFile(t, filepath.Join(wt, ".looper-review-.json"), `{"body":"empty-suffix"}`+"\n")
				writeFile(t, filepath.Join(wt, ".looper-review-1049.json"), `{"body":"scratch"}`+"\n")
				if status := runGit(t, wt, "status", "--short", "--", ".looper-review-1049.json"); !strings.Contains(status, ".looper-review-1049.json") {
					t.Fatalf("expected git status to show untracked scratch under negation; status = %q", status)
				}
			},
			prepare: &prepWant{clean: true},
			commit: &commitWant{
				include: []string{"app.go", "nested/.looper-review-1.json"},
				exclude: []string{".looper-review-1049.json", ".looper-review-.json"},
			},
			keepOnDisk: []string{".looper-review-1049.json", ".looper-review-.json"},
		},
		{
			name:            "commit_unstages_prestaged",
			branch:          "feature/review-prestaged",
			gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-1050.json"), `{"body":"scratch"}`+"\n")
				runGit(t, wt, "add", "-A", "--", ".looper-review-1050.json")
				if staged := runGit(t, wt, "diff", "--cached", "--name-only"); !strings.Contains(staged, ".looper-review-1050.json") {
					t.Fatalf("expected scratch pre-staged before Commit; staged = %q", staged)
				}
			},
			commit:     &commitWant{include: []string{"app.go"}, exclude: []string{".looper-review-1050.json"}},
			keepOnDisk: []string{".looper-review-1050.json"},
		},
		{
			name:            "commit_unstages_despite_rename_detection",
			branch:          "feature/review-rename",
			gitignoreNegate: true,
			seedFiles:       map[string]string{"old.json": `{"body":"scratch"}` + "\n"},
			setup: func(t *testing.T, wt string) {
				payload := `{"body":"scratch"}` + "\n"
				if err := os.Remove(filepath.Join(wt, "old.json")); err != nil {
					t.Fatalf("Remove old.json: %v", err)
				}
				writeFile(t, filepath.Join(wt, ".looper-review-1.json"), payload)
				writeFile(t, filepath.Join(wt, "app.go"), "package main\n")
				runGit(t, wt, "add", "-A")
				if renameStatus := runGit(t, wt, "diff", "--cached", "--name-status"); !strings.Contains(renameStatus, "R100") || !strings.Contains(renameStatus, ".looper-review-1.json") {
					t.Fatalf("expected staged R100 rename onto reserved scratch; status = %q", renameStatus)
				}
			},
			commit:     &commitWant{include: []string{"app.go", "old.json"}, exclude: []string{".looper-review-1.json"}},
			keepOnDisk: []string{".looper-review-1.json"},
		},
		{
			name:            "non_ascii_prepare_and_commit",
			branch:          "feature/review-nonascii",
			gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				name := ".looper-review-\u00e9.json"
				writeFile(t, filepath.Join(wt, name), `{"body":"scratch"}`+"\n")
				quoted := runGit(t, wt, "status", "--porcelain", "--untracked-files=all", "--", name)
				if !strings.Contains(quoted, `\`) && !strings.Contains(quoted, `"`) {
					t.Fatalf("expected default porcelain to quote non-ASCII scratch; status = %q", quoted)
				}
			},
			prepare: &prepWant{clean: true},
			// Pre-stage after prepare so Commit's unstage path classifies NUL paths.
			beforeCommit: func(t *testing.T, wt string) {
				runGit(t, wt, "add", "-A", "--", ".looper-review-\u00e9.json")
			},
			commit:     &commitWant{include: []string{"app.go"}, exclude: []string{".looper-review-\u00e9.json", ".looper-review-"}},
			keepOnDisk: []string{".looper-review-\u00e9.json"},
		},
		{
			name:      "commit_includes_tracked_reserved_name",
			branch:    "feature/review-tracked-name",
			seedFiles: map[string]string{".looper-review-fixture.json": `{"fixture":true}` + "\n"},
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-fixture.json"), `{"fixture":"updated"}`+"\n")
				writeFile(t, filepath.Join(wt, ".looper-review-1051.json"), `{"body":"scratch"}`+"\n")
			},
			commit: &commitWant{
				include: []string{"app.go", ".looper-review-fixture.json"},
				exclude: []string{".looper-review-1051.json"},
			},
			keepOnDisk: []string{".looper-review-1051.json"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFixture(t)
			fixture.createRemoteRepo(t, tc.branch)
			gateway := fixture.gateway()

			runGit(t, fixture.repoPath, "checkout", tc.branch)
			if tc.gitignoreNegate {
				writeFile(t, filepath.Join(fixture.repoPath, ".gitignore"), "!/.looper-review-*.json\n")
				runGit(t, fixture.repoPath, "add", ".gitignore")
			}
			for path, contents := range tc.seedFiles {
				writeFile(t, filepath.Join(fixture.repoPath, path), contents)
				runGit(t, fixture.repoPath, "add", path)
			}
			if tc.gitignoreNegate || len(tc.seedFiles) > 0 {
				runGit(t, fixture.repoPath, "commit", "-m", "seed reserved-scratch contract branch")
				runGit(t, fixture.repoPath, "push", "origin", tc.branch)
			}
			runGit(t, fixture.repoPath, "checkout", "main")

			worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
				ProjectID:    fixture.projectID,
				RepoPath:     fixture.repoPath,
				WorktreeRoot: fixture.worktreeRoot,
				Branch:       tc.branch,
				BaseBranch:   "main",
				PRNumber:     1048,
			})
			if err != nil {
				t.Fatalf("CreateWorktree() error = %v", err)
			}
			if tc.setup != nil {
				tc.setup(t, worktree.WorktreePath)
			}

			if tc.prepare != nil {
				// non_ascii pre-stages during setup; assert prepare before that
				// would be wrong for that case only when clean is expected with
				// untracked-only scratch. Re-run prepare after setup as written.
				prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: tc.branch})
				if err != nil {
					t.Fatalf("PrepareWorktree() error = %v", err)
				}
				if prepared.Clean != tc.prepare.clean {
					t.Fatalf("PrepareWorktree().Clean = %v, want %v", prepared.Clean, tc.prepare.clean)
				}
			}
			if tc.checkExclude {
				if content := readFile(t, worktreeExcludePath(t, worktree.WorktreePath)); !strings.Contains(content, "/.looper-review-*.json") {
					t.Fatalf("PrepareWorktree did not reconcile reserved exclude; content = %q", content)
				}
			}

			if tc.commit != nil {
				if _, err := os.Stat(filepath.Join(worktree.WorktreePath, "app.go")); os.IsNotExist(err) {
					writeFile(t, filepath.Join(worktree.WorktreePath, "app.go"), "package main\n")
				}
				// Nested lookalike only for the negation commit path.
				if tc.name == "negation_empty_suffix_prepare_and_commit" {
					mustMkdirAll(t, filepath.Join(worktree.WorktreePath, "nested"))
					writeFile(t, filepath.Join(worktree.WorktreePath, "nested", ".looper-review-1.json"), "{}\n")
				}
				if tc.beforeCommit != nil {
					tc.beforeCommit(t, worktree.WorktreePath)
				}
				if _, err := gateway.Commit(ctx, CommitInput{
					RepoPath:     fixture.repoPath,
					WorktreeRoot: fixture.worktreeRoot,
					WorktreePath: worktree.WorktreePath,
					Message:      "reserved scratch contract " + tc.name,
				}); err != nil {
					t.Fatalf("Commit() error = %v", err)
				}
				committed := runGit(t, worktree.WorktreePath, "show", "--pretty=format:", "--name-status", "HEAD")
				for _, want := range tc.commit.include {
					if !strings.Contains(committed, want) {
						t.Fatalf("Commit() missing %q; files = %q", want, committed)
					}
				}
				for _, ban := range tc.commit.exclude {
					if strings.Contains(committed, ban) {
						t.Fatalf("Commit() included reserved scratch %q; files = %q", ban, committed)
					}
				}
			}

			for _, rel := range tc.keepOnDisk {
				if _, err := os.Stat(filepath.Join(worktree.WorktreePath, rel)); err != nil {
					t.Fatalf("expected %q preserved on disk: %v", rel, err)
				}
			}
		})
	}
}

func worktreeExcludePath(t *testing.T, worktreePath string) string {
	t.Helper()
	rel := stringsTrimSpace(runGit(t, worktreePath, "rev-parse", "--git-path", "info/exclude"))
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(worktreePath, rel)
}

func TestGatewayPrepareWorktreeSupportsExplicitRef(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "reviewer/pr-42",
		BaseBranch:   "main",
		PRNumber:     42,
		CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: "reviewer/pr-42", Ref: "refs/heads/feature/fixer"})
	if err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	if !prepared.Clean {
		t.Fatal("PrepareWorktree().Clean = false, want true")
	}
	remoteHeadSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "refs/remotes/origin/feature/fixer"))
	if prepared.HeadSHA != remoteHeadSHA {
		t.Fatalf("PrepareWorktree().HeadSHA = %q, want %q", prepared.HeadSHA, remoteHeadSHA)
	}
}

func TestGatewayBranchExistsTreatsOnlyExitCode1AsMissing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	exists, err := gateway.branchExists(ctx, fixture.repoPath, "missing")
	if err != nil {
		t.Fatalf("branchExists(missing) error = %v", err)
	}
	if exists {
		t.Fatal("branchExists(missing) = true, want false")
	}

	nonRepoPath := filepath.Join(fixture.rootDir, "not-a-repo")
	mustMkdirAll(t, nonRepoPath)
	exists, err = gateway.branchExists(ctx, nonRepoPath, "missing")
	if err == nil {
		t.Fatal("branchExists(non-repo) error = nil, want error")
	}
	if exists {
		t.Fatal("branchExists(non-repo) = true, want false")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("branchExists(non-repo) error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("branchExists(non-repo) exit code = %d, want non-1", commandErr.Result.ExitCode)
	}
}

func TestGatewayResolveDetachedStartPointPropagatesFetchFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	runGit(t, fixture.repoPath, "remote", "set-url", "origin", filepath.Join(fixture.rootDir, "missing-remote.git"))
	gateway := fixture.gateway()

	_, err := gateway.resolveDetachedStartPoint(ctx, CreateWorktreeInput{
		RepoPath:     fixture.repoPath,
		Branch:       "feature/missing",
		BaseBranch:   "main",
		CheckoutMode: CheckoutModeDetached,
	})
	if err == nil {
		t.Fatal("resolveDetachedStartPoint() error = nil, want fetch failure")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("resolveDetachedStartPoint() error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("resolveDetachedStartPoint() exit code = %d, want non-1 fetch failure", commandErr.Result.ExitCode)
	}
}

func TestGatewayRetriesFetchWhenRemoteTrackingRefWasUpdatedConcurrently(t *testing.T) {
	ctx := context.Background()
	gitPath := writeFakeGit(t, `#!/bin/sh
count_file="$FAKE_GIT_COUNT"
count=0
if [ -f "$count_file" ]; then
	count=$(cat "$count_file")
fi
count=$((count + 1))
printf '%s' "$count" > "$count_file"
if [ "$count" -eq 1 ]; then
	cat >&2 <<'EOF'
From github.com:nexu-io/open-design
 * branch                main       -> FETCH_HEAD
error: cannot lock ref 'refs/remotes/origin/main': is at e64f1d8497409e76387cce3afcd5c51406a4174d but expected 6bf865a43beb8149c8f64a0af297c09c313f9a4a
 ! 6bf865a43..e64f1d849  main       -> origin/main  (unable to update local ref)
EOF
	exit 1
fi
exit 0
`)
	countPath := filepath.Join(t.TempDir(), "git-count")
	gateway := New(Options{GitPath: gitPath})

	err := gateway.runGit(ctx, t.TempDir(), map[string]string{"FAKE_GIT_COUNT": countPath}, "fetch", "origin", "main")
	if err != nil {
		t.Fatalf("runGit(fetch) error = %v, want retry success", err)
	}
	if got := stringsTrimSpace(readFile(t, countPath)); got != "2" {
		t.Fatalf("git attempts = %s, want 2", got)
	}
}

func TestGatewayRemoteBranchExistsTreatsOnlyExitCode1AsMissing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	exists, err := gateway.remoteBranchExists(ctx, fixture.repoPath, "origin", "feature/missing")
	if err != nil {
		t.Fatalf("remoteBranchExists(missing) error = %v", err)
	}
	if exists {
		t.Fatal("remoteBranchExists(missing) = true, want false")
	}

	nonRepoPath := filepath.Join(fixture.rootDir, "not-a-repo")
	mustMkdirAll(t, nonRepoPath)
	exists, err = gateway.remoteBranchExists(ctx, nonRepoPath, "origin", "feature/missing")
	if err == nil {
		t.Fatal("remoteBranchExists(non-repo) error = nil, want error")
	}
	if exists {
		t.Fatal("remoteBranchExists(non-repo) = true, want false")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("remoteBranchExists(non-repo) error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("remoteBranchExists(non-repo) exit code = %d, want non-1", commandErr.Result.ExitCode)
	}
}

func TestGatewayRemoteBranchExistsUsesFetchedTrackingRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/fixer"
	fixture.createRemoteRepo(t, branch)
	runGit(t, fixture.repoPath, "fetch", "origin", fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", branch, branch))
	runGit(t, fixture.repoPath, "remote", "set-url", "origin", filepath.Join(fixture.rootDir, "missing-remote.git"))
	gateway := fixture.gateway()

	exists, err := gateway.remoteBranchExists(ctx, fixture.repoPath, "origin", branch)
	if err != nil {
		t.Fatalf("remoteBranchExists(fetched tracking ref) error = %v", err)
	}
	if !exists {
		t.Fatal("remoteBranchExists(fetched tracking ref) = false, want true")
	}
}

func TestGatewayDiscardWorktreeChangesResetsTrackedAndUntracked(t *testing.T) {
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	ctx := context.Background()
	gateway := fixture.gateway()

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

	originalREADME := readFile(t, filepath.Join(worktree.WorktreePath, "README.md"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "dirty tracked\n")
	writeFile(t, filepath.Join(worktree.WorktreePath, "untracked.txt"), "dirty untracked\n")
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, "untracked-dir"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "untracked-dir", "nested.txt"), "nested\n")

	result, err := gateway.DiscardWorktreeChanges(ctx, DiscardWorktreeChangesInput{
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath,
	})
	if err != nil {
		t.Fatalf("DiscardWorktreeChanges() error = %v", err)
	}
	if result.NoOp || !result.WasDirty || result.WorktreePath != worktree.WorktreePath {
		t.Fatalf("DiscardWorktreeChanges() = %#v, want dirty discard of managed path", result)
	}
	if got := readFile(t, filepath.Join(worktree.WorktreePath, "README.md")); got != originalREADME {
		t.Fatalf("README.md after discard = %q, want %q", got, originalREADME)
	}
	if _, err := os.Stat(filepath.Join(worktree.WorktreePath, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked.txt still exists after discard: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree.WorktreePath, "untracked-dir")); !os.IsNotExist(err) {
		t.Fatalf("untracked-dir still exists after discard: %v", err)
	}
	if _, err := os.Stat(worktree.WorktreePath); err != nil {
		t.Fatalf("worktree directory missing after discard: %v", err)
	}
	clean, err := gateway.WorktreeClean(ctx, worktree.WorktreePath)
	if err != nil || !clean {
		t.Fatalf("WorktreeClean() = %v, %v, want clean", clean, err)
	}

	// Second call is a no-op when already clean.
	second, err := gateway.DiscardWorktreeChanges(ctx, DiscardWorktreeChangesInput{
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath,
	})
	if err != nil {
		t.Fatalf("DiscardWorktreeChanges(clean) error = %v", err)
	}
	if !second.NoOp || second.WasDirty {
		t.Fatalf("DiscardWorktreeChanges(clean) = %#v, want no-op", second)
	}
}

func TestGatewayDiscardWorktreeChangesRemovesNestedRepositories(t *testing.T) {
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	ctx := context.Background()
	gateway := fixture.gateway()

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

	// Tracked dirt so discard does real work (not the clean no-op path).
	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "dirty tracked\n")

	// Untracked nested Git checkout: single-force git clean -fd leaves these
	// behind; double-force -ffd is required to remove them.
	nestedPath := filepath.Join(worktree.WorktreePath, "vendor-checkout")
	mustMkdirAll(t, nestedPath)
	runGit(t, nestedPath, "init")
	writeFile(t, filepath.Join(nestedPath, "nested.txt"), "nested repo content\n")
	runGit(t, nestedPath, "add", "nested.txt")
	runGit(t, nestedPath, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", "nested")

	result, err := gateway.DiscardWorktreeChanges(ctx, DiscardWorktreeChangesInput{
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath,
	})
	if err != nil {
		t.Fatalf("DiscardWorktreeChanges() error = %v", err)
	}
	if result.NoOp || !result.WasDirty {
		t.Fatalf("DiscardWorktreeChanges() = %#v, want dirty discard including nested repo", result)
	}
	if _, err := os.Stat(nestedPath); !os.IsNotExist(err) {
		t.Fatalf("nested repository still exists after discard: %v", err)
	}
	clean, err := gateway.WorktreeClean(ctx, worktree.WorktreePath)
	if err != nil || !clean {
		t.Fatalf("WorktreeClean() = %v, %v, want clean after nested-repo discard", clean, err)
	}
}

func TestGatewayDiscardWorktreeChangesResetsDirtySubmodules(t *testing.T) {
	// Tracked submodule dirt is invisible to top-level git clean -ffd and to
	// reset --hard without --recurse-submodules. Discard must recurse so the
	// post-clean check can pass after top-level edits are discarded.
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	ctx := context.Background()
	gateway := fixture.gateway()

	// Build a bare submodule remote, commit it into the feature branch, then
	// create a managed worktree from that branch so discard runs under safety.
	// Pin bare HEAD to main: CI often has init.defaultBranch=master, so a push of
	// only main leaves HEAD on an unborn branch and `submodule add` fails with
	// "You are on a branch yet to be born".
	subRemote := filepath.Join(fixture.rootDir, "submodule.git")
	subWork := filepath.Join(fixture.rootDir, "submodule-work")
	mustMkdirAll(t, subRemote)
	runGit(t, fixture.rootDir, "init", "--bare", "-b", "main", subRemote)
	runGit(t, fixture.rootDir, "clone", subRemote, subWork)
	configureRepo(t, subWork)
	writeFile(t, filepath.Join(subWork, "module.txt"), "module-v1\n")
	runGit(t, subWork, "add", "module.txt")
	runGit(t, subWork, "commit", "-m", "submodule init")
	runGit(t, subWork, "push", "origin", "HEAD:main")
	runGit(t, subRemote, "symbolic-ref", "HEAD", "refs/heads/main")

	runGit(t, fixture.repoPath, "checkout", "feature/fixer")
	runGit(t, fixture.repoPath, "-c", "protocol.file.allow=always", "submodule", "add", "-b", "main", subRemote, "vendor")
	runGit(t, fixture.repoPath, "commit", "-m", "add vendor submodule")
	runGit(t, fixture.repoPath, "push", "origin", "feature/fixer")
	runGit(t, fixture.repoPath, "checkout", "main")

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

	// Worktrees do not always materialize submodule checkouts; ensure vendor is present.
	vendorPath := filepath.Join(worktree.WorktreePath, "vendor")
	if _, err := os.Stat(filepath.Join(vendorPath, ".git")); err != nil {
		runGit(t, worktree.WorktreePath, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	}
	if _, err := os.Stat(filepath.Join(vendorPath, "module.txt")); err != nil {
		t.Fatalf("submodule content missing after init: %v", err)
	}
	originalModule := readFile(t, filepath.Join(vendorPath, "module.txt"))

	// Top-level tracked dirt + dirty submodule (modified tracked + untracked).
	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "dirty tracked\n")
	writeFile(t, filepath.Join(vendorPath, "module.txt"), "dirty submodule tracked\n")
	writeFile(t, filepath.Join(vendorPath, "untracked-in-sub.txt"), "sub untracked\n")

	result, err := gateway.DiscardWorktreeChanges(ctx, DiscardWorktreeChangesInput{
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: worktree.WorktreePath,
	})
	if err != nil {
		t.Fatalf("DiscardWorktreeChanges() error = %v", err)
	}
	if result.NoOp || !result.WasDirty {
		t.Fatalf("DiscardWorktreeChanges() = %#v, want dirty discard including submodule", result)
	}
	if got := readFile(t, filepath.Join(vendorPath, "module.txt")); got != originalModule {
		t.Fatalf("submodule module.txt after discard = %q, want %q", got, originalModule)
	}
	if _, err := os.Stat(filepath.Join(vendorPath, "untracked-in-sub.txt")); !os.IsNotExist(err) {
		t.Fatalf("submodule untracked file still exists after discard: %v", err)
	}
	clean, err := gateway.WorktreeClean(ctx, worktree.WorktreePath)
	if err != nil || !clean {
		t.Fatalf("WorktreeClean() = %v, %v, want clean after submodule discard", clean, err)
	}
}

func TestGatewayDiscardWorktreeChangesRejectsUnsafePaths(t *testing.T) {
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	ctx := context.Background()
	gateway := fixture.gateway()

	if _, err := gateway.DiscardWorktreeChanges(ctx, DiscardWorktreeChangesInput{
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: fixture.repoPath,
	}); err == nil {
		t.Fatal("DiscardWorktreeChanges(repoPath) error = nil, want safety rejection")
	}

	outside := filepath.Join(fixture.rootDir, "outside-wt")
	mustMkdirAll(t, outside)
	if _, err := gateway.DiscardWorktreeChanges(ctx, DiscardWorktreeChangesInput{
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: outside,
	}); err == nil {
		t.Fatal("DiscardWorktreeChanges(outside) error = nil, want safety rejection")
	}
}

func TestGatewayRestoreWorktreePropagatesHealthCheckFailureForStoredWorktree(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	brokenWorktreePath := filepath.Join(fixture.worktreeRoot, "broken-worktree")
	mustMkdirAll(t, brokenWorktreePath)
	metadata := `{"recovered":false}`
	baseBranch := "main"
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{ID: "broken-record", ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreePath: brokenWorktreePath, Branch: "feature/fixer", BaseBranch: &baseBranch, Status: "active", MetadataJSON: &metadata, CreatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: fixture.now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	_, err := gateway.RestoreWorktree(ctx, RestoreWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, Branch: "feature/fixer", WorktreeRoot: fixture.worktreeRoot})
	if err == nil {
		t.Fatal("RestoreWorktree() error = nil, want health check failure")
	}
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		t.Fatalf("RestoreWorktree() error = %T, want *shell.CommandExecutionError", err)
	}
	if commandErr.Result.ExitCode == 1 {
		t.Fatalf("RestoreWorktree() exit code = %d, want non-1 health check failure", commandErr.Result.ExitCode)
	}
}

type fixture struct {
	rootDir      string
	repoPath     string
	remotePath   string
	worktreeRoot string
	projectID    string
	coordinator  *storage.SQLiteCoordinator
	repos        *storage.Repositories
	now          func() time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	rootDir := t.TempDir()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(rootDir, "state", "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := func() time.Time { return time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC) }
	projectID := "project_1"
	repoPath := filepath.Join(rootDir, "repo")
	worktreeRoot := filepath.Join(rootDir, "worktrees")
	remotePath := filepath.Join(rootDir, "remote.git")
	mustMkdirAll(t, repoPath)
	baseBranch := "main"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: repoPath, BaseBranch: &baseBranch, Archived: false, CreatedAt: now().UTC().Format(javaScriptISOStringLayout), UpdatedAt: now().UTC().Format(javaScriptISOStringLayout)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return &fixture{rootDir: rootDir, repoPath: repoPath, remotePath: remotePath, worktreeRoot: worktreeRoot, projectID: projectID, coordinator: coordinator, repos: repos, now: now}
}

func (f *fixture) gateway() *Gateway {
	return New(Options{GitPath: "git", Repos: f.repos, Now: f.now})
}

func (f *fixture) createMainOnlyRepo(t *testing.T) {
	t.Helper()
	mustMkdirAll(t, f.remotePath)
	runGit(t, f.repoPath, "init", "-b", "main")
	runGit(t, f.remotePath, "init", "--bare")
	configureRepo(t, f.repoPath)
	runGit(t, f.repoPath, "remote", "add", "origin", f.remotePath)
	writeFile(t, filepath.Join(f.repoPath, "README.md"), "hello\n")
	runGit(t, f.repoPath, "add", "README.md")
	runGit(t, f.repoPath, "commit", "-m", "init")
	runGit(t, f.repoPath, "push", "-u", "origin", "main")
}

func (f *fixture) createRemoteRepo(t *testing.T, branch string) {
	t.Helper()
	f.createMainOnlyRepo(t)
	runGit(t, f.repoPath, "checkout", "-b", branch)
	writeFile(t, filepath.Join(f.repoPath, "fix.txt"), "remote change\n")
	runGit(t, f.repoPath, "add", "fix.txt")
	runGit(t, f.repoPath, "commit", "-m", "feature")
	runGit(t, f.repoPath, "push", "-u", "origin", branch)
	runGit(t, f.repoPath, "checkout", "main")
}

func (f *fixture) createUnfetchedRemoteBranch(t *testing.T, branch string) {
	t.Helper()
	clonePath := filepath.Join(f.rootDir, "remote-clone-"+sanitizeBranchName(branch))
	runGit(t, f.rootDir, "clone", f.remotePath, clonePath)
	configureRepo(t, clonePath)
	runGit(t, clonePath, "checkout", "-b", branch)
	writeFile(t, filepath.Join(clonePath, sanitizeBranchName(branch)+".txt"), "remote change\n")
	runGit(t, clonePath, "add", ".")
	runGit(t, clonePath, "commit", "-m", "remote branch")
	runGit(t, clonePath, "push", "-u", "origin", branch)
	if got := stringsTrimSpace(runGitMaybe(t, f.repoPath, "show-ref", "--verify", "refs/remotes/origin/"+branch)); got != "" {
		t.Fatalf("remote tracking ref for %q already exists locally: %q", branch, got)
	}
	if got := stringsTrimSpace(runGitMaybe(t, f.repoPath, "show-ref", "--verify", "refs/heads/"+branch)); got != "" {
		t.Fatalf("local branch %q already exists: %q", branch, got)
	}
}

func (f *fixture) createLocalFeatureRepo(t *testing.T) {
	t.Helper()
	runGit(t, f.repoPath, "init", "-b", "main")
	configureRepo(t, f.repoPath)
	writeFile(t, filepath.Join(f.repoPath, "README.md"), "hello\n")
	runGit(t, f.repoPath, "add", "README.md")
	runGit(t, f.repoPath, "commit", "-m", "init")
	runGit(t, f.repoPath, "checkout", "-b", "feature/fixer")
	runGit(t, f.repoPath, "checkout", "main")
}

func (f *fixture) createLocalFeatureAndOtherRepo(t *testing.T) {
	t.Helper()
	f.createLocalFeatureRepo(t)
	runGit(t, f.repoPath, "checkout", "-b", "feature/other")
	runGit(t, f.repoPath, "checkout", "main")
}

func (f *fixture) createLocalFeatureRepoWithoutReturningToMain(t *testing.T) {
	t.Helper()
	runGit(t, f.repoPath, "init", "-b", "main")
	configureRepo(t, f.repoPath)
	writeFile(t, filepath.Join(f.repoPath, "README.md"), "hello\n")
	runGit(t, f.repoPath, "add", "README.md")
	runGit(t, f.repoPath, "commit", "-m", "init")
	runGit(t, f.repoPath, "checkout", "-b", "feature/fixer")
}

func (f *fixture) advanceRemoteBranch(t *testing.T, branch, fileName, contents string) {
	t.Helper()
	clonePath := filepath.Join(f.rootDir, "remote-clone-"+sanitizeBranchName(branch))
	runGit(t, f.rootDir, "clone", f.remotePath, clonePath)
	configureRepo(t, clonePath)
	runGit(t, clonePath, "checkout", branch)
	writeFile(t, filepath.Join(clonePath, fileName), contents)
	runGit(t, clonePath, "add", fileName)
	runGit(t, clonePath, "commit", "-m", "remote update")
	runGit(t, clonePath, "push", "origin", branch)
}

func (f *fixture) forcePushRemoteBranch(t *testing.T, branch, fileName, contents string) {
	t.Helper()
	clonePath := filepath.Join(f.rootDir, "remote-force-clone-"+sanitizeBranchName(branch))
	runGit(t, f.rootDir, "clone", f.remotePath, clonePath)
	configureRepo(t, clonePath)
	runGit(t, clonePath, "checkout", branch)
	runGit(t, clonePath, "reset", "--hard", "origin/main")
	writeFile(t, filepath.Join(clonePath, fileName), contents)
	runGit(t, clonePath, "add", fileName)
	runGit(t, clonePath, "commit", "-m", "remote force update")
	runGit(t, clonePath, "push", "--force", "origin", branch)
}

func configureRepo(t *testing.T, repoPath string) {
	t.Helper()
	runGit(t, repoPath, "config", "user.email", "test@example.com")
	runGit(t, repoPath, "config", "user.name", "Looper Test")
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(contents)
}

func runGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	output, err := runGitCommand(cwd, args...)
	if err != nil {
		t.Fatalf("git %v error = %v", args, err)
	}
	return output
}

func runGitMaybe(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	output, _ := runGitCommand(cwd, args...)
	return output
}

func runGitCommand(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := stringsTrimSpace(stderr.String())
		if message == "" {
			message = stringsTrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("%s", message)
	}
	return stdout.String(), nil
}

func writeFakeGit(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	tmpPath := filepath.Join(dir, "git.tmp")
	if err := os.WriteFile(tmpPath, []byte(script), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		t.Fatalf("Chmod(%q) error = %v", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatalf("Rename(%q, %q) error = %v", tmpPath, path, err)
	}
	return path
}

func stringsTrimSpace(value string) string {
	return string(bytes.TrimSpace([]byte(value)))
}
