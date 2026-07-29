package validationcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildPermissionProfileDeniesNetworkAndLimitsWrites(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/tools/bin")
	t.Setenv("GOMODCACHE", "/cache/go-mod")

	profile := buildPermissionProfile("/workspace/repo", "/private/tmp/validation")
	for _, want := range []string{
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
	for _, required := range []string{"HOME=/tmp/validation/home", "GIT_CONFIG_GLOBAL=/dev/null", "PWD=/workspace/repo"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("isolated environment = %s, missing %q", joined, required)
		}
	}
}

func TestRunUsesNativeSandboxForCredentialFreeWorkspaceValidation(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex sandbox is not installed")
	}
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
		CWD:          worktree,
		Command:      `test -z "$SSH_AUTH_SOCK" && test -z "$LOOPER_CONFIG" && test ! -r ../daemon-secret && touch validation-ok`,
		Timeout:      10 * time.Second,
		CodexCommand: codex,
	})
	if err != nil {
		t.Fatalf("Run() error = %v; stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
	}
	if _, err := os.Stat(filepath.Join(worktree, "validation-ok")); err != nil {
		t.Fatalf("sandbox did not preserve workspace write: %v", err)
	}
}
