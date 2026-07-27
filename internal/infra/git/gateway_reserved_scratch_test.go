package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsReservedReviewerScratchPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path       string
		ignoreCase bool
		want       bool
	}{
		{path: ".looper-review-1048.json", want: true},
		{path: ".looper-review-.json", want: true}, // Git * matches empty
		{path: ".looper-review-\u00e9.json", want: true},
		{path: ".looper-review-a\\b.json", want: true}, // Unix: backslash is a basename byte
		{path: ".looper-review-a\nb.json", want: true}, // Git * matches newline; Go '.' does not
		{path: `".looper-review-1.json"`, want: false}, // -z pathnames are literal
		{path: "nested/.looper-review-1.json", want: false},
		{path: `.looper-review-1\json`, want: false},
		{path: ".looper-review.json", want: false},
		{path: " .looper-review-x.json", want: false},
		{path: ".looper-review-x.json ", want: false},
		{path: "app.go", want: false},
		{path: "", want: false},
		// Case folding only when core.ignoreCase is true (Git exclude/pathspec).
		{path: ".LOOPER-REVIEW-X.JSON", ignoreCase: false, want: false},
		{path: ".LOOPER-REVIEW-X.JSON", ignoreCase: true, want: true},
		{path: ".Looper-Review-Mixed.JSON", ignoreCase: true, want: true},
		{path: "nested/.LOOPER-REVIEW-X.JSON", ignoreCase: true, want: false},
	}
	for _, tc := range cases {
		if got := isReservedReviewerScratchPath(tc.path, tc.ignoreCase); got != tc.want {
			t.Fatalf("isReservedReviewerScratchPath(%q, ignoreCase=%v) = %v, want %v", tc.path, tc.ignoreCase, got, tc.want)
		}
	}
}

// Dual-classifier real-Git contracts for reserved /.looper-review-*.json.
// Basename edges live in the unit table; cases below cover prepare/exclude,
// negation, prestaged/ITA prepare+commit, rename, tracked M, case-folding,
// and literal pathspecs.
func TestGatewayReservedReviewerScratchContract(t *testing.T) {
	type caseSpec struct {
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
			beforeCommit: func(t *testing.T, wt string) {
				mustMkdirAll(t, filepath.Join(wt, "nested"))
				writeFile(t, filepath.Join(wt, "nested", ".looper-review-1.json"), "{}\n")
			},
			wantClean: &yes, include: []string{"app.go", "nested/.looper-review-1.json"},
			exclude:    []string{".looper-review-1049.json", ".looper-review-.json"},
			keepOnDisk: []string{".looper-review-1049.json", ".looper-review-.json"},
		},
		{
			// Prestaged under negation must unstage in Prepare (retry lifecycle),
			// then Commit must still keep reserved scratch off HEAD.
			name: "prepare_and_commit_unstages_prestaged", branch: "feature/review-prestaged", gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-1050.json"), "{}\n")
				runGit(t, wt, "add", "-A", "--", ".looper-review-1050.json")
			},
			wantClean: &yes, include: []string{"app.go"}, exclude: []string{".looper-review-1050.json"},
			keepOnDisk: []string{".looper-review-1050.json"},
		},
		{
			// git add -N records intent-to-add only; cached diff omits ITA unless
			// --ita-visible-in-index. Retry must still unstage and treat clean.
			name: "prepare_and_commit_intent_to_add", branch: "feature/review-ita", gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				writeFile(t, filepath.Join(wt, ".looper-review-ita.json"), "{}\n")
				runGit(t, wt, "add", "-N", "--", ".looper-review-ita.json")
				// Prove fixture: porcelain is " A", not "??".
				status := runGit(t, wt, "status", "--porcelain", "-z", "--untracked-files=all")
				if !strings.Contains(status, " A .looper-review-ita.json") && !strings.HasPrefix(status, " A ") {
					// -z uses " A\x00path" form; accept either record layout.
					if !strings.Contains(status, " A") || !strings.Contains(status, ".looper-review-ita.json") {
						t.Fatalf("expected intent-to-add porcelain for fixture; status = %q", status)
					}
				}
			},
			wantClean: &yes, include: []string{"app.go"}, exclude: []string{".looper-review-ita.json"},
			keepOnDisk: []string{".looper-review-ita.json"},
		},
		{
			// With core.ignoreCase=true, Git exclude/pathspec fold case; classifiers
			// and :(glob,icase) must ignore uppercase reserved names under negation.
			name: "prepare_and_commit_ignore_case", branch: "feature/review-ignore-case",
			gitignoreNegate: true, coreIgnoreCase: &yes,
			setup: func(t *testing.T, wt string) {
				// Use a distinct uppercase basename that folds to the reserved
				// pattern. On case-insensitive filesystems the on-disk name may
				// preserve the create-time spelling Git reports via -z.
				writeFile(t, filepath.Join(wt, ".LOOPER-REVIEW-CASE.JSON"), "{}\n")
				// Prove Git's exclude folds; negation re-includes it as dirt.
				if out := runGitMaybe(t, wt, "check-ignore", "-v", ".LOOPER-REVIEW-CASE.JSON"); out == "" {
					// Negation makes check-ignore exit 1 (not ignored / re-included).
					// Still require status to see the path as untracked dirt.
				}
				status := runGit(t, wt, "status", "--porcelain", "-z", "--untracked-files=all")
				if !strings.Contains(strings.ToLower(status), ".looper-review-case.json") {
					t.Fatalf("expected uppercase reserved scratch in status under negation; status = %q", status)
				}
			},
			wantClean: &yes, include: []string{"app.go"},
			// Match case-insensitively in committed listing ban check below via keepOnDisk.
			exclude:    []string{".LOOPER-REVIEW-CASE.JSON", ".looper-review-case.json"},
			keepOnDisk: []string{".LOOPER-REVIEW-CASE.JSON"},
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
