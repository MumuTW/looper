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

	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/storage"
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
	runGit(t, worktree.WorktreePath, "add", "README.md")
	writeFile(t, filepath.Join(worktree.WorktreePath, "timeout-progress.txt"), "first observation\n")
	inspectBeforeCommit, err := gateway.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.WorktreePath, BaseRef: prepared.HeadSHA})
	if err != nil {
		t.Fatalf("InspectHead(before) error = %v", err)
	}
	writeFile(t, filepath.Join(worktree.WorktreePath, "timeout-progress.txt"), "changed contents stay private\n")
	inspectBeforeCommitContentChange, err := gateway.InspectHead(ctx, InspectHeadInput{WorktreePath: worktree.WorktreePath, BaseRef: prepared.HeadSHA})
	if err != nil {
		t.Fatalf("InspectHead(before content change) error = %v", err)
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
	if got := inspectBeforeCommit.StagedFiles; len(got) != 1 || got[0] != "README.md" {
		t.Fatalf("InspectHead(before).StagedFiles = %#v, want [README.md]", got)
	}
	if got := inspectBeforeCommit.UntrackedFiles; len(got) != 1 || got[0] != "timeout-progress.txt" {
		t.Fatalf("InspectHead(before).UntrackedFiles = %#v, want [timeout-progress.txt]", got)
	}
	if inspectBeforeCommit.DiffFingerprint == "" {
		t.Fatal("InspectHead(before).DiffFingerprint = empty, want status fingerprint")
	}
	if inspectBeforeCommitContentChange.DiffFingerprint != inspectBeforeCommit.DiffFingerprint {
		t.Fatalf("status fingerprint changed after content-only update: before=%q after=%q", inspectBeforeCommit.DiffFingerprint, inspectBeforeCommitContentChange.DiffFingerprint)
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

func TestGatewayInspectHeadRecordsExactRenamePathsAndDetachedBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/inspect-head")
	gateway := fixture.gateway()
	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "feature/inspect-head", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	writeFile(t, filepath.Join(worktree.WorktreePath, "before name.txt"), "before\n")
	runGit(t, worktree.WorktreePath, "add", "before name.txt")
	runGit(t, worktree.WorktreePath, "commit", "-m", "add rename source")
	runGit(t, worktree.WorktreePath, "mv", "before name.txt", "after name.txt")

	inspect, err := gateway.InspectHead(ctx, InspectHeadInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath})
	if err != nil {
		t.Fatalf("InspectHead() error = %v", err)
	}
	if inspect.Branch != "feature/inspect-head" {
		t.Fatalf("InspectHead().Branch = %q, want feature/inspect-head", inspect.Branch)
	}
	if got := inspect.ChangedFiles; len(got) != 1 || got[0] != "after name.txt" {
		t.Fatalf("InspectHead().ChangedFiles = %#v, want exact rename destination", got)
	}
	if got := inspect.StagedFiles; len(got) != 1 || got[0] != "after name.txt" {
		t.Fatalf("InspectHead().StagedFiles = %#v, want exact rename destination", got)
	}

	runGit(t, worktree.WorktreePath, "checkout", "--detach")
	detached, err := gateway.InspectHead(ctx, InspectHeadInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath})
	if err != nil {
		t.Fatalf("InspectHead(detached) error = %v", err)
	}
	if detached.Branch != "HEAD" {
		t.Fatalf("InspectHead(detached).Branch = %q, want HEAD", detached.Branch)
	}
}

func TestParseStatusResultRejectsTruncatedOutput(t *testing.T) {
	t.Parallel()

	_, err := parseStatusResult(shell.Result{Stdout: "?? partial-path", StdoutTruncated: true})
	if err == nil || !strings.Contains(err.Error(), "exceeded capture limit") {
		t.Fatalf("parseStatusResult() error = %v, want truncated-output rejection", err)
	}
}

