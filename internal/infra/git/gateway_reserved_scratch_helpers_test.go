package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shared fixtures for reserved /.looper-review-*.json dual-classifier contracts.
// Kept out of the case matrices so no single reserved-scratch *_test.go exceeds
// the AGENTS.md single-file subsystem-test growth threshold (300 lines).

type reservedScratchCase struct {
	name, branch    string
	gitignoreNegate bool
	// coreIgnoreCase, when non-nil, forces core.ignoreCase on the branch
	// repo before CreateWorktree (worktrees inherit the shared git dir).
	coreIgnoreCase *bool
	seedFiles      map[string]string
	setup          func(t *testing.T, wt string)
	wantClean      *bool // nil skips PrepareWorktree
	checkExclude   bool
	beforeCommit   func(t *testing.T, wt string)
	include        []string // nil skips Commit
	exclude        []string
	keepOnDisk     []string
}

func runReservedScratchCases(t *testing.T, cases []reservedScratchCase) {
	t.Helper()
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
			if tc.coreIgnoreCase != nil {
				val := "false"
				if *tc.coreIgnoreCase {
					val = "true"
				}
				runGit(t, fixture.repoPath, "config", "core.ignoreCase", val)
			}
			runGit(t, fixture.repoPath, "checkout", "main")

			worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
				ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
				Branch: tc.branch, BaseBranch: "main", PRNumber: 1048,
			})
			if err != nil {
				t.Fatalf("CreateWorktree() error = %v", err)
			}
			if tc.coreIgnoreCase != nil {
				// Worktree shares the repo object database/config; reaffirm on the
				// worktree path so Prepare/Commit classifiers observe the flag.
				val := "false"
				if *tc.coreIgnoreCase {
					val = "true"
				}
				runGit(t, worktree.WorktreePath, "config", "core.ignoreCase", val)
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
					// Case-insensitive FS may preserve a different spelling.
					if tc.coreIgnoreCase != nil && *tc.coreIgnoreCase {
						if alt := findCaseFoldedRootFile(t, worktree.WorktreePath, rel); alt != "" {
							continue
						}
					}
					t.Fatalf("expected %q preserved on disk: %v", rel, err)
				}
			}
		})
	}
}

// findCaseFoldedRootFile returns an existing root basename that EqualFold-matches
// wantRel, or "" if none. Used when core.ignoreCase tests run on case-insensitive
// filesystems that may preserve a different create-time spelling.
func findCaseFoldedRootFile(t *testing.T, dir, wantRel string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	base := filepath.Base(wantRel)
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), base) {
			return e.Name()
		}
	}
	return ""
}

func worktreeExcludePath(t *testing.T, worktreePath string) string {
	t.Helper()
	rel := stringsTrimSpace(runGit(t, worktreePath, "rev-parse", "--git-path", "info/exclude"))
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(worktreePath, rel)
}
