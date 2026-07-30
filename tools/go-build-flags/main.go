package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"github.com/nexu-io/looper/internal/version"
)

func main() {
	overrides := version.BuildOverridesFromEnv(os.Getenv)
	overrides.Version = applyDirtySuffix(overrides.Version, worktreeDirty(runGit))
	_, _ = fmt.Fprint(os.Stdout, version.LDFlags(overrides))
}

// applyDirtySuffix marks a version built from a modified checkout. A stamp that
// names a commit the binary does not actually contain is worse than no stamp at
// all: it sends whoever is debugging to source that never produced the binary.
func applyDirtySuffix(value string, dirty bool) string {
	if !dirty || value == "" {
		return value
	}
	return value + "-dirty"
}

// worktreeDirty reports whether the working tree has uncommitted or untracked
// changes. Untracked files count: a new .go file changes the build.
//
// Uncertainty resolves to clean. Release and CI builds run from a pristine
// checkout and a tarball build has no git at all, so suffixing those on a
// failed probe would corrupt artifact versions to avoid a stamp that was
// accurate anyway. The suffix is added only on positive evidence of dirt.
//
// Only the version string is suffixed. GitCommitSHA keeps its exact value — it
// is a commit id, and appending to it would make it stop being one.
func worktreeDirty(run func(name string, args ...string) ([]byte, error)) bool {
	out, err := run("git", "status", "--porcelain")
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}

func runGit(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}