func TestGatewayDetachedPRWorktreeReusesRecordAcrossBranches(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	runGit(t, fixture.repoPath, "checkout", "-b", "reviewer/pr-42-head")
	runGit(t, fixture.repoPath, "push", "-u", "origin", "reviewer/pr-42-head")
	runGit(t, fixture.repoPath, "checkout", "main")
	gateway := fixture.gateway()
	first, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("first CreateWorktree() error = %v", err)
	}
	second, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "reviewer/pr-42-head", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached})
	if err != nil {
		t.Fatalf("second CreateWorktree() error = %v", err)
	}
	if second.ID != first.ID || second.WorktreePath != first.WorktreePath {
		t.Fatalf("records = %#v / %#v, want one detached checkout record", first, second)
	}
	if second.Branch != "reviewer/pr-42-head" {
		t.Fatalf("second.Branch = %q, want reviewer/pr-42-head after path reuse", second.Branch)
	}
	items, err := fixture.repos.Worktrees.ListByProject(ctx, fixture.projectID)
	if err != nil || len(items) != 1 {
		t.Fatalf("Worktrees.ListByProject() = %#v, %v; want one row", items, err)
	}
	if items[0].Branch != "reviewer/pr-42-head" {
		t.Fatalf("stored branch = %q, want reviewer/pr-42-head", items[0].Branch)
	}
	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: first.WorktreePath, Branch: first.Branch}); err == nil || !strings.Contains(err.Error(), "currently claimed") {
		t.Fatalf("CleanupWorktree(stale branch) error = %v, want current-claim refusal", err)
	}
	if _, err := os.Stat(second.WorktreePath); err != nil {
		t.Fatalf("shared checkout removed by stale cleanup: %v", err)
	}
	if err := gateway.CleanupWorktree(ctx, CleanupWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: second.WorktreePath, Branch: "reviewer/pr-42-head"}); err != nil {
		t.Fatalf("CleanupWorktree() error = %v", err)
	}
	cleaned, err := fixture.repos.Worktrees.GetByPath(ctx, second.WorktreePath)
	if err != nil || cleaned == nil || cleaned.Status != "cleaned" {
		t.Fatalf("Worktrees.GetByPath() after CleanupWorktree() = %#v, %v; want cleaned record", cleaned, err)
	}
}

func TestGatewayCreateWorktreeRejectsPathOwnedByOtherProject(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	// Directory names embed project id, so force a collision by planting another
	// project's durable row on the path this project is about to claim.
	input := CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached,
	}
	worktreePath := filepath.Join(fixture.worktreeRoot, buildWorktreeDirectoryName(input))
	nowISO := fixture.now().UTC().Format("2006-01-02T15:04:05.000Z")
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{
		ID: "other-project", Name: "Other", RepoPath: fixture.repoPath, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert(other) error = %v", err)
	}
	if err := fixture.repos.Worktrees.Upsert(ctx, storage.WorktreeRecord{
		ID: "wt-other", ProjectID: "other-project", RepoPath: fixture.repoPath, WorktreePath: worktreePath,
		Branch: "feature/other", Status: "active", CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Worktrees.Upsert(other path) error = %v", err)
	}

	_, err := gateway.CreateWorktree(ctx, input)
	if err == nil {
		t.Fatal("CreateWorktree() error = nil, want project ownership refusal")
	}
	if !strings.Contains(err.Error(), "belongs to project") {
		t.Fatalf("CreateWorktree() error = %v, want project ownership refusal", err)
	}
	stored, err := fixture.repos.Worktrees.GetByPath(ctx, worktreePath)
	if err != nil || stored == nil || stored.ProjectID != "other-project" {
		t.Fatalf("path ownership changed after refusal: %#v, %v", stored, err)
	}
}

