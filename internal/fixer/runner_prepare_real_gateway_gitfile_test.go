package fixer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/worktreesafety"
)

func TestRunPrepareWorktreeStepRealGatewayRecreatesInvalidGitfilePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rewrite func(string) string
	}{
		{
			name: "uppercase",
			rewrite: func(content string) string {
				return strings.Replace(content, "gitdir: ", "GITDIR: ", 1)
			},
		},
		{
			name: "missing space",
			rewrite: func(content string) string {
				return strings.Replace(content, "gitdir: ", "gitdir:", 1)
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := setupRealLinkedWorktree(t, "project_real_invalid_gitfile_"+strings.ReplaceAll(tt.name, " ", "_"), "feature/fix-42")
			gitfile := filepath.Join(f.wtPath, ".git")
			data, err := os.ReadFile(gitfile)
			if err != nil {
				t.Fatalf("ReadFile .git: %v", err)
			}
			if err := os.WriteFile(gitfile, []byte(tt.rewrite(string(data))), 0o644); err != nil {
				t.Fatalf("WriteFile .git: %v", err)
			}
			if err := tryRunGit(f.wtPath, "rev-parse", "--git-dir"); err == nil {
				t.Fatal("git rev-parse accepted invalid gitfile prefix")
			}
			stripWorktreeExceptGit(t, f.wtPath)

			probeErr := errors.New("fatal: invalid gitfile format: .git")
			if worktreesafety.LocalFixerWorktreeCheckoutUsable(f.wtPath) {
				t.Fatal("LocalFixerWorktreeCheckoutUsable = true, want false")
			}
			if !worktreesafety.IsMissingOrUnusableFixerWorktree(f.wtPath, probeErr) {
				t.Fatal("IsMissingOrUnusableFixerWorktree = false, want true")
			}
			assertRealGatewayRecreatesUnusableWorktree(t, f)
		})
	}
}
