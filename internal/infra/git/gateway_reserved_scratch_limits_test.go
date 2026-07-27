package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real-Git: a large ordinary staged-addition listing (>256 KiB of non-scratch
// paths) must still Commit successfully. Unstage queries only the reserved root
// namespace, so shell capture bounds cannot reject unrelated worker/planner/fixer
// publishes while still keeping reserved scratch off the commit.
func TestGatewayCommitSucceedsWithLargeNonScratchAdditionListing(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-large-listing"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	// Higher-precedence negation so Commit's git add -A stages reserved scratch;
	// info/exclude alone would hide it and skip the unstage path under test.
	runGit(t, fixture.repoPath, "checkout", branch)
	writeFile(t, filepath.Join(fixture.repoPath, ".gitignore"), "!/.looper-review-*.json\n")
	runGit(t, fixture.repoPath, "add", ".gitignore")
	runGit(t, fixture.repoPath, "commit", "-m", "seed negation for large listing")
	runGit(t, fixture.repoPath, "push", "origin", branch)
	runGit(t, fixture.repoPath, "checkout", "main")

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1048,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	writeFile(t, filepath.Join(wt, "app.go"), "package main\n")
	writeFile(t, filepath.Join(wt, ".looper-review-large.json"), "{}\n")
	// Long basenames: ~1500 additions exceed defaultMaxOutputBytes of -z --name-only
	// for an unscoped listing; pathspec-scoped scratch query stays tiny.
	bulkDir := filepath.Join(wt, "bulk")
	mustMkdirAll(t, bulkDir)
	pad := strings.Repeat("x", 180)
	for i := 0; i < 1500; i++ {
		writeFile(t, filepath.Join(bulkDir, fmt.Sprintf("f%04d_%s.txt", i, pad)), "1\n")
	}
	runGit(t, wt, "add", "-A")
	fullListing, err := runGitCommand(wt, "diff", "--cached", "--name-only", "--diff-filter=A", "--no-renames", "-z")
	if err != nil {
		t.Fatalf("diff --cached full listing error = %v", err)
	}
	if len(fullListing) <= 256*1024 {
		t.Fatalf("fixture full listing is %d bytes; need >256KiB to prove unscoped capture would truncate", len(fullListing))
	}
	if !strings.Contains(fullListing, ".looper-review-large.json") {
		t.Fatalf("fixture full listing missing staged reserved scratch under negation")
	}
	scopedListing, err := runGitCommand(wt, "diff", "--cached", "--name-only", "--diff-filter=A", "--no-renames", "-z",
		"--", ":(glob).looper-review-*.json")
	if err != nil {
		t.Fatalf("diff --cached scoped listing error = %v", err)
	}
	if len(scopedListing) == 0 || !strings.Contains(scopedListing, ".looper-review-large.json") {
		t.Fatalf("scoped listing missing reserved scratch; got %q", scopedListing)
	}
	if len(scopedListing) > 256*1024 {
		t.Fatalf("scoped listing unexpectedly huge (%d bytes)", len(scopedListing))
	}

	if _, err := gateway.Commit(ctx, CommitInput{
		RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: wt, Message: "large non-scratch commit must publish",
	}); err != nil {
		t.Fatalf("Commit() error = %v, want success for large non-scratch listing", err)
	}
	if _, statErr := os.Stat(filepath.Join(wt, ".looper-review-large.json")); statErr != nil {
		t.Fatalf("expected reserved scratch preserved on disk: %v", statErr)
	}
	committed := runGit(t, wt, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
	if strings.Contains(committed, ".looper-review-large.json") {
		t.Fatalf("HEAD unexpectedly contains reserved scratch; files = %q", committed)
	}
	if !strings.Contains(committed, "app.go") {
		t.Fatalf("HEAD missing app.go; files = %q", committed)
	}
	sampleBulk := fmt.Sprintf("bulk/f0000_%s.txt", pad)
	if !strings.Contains(committed, sampleBulk) {
		prefix := committed
		if len(prefix) > 200 {
			prefix = prefix[:200]
		}
		t.Fatalf("HEAD missing bulk sample %q; files prefix = %q", sampleBulk, prefix)
	}
}

// Real-Git InspectHead contract: ordinary large dirty worktrees must not hard-
// fail status classification under the shell's generic 256 KiB default. Worker/
// planner/fixer reconciliation call InspectHead before fallback commit; ~1500
// long pathnames exceed that default but fit the dedicated status budget.
func TestGatewayInspectHeadSucceedsWithLargeOrdinaryStatus(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-status-large-ordinary"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1049,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath

	bulkDir := filepath.Join(wt, "bulk")
	mustMkdirAll(t, bulkDir)
	pad := strings.Repeat("x", 180)
	const n = 1500
	for i := 0; i < n; i++ {
		writeFile(t, filepath.Join(bulkDir, fmt.Sprintf("f%04d_%s.txt", i, pad)), "1\n")
	}
	// One reserved scratch among ordinary dirt must be classified away, not
	// used as a reason to abort the whole inspection.
	writeFile(t, filepath.Join(wt, ".looper-review-large-status.json"), "{}\n")

	statusOut, err := runGitCommand(wt, "status", "--porcelain", "-z", "--untracked-files=all", "--ignored=no")
	if err != nil {
		t.Fatalf("status fixture error = %v", err)
	}
	if len(statusOut) <= 256*1024 {
		t.Fatalf("fixture status is %d bytes; need >256KiB to prove generic shell default would truncate", len(statusOut))
	}
	if len(statusOut) > defaultStatusMaxCapturedBytes {
		t.Fatalf("fixture status is %d bytes; exceeds status budget %d", len(statusOut), defaultStatusMaxCapturedBytes)
	}

	headBefore, err := gateway.getHeadSHA(ctx, wt)
	if err != nil {
		t.Fatalf("getHeadSHA() error = %v", err)
	}
	inspect, err := gateway.InspectHead(ctx, InspectHeadInput{WorktreePath: wt, BaseRef: headBefore})
	if err != nil {
		t.Fatalf("InspectHead() error = %v, want success for large ordinary dirty status", err)
	}
	if !inspect.HasUncommittedChanges {
		t.Fatal("InspectHead().HasUncommittedChanges = false, want true for ordinary bulk dirt")
	}
	// Prepare must also classify complete status (dirty) without truncation error.
	prepared, prepErr := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch})
	if prepErr != nil {
		t.Fatalf("PrepareWorktree() error = %v, want clean classification (dirty, not truncated)", prepErr)
	}
	if prepared.Clean {
		t.Fatal("PrepareWorktree().Clean = true, want false for ordinary bulk dirt")
	}
}