func TestGatewayCreateWorktreeReconcilesBranchCollisionBeforePathReuse(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()

	attached, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(attached) error = %v", err)
	}
	reviewer, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "reviewer/pr-42-head", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(reviewer detached) error = %v", err)
	}
	if reviewer.WorktreePath == attached.WorktreePath {
		t.Fatalf("reviewer path = attached path %q; want separate detached path", reviewer.WorktreePath)
	}

	// Detached fixer claims the shared PR path under the same logical branch the
	// attached row still holds. Path wins; the attached branch label must be freed.
	fixer, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(fixer detached) error = %v", err)
	}
	if fixer.ID != reviewer.ID || fixer.WorktreePath != reviewer.WorktreePath {
		t.Fatalf("fixer = %#v, want reuse of reviewer path identity %#v", fixer, reviewer)
	}
	if fixer.Branch != "feature/fixer" {
		t.Fatalf("fixer.Branch = %q, want feature/fixer", fixer.Branch)
	}
	attachedStored, err := fixture.repos.Worktrees.GetByID(ctx, attached.ID)
	if err != nil || attachedStored == nil {
		t.Fatalf("GetByID(attached) = %#v, %v", attachedStored, err)
	}
	if attachedStored.Branch == "feature/fixer" {
		t.Fatalf("attached branch still %q after collision reconcile", attachedStored.Branch)
	}
	if _, err := os.Stat(attached.WorktreePath); err != nil {
		t.Fatalf("attached checkout removed during branch reconcile: %v", err)
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

func TestGatewayCommitExcludesArtifactsWithoutTouchingSharedExcludes(t *testing.T) {
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

	// #71: creating a looper worktree must not write into the repository's
	// COMMON .git/info/exclude — in a linked worktree that file is shared with
	// the main checkout and every sibling.
	commonExcludeRel := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "--git-path", "info/exclude"))
	commonExclude := commonExcludeRel
	if !filepath.IsAbs(commonExclude) {
		commonExclude = filepath.Join(fixture.repoPath, commonExcludeRel)
	}
	if raw, err := os.ReadFile(commonExclude); err == nil {
		for _, pattern := range []string{".pnpm-store/", "node_modules/", "dist/", "*.log"} {
			if strings.Contains(string(raw), pattern) {
				t.Fatalf("common info/exclude contains looper pattern %q after CreateWorktree; shared developer ignore policy was mutated", pattern)
			}
		}
	}

	// The real-world failure: looper's fallback commit must NOT stage a
	// 100MB+ .pnpm-store, while ordinary source (including nested) is staged.
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, ".pnpm-store", "v3"))
	writeFile(t, filepath.Join(worktree.WorktreePath, ".pnpm-store", "v3", "huge.bin"), "artifact\n")
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, "node_modules"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "node_modules", "dep.js"), "module\n")
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, "sub", "logs"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "sub", "logs", "run.log"), "log\n")
	writeFile(t, filepath.Join(worktree.WorktreePath, "app.ts"), "export const x = 1\n")
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, "src"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "src", "lib.ts"), "export const y = 2\n")

	if _, err := gateway.Commit(ctx, CommitInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath, Message: "fallback commit"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	committed := runGit(t, worktree.WorktreePath, "show", "--name-only", "--format=", "HEAD")
	for _, want := range []string{"app.ts", "src/lib.ts"} {
		if !strings.Contains(committed, want) {
			t.Fatalf("commit missing source %q; files = %q", want, committed)
		}
	}
	for _, banned := range []string{".pnpm-store", "node_modules", "run.log"} {
		if strings.Contains(committed, banned) {
			t.Fatalf("commit contains excluded artifact %q; files = %q", banned, committed)
		}
	}
	// The artifacts stay on disk, untracked (ignored, not deleted, not staged).
	if _, err := os.Stat(filepath.Join(worktree.WorktreePath, ".pnpm-store", "v3", "huge.bin")); err != nil {
		t.Fatalf("artifact removed from disk: %v", err)
	}
	ignoredStatus := runGit(t, worktree.WorktreePath, "status", "--porcelain", "--ignored", "--untracked-files=all")
	if !strings.Contains(ignoredStatus, ".pnpm-store/") {
		t.Fatalf("artifact not reported as ignored: %q", ignoredStatus)
	}

	// Main checkout and its status are untouched.
	if mainStatus := runGit(t, fixture.repoPath, "status", "--porcelain"); strings.TrimSpace(mainStatus) != "" {
		t.Fatalf("main checkout dirty after worktree commit: %q", mainStatus)
	}

	// Repositories that intentionally track these paths remain supported:
	// tracked artifact-path files still stage their modifications, while an
	// untracked neighbor stays excluded.
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, "dist"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "dist", "keep.js"), "v1\n")
	runGit(t, worktree.WorktreePath, "add", "-f", "dist/keep.js")
	runGit(t, worktree.WorktreePath, "commit", "-m", "track dist artifact")
	writeFile(t, filepath.Join(worktree.WorktreePath, "dist", "keep.js"), "v2\n")
	writeFile(t, filepath.Join(worktree.WorktreePath, "dist", "junk.js"), "junk\n")
	if _, err := gateway.Commit(ctx, CommitInput{RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, WorktreePath: worktree.WorktreePath, Message: "update tracked artifact"}); err != nil {
		t.Fatalf("Commit(tracked artifact) error = %v", err)
	}
	committed = runGit(t, worktree.WorktreePath, "show", "--name-only", "--format=", "HEAD")
	if !strings.Contains(committed, "dist/keep.js") {
		t.Fatalf("tracked artifact modification not committed; files = %q", committed)
	}
	if strings.Contains(committed, "dist/junk.js") {
		t.Fatalf("untracked artifact neighbor was committed; files = %q", committed)
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

func TestGatewayPushPublishesPinnedCommitWhenHeadAdvances(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createRemoteRepo(t, "feature/fixer")
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "feature/fixer", BaseBranch: "main", PRNumber: 42, CheckoutMode: CheckoutModeDetached,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	baseHeadSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "refs/remotes/origin/feature/fixer"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "validated\n")
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "validated"}); err != nil {
		t.Fatalf("Commit(validated) error = %v", err)
	}
	validatedSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "advanced after validation\n")
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "unvalidated"}); err != nil {
		t.Fatalf("Commit(unvalidated) error = %v", err)
	}
	advancedSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))

	if err := gateway.Push(ctx, PushInput{WorktreePath: worktree.WorktreePath, Branch: "feature/fixer", ExpectedRemoteHeadSHA: baseHeadSHA, LocalHeadSHA: validatedSHA}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	remoteSHA := stringsTrimSpace(runGit(t, fixture.repoPath, "ls-remote", "origin", "refs/heads/feature/fixer"))
	if !strings.HasPrefix(remoteSHA, validatedSHA+"\t") {
		t.Fatalf("remote ref = %q, want validated SHA %s", remoteSHA, validatedSHA)
	}
	if validatedSHA == advancedSHA {
		t.Fatal("test setup did not advance HEAD after validation")
	}
}

