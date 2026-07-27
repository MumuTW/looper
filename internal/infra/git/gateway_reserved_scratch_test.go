package git

import (
	"os"
	"path/filepath"
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

// Core dual-classifier real-Git contracts for reserved /.looper-review-*.json:
// prepare/exclude, real dirt, nested lookalikes, negation, prestaged unstage,
// rename detection, and tracked same-name fixtures. Edge cases (ITA, ignoreCase,
// literal pathspecs) live in gateway_reserved_scratch_edges_test.go.
func TestGatewayReservedReviewerScratchContract(t *testing.T) {
	yes, no := true, false
	runReservedScratchCases(t, []reservedScratchCase{
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
	})
}
