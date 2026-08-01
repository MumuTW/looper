package validationcmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/processsandbox"
)

const permissionProfile = "looper-validation"

func TestBuildPermissionProfileDeniesNetworkAndLimitsWrites(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/tools/bin")
	t.Setenv("GOMODCACHE", "/cache/go-mod")

	profile := buildPermissionProfileForAccess("/workspace/repo", "/private/tmp/validation", permissionProfile, workspaceWritable)
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

func TestBuildAssessmentPermissionProfileReadsWorkspaceAndWritesOnlyTempRoot(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/opt/tools/bin")

	profile := buildPermissionProfileForAccess("/workspace/repo", "/private/tmp/assessment", "looper-assessment", workspaceReadOnly)
	for _, want := range []string{
		`permissions.looper-assessment=`,
		`network = { enabled = false }`,
		`":workspace_roots" = { "." = "read" }`,
		`"/private/tmp/assessment" = "write"`,
		`"/usr/bin" = "read"`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile = %q, missing %q", profile, want)
		}
	}
	if strings.Contains(profile, `":workspace_roots" = { "." = "write" }`) {
		t.Fatalf("assessment profile grants worktree writes: %q", profile)
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

func TestLinkedWorktreeReadRootsRejectsForgedGitDir(t *testing.T) {
	worktree := t.TempDir()
	secret := filepath.Join(t.TempDir(), "ssh")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := linkedWorktreeReadRoots(worktree); len(got) != 0 {
		t.Fatalf("linkedWorktreeReadRoots() = %q, want no untrusted metadata roots", got)
	}
	profile := buildPermissionProfileForAccess(worktree, filepath.Join(t.TempDir(), "assessment"), "looper-assessment", workspaceReadOnly)
	if strings.Contains(profile, secret) {
		t.Fatalf("assessment profile grants forged Git root %q: %s", secret, profile)
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

func TestLinkedWorktreeReadRootsExcludesCommonConfig(t *testing.T) {
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
	// Embed a fake credential in the local remote URL (common .git/config).
	runGit("remote", "add", "origin", "https://user:secret-token@example.invalid/repo.git")
	runGit("worktree", "add", "-b", "linked-test", worktree)

	commonDir, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		commonDir = filepath.Clean(filepath.Join(repo, ".git"))
	}
	roots := linkedWorktreeReadRoots(worktree)
	if len(roots) == 0 {
		t.Fatal("linkedWorktreeReadRoots() returned no roots for a real linked worktree")
	}
	haveObjects := false
	for _, rootPath := range roots {
		resolved := rootPath
		if r, err := filepath.EvalSymlinks(rootPath); err == nil {
			resolved = r
		}
		if filepath.Clean(resolved) == filepath.Clean(commonDir) {
			t.Fatalf("linkedWorktreeReadRoots() allowlisted full commonDir %q (exposes config): %q", commonDir, roots)
		}
		if filepath.Base(resolved) == "config" {
			t.Fatalf("linkedWorktreeReadRoots() allowlisted config: %q", roots)
		}
		if filepath.Clean(resolved) == filepath.Clean(filepath.Join(commonDir, "objects")) {
			haveObjects = true
		}
	}
	if !haveObjects {
		t.Fatalf("linkedWorktreeReadRoots() missing objects root: %q", roots)
	}
	profile := buildPermissionProfileForAccess(worktree, filepath.Join(t.TempDir(), "assessment"), "looper-assessment", workspaceReadOnly)
	// Full commonDir grant would appear as quoted path = "read".
	quotedCommon := strconv.Quote(commonDir)
	if strings.Contains(profile, quotedCommon+` = "read"`) {
		t.Fatalf("assessment profile grants full commonDir %s: %s", quotedCommon, profile)
	}
	if strings.Contains(profile, "secret-token") {
		t.Fatalf("assessment profile embeds credential material: %s", profile)
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
