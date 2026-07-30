package validationcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/processsandbox"
)

func TestBuildPermissionProfileDeniesNetworkAndLimitsWrites(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/tools/bin")
	t.Setenv("GOMODCACHE", "/cache/go-mod")

	profile := buildPermissionProfile("/workspace/repo", "/private/tmp/validation", permissionProfile)
	for _, want := range []string{
		`permissions.looper-validation=`,
		`network = { enabled = false }`,
		`":workspace_roots" = { "." = "write" }`,
		`"/private/tmp/validation" = "write"`,
		`"/usr/bin" = "read"`,
		`"/opt/tools/bin" = "read"`,
		`"/cache/go-mod" = "read"`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile = %q, missing %q", profile, want)
		}
	}
}

func TestIsolatedEnvironmentOmitsDaemonCredentials(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/daemon-agent.sock")
	t.Setenv("LOOPER_CONFIG", "/home/daemon/.looper/config.json")
	t.Setenv("OPENAI_API_KEY", "daemon-secret")

	joined := strings.Join(isolatedEnvironment("/workspace/repo", "/tmp/validation"), "\n")
	for _, forbidden := range []string{"SSH_AUTH_SOCK", "LOOPER_CONFIG", "OPENAI_API_KEY", "daemon-secret"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("isolated environment leaked %q: %s", forbidden, joined)
		}
	}
	for _, required := range []string{"HOME=/tmp/validation/home", "GIT_CONFIG_GLOBAL=/dev/null", "PWD=/workspace/repo", "GOROOT="} {
		if !strings.Contains(joined, required) {
			t.Fatalf("isolated environment = %s, missing %q", joined, required)
		}
	}
}

func TestRunAllowsGitStatusInLinkedWorktree(t *testing.T) {
	requireSandboxRuntime(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "linked")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Looper Test", "-c", "user.email=looper@example.invalid"}, args...)...)
		cmd.Dir = repo
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("git %v: %v\n%s", args, runErr, output)
		}
	}
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "initial")
	runGit("worktree", "add", "-b", "linked-test", worktree)

	result, err := Run(context.Background(), Options{
		CWD:     worktree,
		Command: `git status --porcelain >/dev/null`,
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run(git status) error = %v; stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
	}
}

func TestRunUsesProcessSandboxForCredentialFreeWorkspaceValidation(t *testing.T) {
	requireSandboxRuntime(t)
	root := t.TempDir()
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "daemon-secret")
	if err := os.WriteFile(secret, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", "/tmp/daemon-agent.sock")
	t.Setenv("LOOPER_CONFIG", "/home/daemon/.looper/config.json")

	result, err := Run(context.Background(), Options{
		CWD:     worktree,
		Command: `test -z "$SSH_AUTH_SOCK" && test -z "$LOOPER_CONFIG" && test ! -r ../daemon-secret && touch validation-ok`,
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v; stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
	}
	if _, err := os.Stat(filepath.Join(worktree, "validation-ok")); err != nil {
		t.Fatalf("sandbox did not preserve workspace write: %v", err)
	}
}

func TestRunAllowsInstalledToolchainAndReadOnlyModuleCache(t *testing.T) {
	requireSandboxRuntime(t)
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	result, err := Run(context.Background(), Options{
		CWD:     repoRoot,
		Command: `go test ./internal/validationcmd -run '^TestBuildPermissionProfileDeniesNetworkAndLimitsWrites$' -count=1`,
		Timeout: 180 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run(go test) error = %v; stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
	}
}

func requireSandboxRuntime(t *testing.T) {
	t.Helper()
	if err := processsandbox.Available(); err != nil {
		if os.Getenv("LOOPER_REQUIRE_TRUSTED_SRT") == "1" {
			t.Fatalf("trusted sandbox runtime required: %v", err)
		}
		t.Skipf("trusted sandbox runtime is unavailable: %v", err)
	}
}
