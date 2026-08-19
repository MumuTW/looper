package validationcmd

import (
	"context"
	"fmt"
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

// writeFakeGitTool installs an executable git stub reporting execPath for
// --exec-path so tool-root resolution is deterministic across machines. Like
// real git, an inherited GIT_EXEC_PATH wins, so tests can prove the probe
// environment drops operator overrides.
func writeFakeGitTool(t *testing.T) (binary, execPath string) {
	t.Helper()
	toolDir := t.TempDir()
	execPath = filepath.Join(t.TempDir(), "git-core")
	if err := os.MkdirAll(execPath, 0o755); err != nil {
		t.Fatal(err)
	}
	rawBinary := filepath.Join(toolDir, "git")
	script := fmt.Sprintf("#!/bin/sh\nif [ -n \"$GIT_EXEC_PATH\" ]; then printf '%%s\\n' \"$GIT_EXEC_PATH\"; exit 0; fi\nprintf '%%s\\n' %q\n", execPath)
	if err := os.WriteFile(rawBinary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedBinary, err := filepath.EvalSymlinks(rawBinary)
	if err != nil {
		t.Fatal(err)
	}
	resolvedExecPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		t.Fatal(err)
	}
	return resolvedBinary, resolvedExecPath
}

func TestResolvedGitToolRootsIgnoreInheritedExecPathOverride(t *testing.T) {
	gitBinary, gitExecPath := writeFakeGitTool(t)
	secretDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(secretDir, "git-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_EXEC_PATH", filepath.Join(secretDir, "git-core"))
	setDaemonPathWithSecret(t, filepath.Dir(gitBinary))

	profile := buildPermissionProfileForAccess("/workspace/repo", "/private/tmp/assessment", "looper-assessment", workspaceReadOnly)
	if !strings.Contains(profile, strconv.Quote(gitExecPath)+` = "read"`) {
		t.Fatalf("profile missing installation-default exec-path %q: %s", gitExecPath, profile)
	}
	if strings.Contains(profile, strconv.Quote(secretDir)) {
		t.Fatalf("profile grants the inherited GIT_EXEC_PATH directory %q: %s", secretDir, profile)
	}
}

func TestResolvedGitToolRootsBoundHangingProbe(t *testing.T) {
	toolDir := t.TempDir()
	rawBinary := filepath.Join(toolDir, "git")
	if err := os.WriteFile(rawBinary, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(rawBinary)
	if err != nil {
		t.Fatal(err)
	}
	setDaemonPathWithSecret(t, toolDir)

	previous := toolProbeTimeout
	toolProbeTimeout = 250 * time.Millisecond
	defer func() { toolProbeTimeout = previous }()

	started := time.Now()
	roots := resolvedGitToolRoots()
	if len(roots) != 1 || roots[0] != resolved {
		t.Fatalf("resolvedGitToolRoots() = %q, want only the resolved binary after the probe times out", roots)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("hanging --exec-path probe stalled root resolution for %s", elapsed)
	}
}

// setDaemonPathWithSecret prepends a secret-bearing directory to PATH, the
// arrangement issue #556 requires to stay unreadable.
func setDaemonPathWithSecret(t *testing.T, toolDir string) string {
	t.Helper()
	secretDir := t.TempDir()
	t.Setenv("PATH", secretDir+string(os.PathListSeparator)+toolDir)
	return secretDir
}

func TestBuildPermissionProfileDeniesNetworkAndLimitsWrites(t *testing.T) {
	gitBinary, gitExecPath := writeFakeGitTool(t)
	secretDir := setDaemonPathWithSecret(t, filepath.Dir(gitBinary))
	t.Setenv("GOMODCACHE", "/cache/go-mod")

	profile := buildPermissionProfileForAccess("/workspace/repo", "/private/tmp/validation", permissionProfile, workspaceWritable)
	for _, want := range []string{
		`permissions.looper-validation=`,
		`network = { enabled = false }`,
		`":workspace_roots" = { "." = "write" }`,
		`"/private/tmp/validation" = "write"`,
		strconv.Quote(gitBinary) + ` = "read"`,
		strconv.Quote(gitExecPath) + ` = "read"`,
		`"/cache/go-mod" = "read"`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile = %q, missing %q", profile, want)
		}
	}
	if secret := strconv.Quote(secretDir); strings.Contains(profile, secret) {
		t.Fatalf("profile grants inherited PATH directory %s: %s", secret, profile)
	}
	if binDir := strconv.Quote(filepath.Dir(gitBinary)) + ` = "read"`; strings.Contains(profile, binDir) {
		t.Fatalf("profile grants the tool bin directory instead of the resolved binary: %s", profile)
	}
}

func TestBuildAssessmentPermissionProfileReadsWorkspaceAndWritesOnlyTempRoot(t *testing.T) {
	gitBinary, _ := writeFakeGitTool(t)
	secretDir := setDaemonPathWithSecret(t, filepath.Dir(gitBinary))

	profile := buildPermissionProfileForAccess("/workspace/repo", "/private/tmp/assessment", "looper-assessment", workspaceReadOnly)
	for _, want := range []string{
		`permissions.looper-assessment=`,
		`network = { enabled = false }`,
		`":workspace_roots" = { "." = "read" }`,
		`"/private/tmp/assessment" = "write"`,
		strconv.Quote(gitBinary) + ` = "read"`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile = %q, missing %q", profile, want)
		}
	}
	if strings.Contains(profile, `":workspace_roots" = { "." = "write" }`) {
		t.Fatalf("assessment profile grants worktree writes: %q", profile)
	}
	if secret := strconv.Quote(secretDir); strings.Contains(profile, secret) {
		t.Fatalf("assessment profile grants inherited PATH directory %s: %s", secret, profile)
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

// setupLinkedWorktreeWithWorktreeConfig creates a real linked worktree whose
// per-worktree config records a credential helper, the exact arrangement
// issue #556 requires to stay hidden from untrusted assessment.
func setupLinkedWorktreeWithWorktreeConfig(t *testing.T) (worktree, gitDir string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree = filepath.Join(root, "linked")
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Looper Test", "-c", "user.email=looper@example.invalid"}, args...)...)
		cmd.Dir = dir
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("git %v: %v\n%s", args, runErr, output)
		}
	}
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repo, "add", "README.md")
	runGit(repo, "commit", "-m", "initial")
	runGit(repo, "config", "extensions.worktreeConfig", "true")
	runGit(repo, "worktree", "add", "-b", "linked-test", worktree)
	runGit(worktree, "config", "--worktree", "credential.helper", "/usr/local/bin/leak-helper")
	// Split index keeps only the pointer in <gitDir>/index; the shared backing
	// file must be granted or even git status exits fatally.
	runGit(worktree, "update-index", "--split-index")

	data, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktree, gitDir)
	}
	if _, err := os.Stat(filepath.Join(filepath.Clean(gitDir), "config.worktree")); err != nil {
		t.Fatalf("setup did not create config.worktree: %v", err)
	}
	return worktree, gitDir
}

