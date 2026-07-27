package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsReservedReviewerScratchPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{".looper-review-1048.json", true},
		{".looper-review-.json", true}, // Git * matches empty
		{".looper-review-\u00e9.json", true},
		{".looper-review-a\\b.json", true}, // Unix: backslash is a basename byte
		{".looper-review-a\nb.json", true}, // Git * matches newline; Go '.' does not
		{`".looper-review-1.json"`, false}, // -z pathnames are literal
		{"nested/.looper-review-1.json", false},
		{`.looper-review-1\json`, false},
		{".looper-review.json", false},
		{" .looper-review-x.json", false},
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

// Dual-classifier real-Git contracts for reserved /.looper-review-*.json.
// Basename edges live in the unit table; cases below cover prepare/exclude,
// negation, prestaged, rename, tracked M, and literal pathspec unstage.
func TestGatewayReservedReviewerScratchContract(t *testing.T) {
	type caseSpec struct {
		name, branch    string
		gitignoreNegate bool
		seedFiles       map[string]string
		setup           func(t *testing.T, wt string)
		wantClean       *bool // nil skips PrepareWorktree
		checkExclude    bool
		beforeCommit    func(t *testing.T, wt string)
		include         []string // nil skips Commit
		exclude         []string
		keepOnDisk      []string
	}
	yes, no := true, false
	cases := []caseSpec{
		{
			name: "prepare_ignores_scratch_reconciles_exclude", branch: "feature/review-scratch",
			setup: func(t *testing.T, wt string) {
				writeFile(t, worktreeExcludePath(t, wt), "# stale exclude\n")
				writeFile(t, filepath.Join(wt, ".looper-review-1048.json"), "{}\n")
			},
			wantClean: &yes, checkExclude: true, keepOnDisk: []string{".looper-review-1048.json"},
		},
		{
			name: "prepare_real_dirt_still_dirty", branch: "feature/review-real-dirt",
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-1048.json"), "{}\n")
				writeFile(t, filepath.Join(wt, "partial-fix.go"), "package main\n")
			},
			wantClean: &no,
		},
		{
			name: "prepare_nested_lookalike_dirty", branch: "feature/review-nested",
			setup: func(t *testing.T, wt string) {
				mustMkdirAll(t, filepath.Join(wt, "nested"))
				writeFile(t, filepath.Join(wt, "nested", ".looper-review-1.json"), "{}\n")
			},
			wantClean: &no,
		},
		{
			name: "negation_prepare_and_commit", branch: "feature/review-negation", gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-.json"), "{}\n")
				writeFile(t, filepath.Join(wt, ".looper-review-1049.json"), "{}\n")
			},
			// Nested lookalike is commit-only dirt so Prepare can stay clean under negation.
			beforeCommit: func(t *testing.T, wt string) {
				mustMkdirAll(t, filepath.Join(wt, "nested"))
				writeFile(t, filepath.Join(wt, "nested", ".looper-review-1.json"), "{}\n")
			},
			wantClean: &yes, include: []string{"app.go", "nested/.looper-review-1.json"},
			exclude:    []string{".looper-review-1049.json", ".looper-review-.json"},
			keepOnDisk: []string{".looper-review-1049.json", ".looper-review-.json"},
		},
		{
			name: "commit_unstages_prestaged", branch: "feature/review-prestaged", gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-1050.json"), "{}\n")
				runGit(t, wt, "add", "-A", "--", ".looper-review-1050.json")
			},
			include: []string{"app.go"}, exclude: []string{".looper-review-1050.json"},
			keepOnDisk: []string{".looper-review-1050.json"},
		},
		{
			name: "commit_unstages_despite_rename_detection", branch: "feature/review-rename",
			gitignoreNegate: true, seedFiles: map[string]string{"old.json": "{}\n"},
			setup: func(t *testing.T, wt string) {
				if err := os.Remove(filepath.Join(wt, "old.json")); err != nil {
					t.Fatalf("Remove old.json: %v", err)
				}
				writeFile(t, filepath.Join(wt, ".looper-review-1.json"), "{}\n")
				writeFile(t, filepath.Join(wt, "app.go"), "package main\n")
				runGit(t, wt, "add", "-A")
			},
			include: []string{"app.go", "old.json"}, exclude: []string{".looper-review-1.json"},
			keepOnDisk: []string{".looper-review-1.json"},
		},
		{
			name: "commit_includes_tracked_reserved_name", branch: "feature/review-tracked-name",
			seedFiles: map[string]string{".looper-review-fixture.json": `{"fixture":true}` + "\n"},
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-fixture.json"), `{"fixture":"updated"}`+"\n")
				writeFile(t, filepath.Join(wt, ".looper-review-1051.json"), "{}\n")
			},
			include: []string{"app.go", ".looper-review-fixture.json"}, exclude: []string{".looper-review-1051.json"},
			keepOnDisk: []string{".looper-review-1051.json"},
		},
		{
			// Without :(literal), git treats \ as escape and leaves scratch staged.
			name: "backslash_in_suffix_reserved", branch: "feature/review-backslash-suffix", gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, `.looper-review-a\b.json`), "{}\n")
			},
			wantClean: &yes,
			beforeCommit: func(t *testing.T, wt string) {
				runGit(t, wt, "add", "-A", "--", `.looper-review-a\b.json`)
			},
			include: []string{"app.go"}, exclude: []string{`.looper-review-a\b.json`},
			keepOnDisk: []string{`.looper-review-a\b.json`},
		},
		{
			name: "newline_in_suffix_reserved", branch: "feature/review-newline-suffix", gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-a\nb.json"), "{}\n")
			},
			wantClean: &yes, include: []string{"app.go"}, exclude: []string{".looper-review-a\nb.json"},
			keepOnDisk: []string{".looper-review-a\nb.json"},
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
				runGit(t, fixture.repoPath, "commit", "-m", "seed reserved-scratch contract")
				runGit(t, fixture.repoPath, "push", "origin", tc.branch)
			}
			runGit(t, fixture.repoPath, "checkout", "main")

			worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
				ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
				Branch: tc.branch, BaseBranch: "main", PRNumber: 1048,
			})
			if err != nil {
				t.Fatalf("CreateWorktree() error = %v", err)
			}
			if tc.setup != nil {
				tc.setup(t, worktree.WorktreePath)
			}
			if tc.wantClean != nil {
				prepared, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: worktree.WorktreePath, Branch: tc.branch})
				if err != nil {
					t.Fatalf("PrepareWorktree() error = %v", err)
				}
				if prepared.Clean != *tc.wantClean {
					t.Fatalf("PrepareWorktree().Clean = %v, want %v", prepared.Clean, *tc.wantClean)
				}
			}
			if tc.checkExclude {
				if content := readFile(t, worktreeExcludePath(t, worktree.WorktreePath)); !strings.Contains(content, "/.looper-review-*.json") {
					t.Fatalf("PrepareWorktree did not reconcile reserved exclude; content = %q", content)
				}
			}
			if tc.include != nil {
				if _, err := os.Stat(filepath.Join(worktree.WorktreePath, "app.go")); os.IsNotExist(err) {
					writeFile(t, filepath.Join(worktree.WorktreePath, "app.go"), "package main\n")
				}
				if tc.beforeCommit != nil {
					tc.beforeCommit(t, worktree.WorktreePath)
				}
				if _, err := gateway.Commit(ctx, CommitInput{
					RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
					WorktreePath: worktree.WorktreePath, Message: "reserved scratch " + tc.name,
				}); err != nil {
					t.Fatalf("Commit() error = %v", err)
				}
				committed := runGit(t, worktree.WorktreePath, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
				for _, want := range tc.include {
					if !strings.Contains(committed, want) {
						t.Fatalf("Commit() missing %q; files = %q", want, committed)
					}
				}
				for _, ban := range tc.exclude {
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

// Real-Git: staged addition names over shell capture (256 KiB) must fail closed
// so residual reserved scratch past the prefix cannot land in the commit.
func TestGatewayCommitRejectsTruncatedStagedAdditionListing(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-trunc-listing"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1048,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	writeFile(t, filepath.Join(wt, "app.go"), "package main\n")
	writeFile(t, filepath.Join(wt, ".looper-review-trunc.json"), "{}\n")
	// Long basenames: ~1500 additions exceed defaultMaxOutputBytes of -z --name-only.
	bulkDir := filepath.Join(wt, "bulk")
	mustMkdirAll(t, bulkDir)
	pad := strings.Repeat("x", 180)
	for i := 0; i < 1500; i++ {
		writeFile(t, filepath.Join(bulkDir, fmt.Sprintf("f%04d_%s.txt", i, pad)), "1\n")
	}
	runGit(t, wt, "add", "-A")
	listing, err := runGitCommand(wt, "diff", "--cached", "--name-only", "--diff-filter=A", "--no-renames", "-z")
	if err != nil {
		t.Fatalf("diff --cached listing error = %v", err)
	}
	if len(listing) <= 256*1024 {
		t.Fatalf("fixture listing is %d bytes; need >256KiB to exercise truncation", len(listing))
	}

	_, err = gateway.Commit(ctx, CommitInput{
		RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		WorktreePath: wt, Message: "must refuse truncated staged addition list",
	})
	if err == nil {
		t.Fatal("Commit() error = nil, want truncation refusal")
	}
	if !strings.Contains(err.Error(), "staged addition list truncated") {
		t.Fatalf("Commit() error = %v, want staged addition list truncated", err)
	}
	if _, statErr := os.Stat(filepath.Join(wt, ".looper-review-trunc.json")); statErr != nil {
		t.Fatalf("expected reserved scratch preserved on disk: %v", statErr)
	}
	committed := runGit(t, wt, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
	if strings.Contains(committed, ".looper-review-trunc.json") {
		t.Fatalf("HEAD unexpectedly contains reserved scratch; files = %q", committed)
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
