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
		path string
		want bool
	}{
		{".looper-review-1048.json", true},
		{".looper-review-.json", true}, // Git * matches empty; classifier must too
		{".looper-review-\u00e9.json", true},
		{".looper-review-a\\b.json", true}, // Unix: backslash is a basename byte
		{".looper-review-a\nb.json", true}, // Git * matches newline; Go '.' does not
		{`".looper-review-1.json"`, false}, // -z pathnames are literal; do not dequote
		{"nested/.looper-review-1.json", false},
		{`.looper-review-1\json`, false}, // no .json suffix
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
		{
			// -z pathnames are literal: a root file whose name includes quote
			// bytes must not be dequoted into the reserved basename.
			name:            "literal_quote_bytes_not_reserved",
			branch:          "feature/review-literal-quotes",
			gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				name := `".looper-review-q.json"`
				writeFile(t, filepath.Join(wt, name), `{"body":"quoted-name"}`+"\n")
				statusZ := runGit(t, wt, "status", "--porcelain", "-z", "--untracked-files=all")
				if !strings.Contains(statusZ, name) {
					t.Fatalf("expected -z porcelain to preserve literal quote pathname; status = %q", statusZ)
				}
			},
			prepare: &prepWant{clean: false},
			commit: &commitWant{
				include: []string{"app.go", `".looper-review-q.json"`},
				exclude: []string{},
			},
			keepOnDisk: []string{`".looper-review-q.json"`},
		},
		{
			// Unix: backslash is a valid basename byte; Git's * matches it.
			name:            "backslash_in_suffix_reserved",
			branch:          "feature/review-backslash-suffix",
			gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				name := `.looper-review-a\b.json`
				writeFile(t, filepath.Join(wt, name), `{"body":"backslash"}`+"\n")
			},
			prepare: &prepWant{clean: true},
			// Stage after prepare so Commit's reset receives a pathspec whose
			// basename contains \; without :(literal), git treats \ as escape,
			// resets nothing, and the scratch lands in the commit.
			beforeCommit: func(t *testing.T, wt string) {
				name := `.looper-review-a\b.json`
				runGit(t, wt, "add", "-A", "--", name)
				if staged := runGit(t, wt, "diff", "--cached", "--name-only", "-z"); !strings.Contains(staged, name) {
					t.Fatalf("expected backslash scratch pre-staged; staged = %q", staged)
				}
			},
			commit:     &commitWant{include: []string{"app.go"}, exclude: []string{`.looper-review-a\b.json`}},
			keepOnDisk: []string{`.looper-review-a\b.json`},
		},
		{
			// Git * matches newline in the suffix; classifier must not use
			// Go regexp '.' which stops at newline.
			name:            "newline_in_suffix_reserved",
			branch:          "feature/review-newline-suffix",
			gitignoreNegate: true,
			setup: func(t *testing.T, wt string) {
				name := ".looper-review-a\nb.json"
				writeFile(t, filepath.Join(wt, name), `{"body":"newline"}`+"\n")
				statusZ := runGit(t, wt, "status", "--porcelain", "-z", "--untracked-files=all")
				if !strings.Contains(statusZ, name) {
					t.Fatalf("expected -z porcelain to preserve newline pathname; status = %q", statusZ)
				}
			},
			prepare:    &prepWant{clean: true},
			commit:     &commitWant{include: []string{"app.go"}, exclude: []string{".looper-review-a\nb.json"}},
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
				// -z keeps pathnames literal (quotes, backslash, newline) so
				// includes/excludes match the real tree paths, not core.quotePath.
				committed := runGit(t, worktree.WorktreePath, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "HEAD")
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
