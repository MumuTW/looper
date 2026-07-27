package git

import (
	"path/filepath"
	"strings"
	"testing"
)

// Edge dual-classifier real-Git contracts for reserved /.looper-review-*.json:
// intent-to-add unstage, core.ignoreCase folding, and :(literal) pathspecs for
// basenames that contain backslash or newline. Core prepare/commit matrix lives
// in gateway_reserved_scratch_test.go; helpers in gateway_reserved_scratch_helpers_test.go.
func TestGatewayReservedReviewerScratchEdges(t *testing.T) {
	yes := true
	runReservedScratchCases(t, []reservedScratchCase{
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
	})
}