func TestLinkedWorktreeReadRootsOmitPrivateGitDirAndWorktreeConfig(t *testing.T) {
	worktree, gitDir := setupLinkedWorktreeWithWorktreeConfig(t)
	gitDir = resolvedToolPath(gitDir)
	configWorktree := resolvedToolPath(filepath.Join(gitDir, "config.worktree"))

	roots := linkedWorktreeReadRoots(worktree)
	if len(roots) == 0 {
		t.Fatal("linkedWorktreeReadRoots() returned no roots for a real linked worktree")
	}
	for _, root := range roots {
		if samePath(root, gitDir) {
			t.Fatalf("linkedWorktreeReadRoots() grants the whole private git dir %q: %q", gitDir, roots)
		}
		if samePath(root, configWorktree) {
			t.Fatalf("linkedWorktreeReadRoots() grants config.worktree: %q", roots)
		}
	}
	for _, required := range []string{"HEAD", "commondir", "gitdir", "index"} {
		found := false
		for _, root := range roots {
			if samePath(root, filepath.Join(gitDir, required)) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("linkedWorktreeReadRoots() missing %s root required for read-only inspection: %q", required, roots)
		}
	}
	sharedIndex := false
	for _, root := range roots {
		if strings.HasPrefix(filepath.Base(resolvedToolPath(root)), "sharedindex.") {
			sharedIndex = true
			break
		}
	}
	if !sharedIndex {
		t.Fatalf("linkedWorktreeReadRoots() missing the split-index backing file root: %q", roots)
	}

	profile := buildPermissionProfileForAccess(worktree, filepath.Join(t.TempDir(), "assessment"), "looper-assessment", workspaceReadOnly)
	for _, forbidden := range []string{
		strconv.Quote(gitDir) + ` = "read"`,
		strconv.Quote(configWorktree) + ` = "read"`,
		"leak-helper",
	} {
		if strings.Contains(profile, forbidden) {
			t.Fatalf("assessment profile grants forbidden path %q: %s", forbidden, profile)
		}
	}
}

func TestRunDeniesWorktreeConfigAndDaemonPathSecrets(t *testing.T) {
	requireSandboxRuntime(t)
	worktree, gitDir := setupLinkedWorktreeWithWorktreeConfig(t)
	configWorktree := filepath.Join(filepath.Clean(gitDir), "config.worktree")

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "credentials")
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", secretDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, err := Run(context.Background(), Options{
		CWD: worktree,
		Command: fmt.Sprintf(
			`git status --porcelain >/dev/null && test ! -r %s && test ! -r %s`,
			strconv.Quote(configWorktree), strconv.Quote(secret)),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v; stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
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

