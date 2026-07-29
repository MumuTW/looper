// Package testenv keeps test binaries out of the operator's real ~/.looper.
package testenv

import (
	"fmt"
	"os"
	"testing"
)

// RunTestMain points LOOPER_HOME at a throwaway directory for the whole test
// binary and removes it afterwards.
//
// Runners resolve a project's worktree root from config.DefaultProjectWorktreeRoot
// whenever the project record carries no worktreeRoot metadata. Fixtures that
// omit that metadata therefore create worktrees under the developer's real
// ~/.looper/worktrees, where nothing ever collects them: daemon cleanup plans
// from the worktrees table, and those directories were never registered. Use
// this from TestMain in every package whose tests can reach that fallback.
func RunTestMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "looper-testenv-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testenv: create temporary LOOPER_HOME: %v\n", err)
		return 1
	}
	if err := os.Setenv("LOOPER_HOME", dir); err != nil {
		fmt.Fprintf(os.Stderr, "testenv: set LOOPER_HOME: %v\n", err)
		_ = os.RemoveAll(dir)
		return 1
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	return code
}
