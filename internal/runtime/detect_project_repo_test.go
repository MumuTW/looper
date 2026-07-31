package runtime

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
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

	t.Run("non-github origin is classified for the project authority boundary", func(t *testing.T) {
		t.Parallel()
		repoPath := initRepoWithOrigin(t, "https://code.example.com/acme/app.git")
		detected, err := detectProjectRepo(context.Background(), gateway, view, repoPath)
		if err != nil {
			t.Fatalf("detectProjectRepo() error = %v, want non-GitHub origin classification", err)
		}
		if detected.Provider != "code.example.com" {
			t.Fatalf("detected.Provider = %q, want origin host", detected.Provider)
		}
		if detected.Repo != "acme/app" {
			t.Fatalf("detected.Repo = %q, want owner/name", detected.Repo)
		}
	})
}

func TestRuntimeProjectServiceRejectsNonGitHubOriginWithoutExplicitRepo(t *testing.T) {
	workingDir := t.TempDir()
	repoPath := initRepoWithOrigin(t, "https://code.example.com/acme/app.git")
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	rt := New(Options{Config: cfg})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	_, err = rt.Services().Projects.AddProject(context.Background(), projects.AddInput{
		ID: "non-github", Name: "Non GitHub", RepoPath: repoPath, SnapshotMode: projects.SnapshotModeOff,
	})
	var validation projects.ProjectValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("AddProject() error = %T %v, want provider authority validation failure", err, err)
	}
	if !strings.Contains(err.Error(), "code.example.com") || !strings.Contains(err.Error(), "provider bindings are config-file authority") {
		t.Fatalf("AddProject() error = %q, want non-GitHub config-authority guidance", err)
	}
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