// writeFakeLauncher installs an executable launcher in a dedicated directory
// so command-root resolution is deterministic across machines.
func writeFakeLauncher(t *testing.T, name, body string) string {
	t.Helper()
	toolDir := t.TempDir()
	rawBinary := filepath.Join(toolDir, name)
	if err := os.WriteFile(rawBinary, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(rawBinary)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestResolvedValidationCommandRootsGrantLauncherFileNotPathDirectory(t *testing.T) {
	launcher := writeFakeLauncher(t, "pnpm", "#!/bin/sh\nprintf 'ok\\n'\n")
	secretDir := setDaemonPathWithSecret(t, filepath.Dir(launcher))

	roots := resolvedValidationCommandRoots("pnpm test --reporter dot")

	if len(roots) != 2 {
		t.Fatalf("roots = %q, want the launcher and its shebang interpreter", roots)
	}
	resolvedSh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	hasLauncher := roots[0] == launcher || roots[1] == launcher
	hasInterpreter := roots[0] == resolvedSh || roots[1] == resolvedSh
	if !hasLauncher || !hasInterpreter {
		t.Fatalf("roots = %q, want launcher %q and interpreter %q", roots, launcher, resolvedSh)
	}
	for _, root := range roots {
		if root == secretDir || root == filepath.Dir(launcher) {
			t.Fatalf("roots grant a PATH directory %q instead of resolved files: %q", root, roots)
		}
	}
}

func TestResolvedValidationCommandRootsOmitInterpreterForPlainBinary(t *testing.T) {
	launcher := writeFakeLauncher(t, "looper-fixture", "\x7fELF-no-shebang\n")
	setDaemonPathWithSecret(t, filepath.Dir(launcher))

	roots := resolvedValidationCommandRoots("looper-fixture build")

	if len(roots) != 1 || roots[0] != launcher {
		t.Fatalf("roots = %q, want only the resolved launcher %q", roots, launcher)
	}
}

func TestResolvedValidationCommandRootsAddNothingForRelativeOrMissingCommands(t *testing.T) {
	for _, command := range []string{
		"./scripts/check.sh",
		"../tools/check",
		"looper-definitely-not-installed --flag",
		"",
		"   ",
		"CI=1",
	} {
		if roots := resolvedValidationCommandRoots(command); len(roots) != 0 {
			t.Fatalf("resolvedValidationCommandRoots(%q) = %q, want no roots", command, roots)
		}
	}
}

func TestLeadingCommandName(t *testing.T) {
	cases := map[string]string{
		"pnpm test":             "pnpm",
		"go vet ./...":          "go",
		"  make   check":        "make",
		"CI=1 pnpm test":        "pnpm",
		"A_B=2 C3=d make check": "make",
		`"pnpm" test`:           "pnpm",
		"'git' status":          "git",
		"/opt/tools/pnpm test":  "/opt/tools/pnpm",
		"./scripts/check.sh":    "./scripts/check.sh",
		"1BAD=x tool":           "1BAD=x",
		"":                      "",
		"   ":                   "",
		`"unterminated quote`:   "",
	}
	for command, want := range cases {
		if got := leadingCommandName(command); got != want {
			t.Fatalf("leadingCommandName(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestRunAllowsConfiguredPathLauncherValidationCommands(t *testing.T) {
	requireSandboxRuntime(t)
	toolDir := t.TempDir()
	launcher := filepath.Join(toolDir, "looper-validation-fixture")
	script := "#!/bin/sh\nprintf 'launcher-ok %s\\n' \"$LOOPER_FIXTURE_MODE\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, command := range []string{
		"looper-validation-fixture",
		"LOOPER_FIXTURE_MODE=assigned looper-validation-fixture",
	} {
		result, err := Run(context.Background(), Options{
			CWD:     t.TempDir(),
			Command: command,
			Timeout: 60 * time.Second,
		})
		if err != nil {
			t.Fatalf("Run(%q) error = %v; stdout=%q stderr=%q", command, err, result.Stdout, result.Stderr)
		}
		if !strings.Contains(result.Stdout, "launcher-ok") {
			t.Fatalf("Run(%q) stdout = %q, want launcher output", command, result.Stdout)
		}
	}
}

func TestRunKeepsLauncherPathSiblingsUnreadable(t *testing.T) {
	requireSandboxRuntime(t)
	toolDir := t.TempDir()
	secret := filepath.Join(toolDir, "launcher-secret")
	if err := os.WriteFile(secret, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(toolDir, "looper-validation-fixture")
	script := fmt.Sprintf("#!/bin/sh\ntest ! -r %s && printf 'no-leak\\n'\n", strconv.Quote(secret))
	if err := os.WriteFile(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, err := Run(context.Background(), Options{
		CWD:     t.TempDir(),
		Command: "looper-validation-fixture",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v; stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "no-leak") {
		t.Fatalf("stdout = %q, want the launcher's sibling to stay unreadable", result.Stdout)
	}
}
