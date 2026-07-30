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
// --untracked-files=all is passed explicitly so a user's
// status.showUntrackedFiles=no cannot defeat the untracked-file contract:
// without it, a checkout whose only change is a new untracked .go file would
// probe as clean and the build would keep the wrong stamp.
//
// internal/dashboard/assets is excluded because the build itself writes there
// before this probe runs: ci.yml and release.yml both run `pnpm run build`
// first, and vite's emptyOutDir wipes the directory — deleting the tracked
// .gitkeep and emitting untracked bundles. Without the exclusion every release
// artifact would carry -dirty, which is exactly the false stamp this tool
// exists to prevent. Hand edits under a generated output directory are not a
// signal worth protecting.
//
// Uncertainty resolves to clean. A tarball build has no git at all, so
// suffixing on a failed probe would corrupt artifact versions to avoid a stamp
// that was accurate anyway. The suffix is added only on positive evidence.
//
// Only the version string is suffixed. GitCommitSHA keeps its exact value — it
// is a commit id, and appending to it would make it stop being one.
func worktreeDirty(run func(name string, args ...string) ([]byte, error)) bool {
	out, err := run("git", "status", "--porcelain", "--untracked-files=all",
		"--", ":(exclude)internal/dashboard/assets")
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}

func runGit(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}