func TestGatewayPinnedPushSetsAttachedBranchUpstream(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "worker/pinned-upstream", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	writeFile(t, filepath.Join(worktree.WorktreePath, "README.md"), "validated\n")
	if _, err := gateway.Commit(ctx, CommitInput{WorktreePath: worktree.WorktreePath, Message: "validated"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	validatedSHA := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "HEAD"))

	if err := gateway.Push(ctx, PushInput{WorktreePath: worktree.WorktreePath, Branch: "worker/pinned-upstream", LocalHeadSHA: validatedSHA}); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if got := stringsTrimSpace(runGit(t, worktree.WorktreePath, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")); got != "origin/worker/pinned-upstream" {
		t.Fatalf("upstream = %q, want origin/worker/pinned-upstream", got)
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

	for _, tc := range []struct {
		goos string
		path string
		want string
	}{
		{goos: "darwin", path: "/private/var/tmp/repo", want: "/var/tmp/repo"},
		{goos: "linux", path: "/private/var/tmp/repo", want: "/private/var/tmp/repo"},
		{goos: "darwin", path: "/private-repo/worktree", want: "/private-repo/worktree"},
	} {
		if got := normalizeComparablePathForOS(tc.path, tc.goos); got != tc.want {
			t.Fatalf("normalizeComparablePathForOS(%q, %q) = %q, want %q", tc.path, tc.goos, got, tc.want)
		}
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
		RepoPath:       fixture.repoPath,
		WorktreeRoot:   fixture.worktreeRoot,
		WorktreePath:   worktree.WorktreePath,
		ExpectedBranch: "feature/fixer",
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
		RepoPath:       fixture.repoPath,
		WorktreeRoot:   fixture.worktreeRoot,
		WorktreePath:   worktree.WorktreePath,
		ExpectedBranch: "feature/fixer",
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
		RepoPath:       fixture.repoPath,
		WorktreeRoot:   fixture.worktreeRoot,
		WorktreePath:   worktree.WorktreePath,
		ExpectedBranch: "feature/fixer",
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
		RepoPath:       fixture.repoPath,
		WorktreeRoot:   fixture.worktreeRoot,
		WorktreePath:   worktree.WorktreePath,
		ExpectedBranch: "feature/fixer",
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

func TestGatewayRestoreWorktreeRefusesUnregisteredLinkedCheckout(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()
	retired, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: "feature/fixer", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	writeFile(t, filepath.Join(retired.WorktreePath, "dirty.txt"), "retired project state\n")
	if _, err := fixture.repos.Worktrees.RetireByProject(ctx, fixture.projectID, fixture.now().UTC().Format(javaScriptISOStringLayout)); err != nil {
		t.Fatalf("RetireByProject() error = %v", err)
	}

	_, err = gateway.RestoreWorktree(ctx, RestoreWorktreeInput{
		ProjectID:            fixture.projectID,
		RepoPath:             fixture.repoPath,
		Branch:               "feature/fixer",
		WorktreeRoot:         fixture.worktreeRoot,
		ExpectedWorktreePath: retired.WorktreePath,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to adopt retired worktree") {
		t.Fatalf("RestoreWorktree() error = %v, want refusal to adopt the retired checkout", err)
	}
	if got := readFile(t, filepath.Join(retired.WorktreePath, "dirty.txt")); got != "retired project state\n" {
		t.Fatalf("retired checkout contents = %q, want untouched dirty state", got)
	}
	worktrees, err := fixture.repos.Worktrees.ListByProject(ctx, fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(worktrees) != 1 || worktrees[0].Status != "retired" {
		t.Fatalf("ListByProject() = %#v, want no adoption and retained retirement provenance", worktrees)
	}
}

func TestGatewayRecreatesMissingRetiredWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	gateway := fixture.gateway()
	retired, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	if _, err := fixture.repos.Worktrees.RetireByProject(ctx, fixture.projectID, fixture.now().UTC().Format(javaScriptISOStringLayout)); err != nil {
		t.Fatalf("RetireByProject() error = %v", err)
	}
	if err := os.RemoveAll(retired.WorktreePath); err != nil {
		t.Fatalf("RemoveAll(retired worktree) error = %v", err)
	}

	recreated, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("CreateWorktree() after missing retired checkout = %v", err)
	}
	if normalizeComparablePath(recreated.WorktreePath) != normalizeComparablePath(retired.WorktreePath) || recreated.Status != "active" {
		t.Fatalf("recreated = %#v, want active replacement at %q", recreated, retired.WorktreePath)
	}
}

func TestGatewayRecoversUnpersistedCreatedWorktree(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createLocalFeatureRepo(t)
	input := CreateWorktreeInput{ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot, Branch: "feature/fixer", BaseBranch: "main"}

	created, err := New(Options{GitPath: "git", Now: fixture.now}).CreateWorktree(ctx, input)
	if err != nil {
		t.Fatalf("physical CreateWorktree() error = %v", err)
	}
	recovered, err := fixture.gateway().CreateWorktree(ctx, input)
	if err != nil {
		t.Fatalf("CreateWorktree() after interrupted persistence = %v", err)
	}
	if normalizeComparablePath(recovered.WorktreePath) != normalizeComparablePath(created.WorktreePath) || recovered.Status != "active" {
		t.Fatalf("recovered = %#v, want active claim of %q", recovered, created.WorktreePath)
	}
	stored, err := fixture.repos.Worktrees.GetByPath(ctx, created.WorktreePath)
	if err != nil || stored == nil || stored.Status != "active" {
		t.Fatalf("GetByPath() = %#v, %v; want persisted recovered claim", stored, err)
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

// Follow-up to #71 (post-merge review of PR #206): agent-authored git
// commands must also skip untracked artifacts, via worktree-scoped config
// that never touches the main checkout or siblings; legacy shared-exclude
// blocks from earlier looper versions are removed; and readStatus filters
// untracked artifacts so dirt decisions match what Commit will stage.
func TestGatewayWorktreeScopedExcludesProtectAgentCommits(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	fixture.createMainOnlyRepo(t)
	gateway := fixture.gateway()

	// Seed the legacy managed block in the COMMON info/exclude, plus a user
	// line that must survive the migration.
	commonExcludeRel := stringsTrimSpace(runGit(t, fixture.repoPath, "rev-parse", "--git-path", "info/exclude"))
	commonExclude := commonExcludeRel
	if !filepath.IsAbs(commonExclude) {
		commonExclude = filepath.Join(fixture.repoPath, commonExcludeRel)
	}
	mustMkdirAll(t, filepath.Dir(commonExclude))
	writeFile(t, commonExclude, "my-own-ignore/\n"+worktreeExcludeManagedHeader+"\n.pnpm-store/\nnode_modules/\n.turbo/\ndist/\n.next/\n.cache/\n*.log\n")

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:    fixture.projectID,
		RepoPath:     fixture.repoPath,
		WorktreeRoot: fixture.worktreeRoot,
		Branch:       "feature/agent",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}

	// Legacy managed block removed; the user's own line preserved.
	migrated := readFile(t, commonExclude)
	if strings.Contains(migrated, worktreeExcludeManagedHeader) || strings.Contains(migrated, ".pnpm-store/") {
		t.Fatalf("legacy managed block still present in common exclude: %q", migrated)
	}
	if !strings.Contains(migrated, "my-own-ignore/") {
		t.Fatalf("user content lost during legacy migration: %q", migrated)
	}

	// Agent-style raw `git add -A` inside the worktree skips artifacts.
	mustMkdirAll(t, filepath.Join(worktree.WorktreePath, ".pnpm-store", "v3"))
	writeFile(t, filepath.Join(worktree.WorktreePath, ".pnpm-store", "v3", "huge.bin"), "artifact\n")
	writeFile(t, filepath.Join(worktree.WorktreePath, "app.ts"), "export const x = 1\n")
	runGit(t, worktree.WorktreePath, "add", "-A")
	staged := runGit(t, worktree.WorktreePath, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "app.ts") {
		t.Fatalf("agent add -A did not stage source; staged = %q", staged)
	}
	if strings.Contains(staged, ".pnpm-store") {
		t.Fatalf("agent add -A staged an artifact; staged = %q", staged)
	}

	// The main checkout is untouched by the worktree-scoped config: an
	// artifact dir there still shows in ITS raw status.
	mustMkdirAll(t, filepath.Join(fixture.repoPath, "node_modules"))
	writeFile(t, filepath.Join(fixture.repoPath, "node_modules", "dep.js"), "module\n")
	mainStatus := runGit(t, fixture.repoPath, "status", "--porcelain", "--untracked-files=all")
	if !strings.Contains(mainStatus, "node_modules/dep.js") {
		t.Fatalf("main checkout ignore behavior changed; status = %q", mainStatus)
	}

	// readStatus (dirt decisions) filters untracked artifacts, keeps source.
	runGit(t, worktree.WorktreePath, "reset")
	entries, err := gateway.readStatus(ctx, worktree.WorktreePath)
	if err != nil {
		t.Fatalf("readStatus() error = %v", err)
	}
	var paths []string
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	joined := strings.Join(paths, ",")
	if strings.Contains(joined, ".pnpm-store") {
		t.Fatalf("readStatus reports untracked artifact; entries = %q", joined)
	}
	if !strings.Contains(joined, "app.ts") {
		t.Fatalf("readStatus lost source entry; entries = %q", joined)
	}
}
