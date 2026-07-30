package runtime

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	"github.com/MumuTW/looper/internal/projects"
)

// detectProjectRepo is the production half of the fix for a checkout looper
// cannot drive: without it, an unrecognized origin host returned an empty
// detection with a nil error, AddProject read that as "nothing detected", and
// the project registered with no repository and no warning.
//
// The projects-side test covers the consumer with a fake detector, which would
// stay green if this function regressed. This one drives the real detector over
// a real git repository so both halves of the cross-component fix are pinned.
func TestDetectProjectRepoReportsUnsupportedOriginHost(t *testing.T) {
	t.Parallel()

	gateway := gitinfra.New(gitinfra.Options{GitPath: "git"})
	view := projects.NewCatalog(config.Config{}).View()

	t.Run("github origin is detected", func(t *testing.T) {
		t.Parallel()
		repoPath := initRepoWithOrigin(t, "https://github.com/acme/app.git")
		detected, err := detectProjectRepo(context.Background(), gateway, view, repoPath)
		if err != nil {
			t.Fatalf("detectProjectRepo() error = %v, want a github.com origin detected", err)
		}
		if detected.Repo != "acme/app" {
			t.Fatalf("detected.Repo = %q, want acme/app", detected.Repo)
		}
	})

	t.Run("non-github origin is reported, not silently empty", func(t *testing.T) {
		t.Parallel()
		repoPath := initRepoWithOrigin(t, "https://code.example.com/acme/app.git")
		detected, err := detectProjectRepo(context.Background(), gateway, view, repoPath)
		if err == nil {
			t.Fatalf("detectProjectRepo() = (%#v, nil), want an error naming the unsupported host", detected)
		}
		if !strings.Contains(err.Error(), "code.example.com") {
			t.Fatalf("error = %v, want the origin host named", err)
		}
		if !strings.Contains(err.Error(), "owner/name") {
			t.Fatalf("error = %v, want the explicit-repository remedy", err)
		}
	})
}

func initRepoWithOrigin(t *testing.T, originURL string) string {
	t.Helper()
	repoPath := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", originURL},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Skipf("git %v unavailable in this environment: %v %s", args, err, stderr.String())
		}
	}
	return repoPath
}