// Real-Git Prepare contract: when status capture is still truncated (budget
// forced down to the shell default for this test), fail closed rather than
// filtering a reserved-only prefix to "clean" and letting a later commit
// publish omitted ordinary dirt.
func TestGatewayPrepareFailsClosedOnTruncatedStatus(t *testing.T) {
	// Production uses defaultStatusMaxCapturedBytes so ordinary large dirt
	// succeeds; pin the low generic shell budget only for this fail-closed proof.
	prev := statusMaxCapturedBytes
	statusMaxCapturedBytes = 256 * 1024
	t.Cleanup(func() { statusMaxCapturedBytes = prev })

	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-status-trunc"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	runGit(t, fixture.repoPath, "checkout", branch)
	writeFile(t, filepath.Join(fixture.repoPath, ".gitignore"), "!/.looper-review-*.json\n")
	runGit(t, fixture.repoPath, "add", ".gitignore")
	runGit(t, fixture.repoPath, "commit", "-m", "seed negation for status truncation")
	runGit(t, fixture.repoPath, "push", "origin", branch)
	runGit(t, fixture.repoPath, "checkout", "main")

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1048,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath

	// Long reserved basenames fill status capture; zz-real.txt sorts after them
	// and is the ordinary dirt that truncation would hide from the prefix.
	pad := strings.Repeat("x", 220)
	const n = 1100
	for i := 0; i < n; i++ {
		name := fmt.Sprintf(".looper-review-%04d-%s.json", i, pad)
		writeFile(t, filepath.Join(wt, name), "{}\n")
	}
	writeFile(t, filepath.Join(wt, "zz-real.txt"), "ordinary dirt\n")

	statusOut, err := runGitCommand(wt, "status", "--porcelain", "-z", "--untracked-files=all", "--ignored=no")
	if err != nil {
		t.Fatalf("status fixture error = %v", err)
	}
	if len(statusOut) <= 256*1024 {
		t.Fatalf("fixture status is %d bytes; need >256KiB so shell capture truncates", len(statusOut))
	}
	if !strings.Contains(statusOut, "zz-real.txt") {
		t.Fatalf("fixture status missing zz-real.txt ordinary dirt")
	}

	_, prepErr := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch})
	if prepErr == nil {
		t.Fatal("PrepareWorktree() error = nil, want fail-closed on truncated status")
	}
	if !strings.Contains(prepErr.Error(), "truncated") {
		t.Fatalf("PrepareWorktree() error = %v, want truncated classification failure", prepErr)
	}
	// Reserved payloads and ordinary dirt must still be on disk (never deleted).
	if _, err := os.Stat(filepath.Join(wt, "zz-real.txt")); err != nil {
		t.Fatalf("expected ordinary dirt preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, fmt.Sprintf(".looper-review-0000-%s.json", pad))); err != nil {
		t.Fatalf("expected reserved scratch preserved: %v", err)
	}
}
