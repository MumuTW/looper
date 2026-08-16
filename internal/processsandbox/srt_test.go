package processsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRuntimePathsPutSealedNodeDirectoryFirstOnPATH(t *testing.T) {
	paths := runtimePaths{command: "/runtime/srt", moduleRoot: "/runtime/lib/node_modules", node: "/sealed/node", ripgrep: "/tools/rg", bwrap: "/tools/bwrap", socat: "/tools/socat"}
	got := paths.executableDirectories()
	if len(got) == 0 || got[0] != "/sealed" {
		t.Fatalf("executableDirectories() = %#v, want sealed Node directory first", got)
	}
}

func TestRuntimePathsAllowReadSealedModuleRoot(t *testing.T) {
	paths := runtimePaths{command: "/runtime/bin/srt", moduleRoot: "/runtime/lib/node_modules"}
	if got := paths.directories(); !contains(got, "/runtime/lib/node_modules") {
		t.Fatalf("directories() = %#v, want sealed module root", got)
	}
}

func TestRuntimePathsAllowReadVerifiedClosureDirectories(t *testing.T) {
	paths := runtimePaths{closureDirectories: []string{"/opt/runtime/lib", "/nix/store/hash/lib"}}
	got := paths.directories()
	for _, want := range []string{"/opt/runtime/lib", "/nix/store/hash/lib"} {
		if !contains(got, want) {
			t.Fatalf("directories() = %#v, want verified closure directory %s", got, want)
		}
	}
}

func TestRunSuppliesReviewedPolicyAndIsolatedEnvironment(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(root, "fake-srt")
	if err := os.WriteFile(runtime, []byte("#!/bin/sh\ncat \"$2\"\nprintf '\\nenv:%s:%s:%s\\n' \"${GH_TOKEN-}\" \"${SSH_AUTH_SOCK-}\" \"${OPENAI_API_KEY-}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_TOKEN", "daemon-token")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/daemon-agent.sock")
	t.Setenv("OPENAI_API_KEY", "daemon-model-secret")

	result, err := Run(context.Background(), Options{
		CWD:            root,
		Command:        "/bin/true",
		Profile:        ReadOnlyProfile([]string{root}, []string{"api.example.com"}),
		runtimeCommand: runtime,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	raw, _, ok := strings.Cut(result.Stdout, "\nenv:")
	if !ok {
		t.Fatalf("stdout = %q, want settings and environment marker", result.Stdout)
	}
	var got settings
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode settings: %v\n%s", err, raw)
	}
	if strings.Join(got.Network.AllowedDomains, ",") != "api.example.com" {
		t.Fatalf("allowed domains = %#v", got.Network.AllowedDomains)
	}
	if got.Network.AllowLocalBinding || len(got.Network.AllowUnixSockets) != 0 {
		t.Fatalf("local network capabilities enabled: %#v", got.Network)
	}
	if got.EnableWeakerNestedSandbox || got.EnableWeakerNetworkIsolation || got.AllowAppleEvents {
		t.Fatalf("weaker sandbox mode enabled: %#v", got)
	}
	if !contains(got.FS.DenyRead, "/") || !contains(got.FS.DenyWrite, resolvedRoot) || contains(got.FS.AllowWrite, resolvedRoot) {
		t.Fatalf("filesystem policy = %#v, want root read denial and read-only cwd", got.FS)
	}
	if !strings.Contains(result.Stdout, "\nenv:::\n") {
		t.Fatalf("runtime inherited daemon credentials: %q", result.Stdout)
	}
}

func TestRunRejectsForgeNetworkDomain(t *testing.T) {
	_, err := Run(context.Background(), Options{
		CWD:            t.TempDir(),
		Command:        "/bin/true",
		Profile:        ReadOnlyProfile(nil, []string{"api.github.com"}),
		runtimeCommand: "/bin/true",
	})
	if err == nil || !strings.Contains(err.Error(), `network domain "api.github.com" is forbidden`) {
		t.Fatalf("Run() error = %v, want forbidden forge domain", err)
	}
}

func TestRunRejectsBroadReadRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/", filepath.Join(home, ".config")} {
		_, err := Run(context.Background(), Options{
			CWD:            t.TempDir(),
			Command:        "/bin/true",
			Profile:        ReadOnlyProfile([]string{root}, nil),
			runtimeCommand: "/bin/true",
		})
		if err == nil || !strings.Contains(err.Error(), "is too broad") {
			t.Fatalf("Run(allowRead=%q) error = %v, want broad read-root rejection", root, err)
		}
	}
}

// A home directory behind a symlink must not defeat the guard: the candidate
// read root stays lexical when it does not exist yet, while home resolves, so
// a single-form prefix comparison misses protected roots (#563). Rejection
// must fire on the spelling mismatch, before any exec.
func TestUnsafeAllowReadRootRejectsProtectedRootsUnderSymlinkedHome(t *testing.T) {
	realHome := t.TempDir()
	linkParent := t.TempDir()
	linkedHome := filepath.Join(linkParent, "home-link")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}
	t.Setenv("HOME", linkedHome)

	for _, rel := range []string{".ssh", ".aws", ".config/gh", ".gitconfig", ".looper"} {
		// Deliberately do not create the protected directory: the lexical
		// candidate is what failed to match before the fix.
		candidate := filepath.Join(linkedHome, rel)
		if !unsafeAllowReadRoot(candidate) {
			t.Fatalf("unsafeAllowReadRoot(%q) = false under symlinked home, want rejection", candidate)
		}
	}
	// An ordinary workspace under the same symlinked home stays admissible.
	if unsafeAllowReadRoot(filepath.Join(linkedHome, "workspace")) {
		t.Fatal("unsafeAllowReadRoot() rejected an ordinary workspace under the symlinked home")
	}
	// resolveExistingPrefix must place a not-yet-created path in resolved
	// space: deepest existing ancestor resolved, tail preserved.
	resolvedRealHome, err := filepath.EvalSymlinks(realHome)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveExistingPrefix(filepath.Join(linkedHome, ".ssh", "keys")); filepath.Dir(got) != filepath.Join(resolvedRealHome, ".ssh") {
		t.Fatalf("resolveExistingPrefix() = %q, want under %q", got, filepath.Join(resolvedRealHome, ".ssh"))
	}
}

// A dangling symlink whose target is a not-yet-created protected path still
// names that location: the link must be read and the target's spelling
// compared, not just the link's parent directory.
func TestUnsafeAllowReadRootFollowsDanglingSymlinkToProtectedPath(t *testing.T) {
	realHome := t.TempDir()
	linkParent := t.TempDir()
	linkedHome := filepath.Join(linkParent, "home-link")
	if err := os.Symlink(realHome, linkedHome); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}
	t.Setenv("HOME", linkedHome)
	credentials := filepath.Join(t.TempDir(), "credentials")
	if err := os.Symlink(filepath.Join(linkedHome, ".ssh"), credentials); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}

	if !unsafeAllowReadRoot(credentials) {
		t.Fatalf("unsafeAllowReadRoot(%q) = false, want the dangling link to ~/.ssh rejected by target", credentials)
	}
}

// Rejecting filesystem roots must not depend on HOME being set: with HOME
// unset an explicit allowRead of "/" still fails validation.
func TestUnsafeAllowReadRootRejectsFilesystemRootWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if !unsafeAllowReadRoot("/") {
		t.Fatal("unsafeAllowReadRoot(\"/\") = false with HOME unset, want rejection before the home lookup")
	}
}

// A protected directory symlinked outside the home directory must guard its
// real target: a read root containing the target location is rejected.
func TestUnsafeAllowReadRootResolvesProtectedRootsOutsideHome(t *testing.T) {
	realHome := t.TempDir()
	secrets := t.TempDir()
	target := filepath.Join(secrets, "ssh")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(realHome, ".ssh")); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}
	t.Setenv("HOME", realHome)

	if !unsafeAllowReadRoot(secrets) {
		t.Fatalf("unsafeAllowReadRoot(%q) = false, want the parent of the symlinked ~/.ssh target rejected", secrets)
	}
}

func TestRunAssessmentReadOnlyContainsMaliciousProcessTree(t *testing.T) {
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Skip("srt is not installed")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	if runtime.GOOS == "darwin" {
		output, resolveErr := exec.Command("xcrun", "-f", "git").Output()
		if resolveErr != nil {
			t.Fatalf("resolve macOS git toolchain: %v", resolveErr)
		}
		git = strings.TrimSpace(string(output))
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "assessment-worktree")
	fileRemote := filepath.Join(root, "file-remote.git")
	loopbackRemote := filepath.Join(root, "loopback-remote.git")
	absoluteSentinel := filepath.Join(root, "absolute-sentinel")
	siblingSentinel := filepath.Join(root, "sibling-sentinel")
	outsideSecret := filepath.Join(root, "outside-secret")
	srtDefaultRoot := "/tmp/claude"
	if err := os.MkdirAll(srtDefaultRoot, 0o700); err != nil {
		t.Fatalf("create SRT default temp root: %v", err)
	}
	srtDefaultSentinel := filepath.Join(srtDefaultRoot, fmt.Sprintf("looper-capability-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(srtDefaultSentinel) })
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideSecret, []byte("outside-secret-marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "--bare", fileRemote)
	runGit(t, root, "init", "--bare", loopbackRemote)
	runGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(repo, "outside-link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "-c", "user.name=Looper Test", "-c", "user.email=test@example.invalid", "add", "README.md")
	runGit(t, repo, "-c", "user.name=Looper Test", "-c", "user.email=test@example.invalid", "add", "outside-link")
	runGit(t, repo, "-c", "user.name=Looper Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	loopbackURL := startGitDaemon(t, git, root)
	runGit(t, repo, "remote", "add", "file", "file://"+fileRemote)
	runGit(t, repo, "remote", "add", "loopback", loopbackURL+"/loopback-remote.git")
	runGit(t, repo, "push", "file", "HEAD:refs/heads/main")
	runGit(t, repo, "push", "loopback", "HEAD:refs/heads/main")
	runGit(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	privateGitDir := absoluteGitPath(t, worktree, "--git-dir")
	commonGitDir := absoluteGitPath(t, worktree, "--git-common-dir")
	beforeHead := strings.TrimSpace(runGit(t, worktree, "rev-parse", "HEAD"))
	beforeIndex, err := os.ReadFile(filepath.Join(privateGitDir, "index"))
	if err != nil {
		t.Fatalf("read linked-worktree index: %v", err)
	}
	beforeConfig, err := os.ReadFile(filepath.Join(commonGitDir, "config"))
	if err != nil {
		t.Fatalf("read linked-worktree common config: %v", err)
	}
	beforeFileRemote := gitRefs(t, fileRemote)
	beforeLoopbackRemote := gitRefs(t, loopbackRemote)

	t.Setenv("GH_TOKEN", "daemon-github-secret")
	t.Setenv("GITHUB_TOKEN", "daemon-github-secret-2")
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(root, "daemon-agent.sock"))
	t.Setenv("LOOPER_CONFIG", filepath.Join(root, "daemon-config.toml"))

	script := fmt.Sprintf(`
set -u
printf 'read:'; test -r README.md && printf 'ok\n'
printf 'credentials:%%s:%%s:%%s:%%s\n' "${GH_TOKEN-}" "${GITHUB_TOKEN-}" "${SSH_AUTH_SOCK-}" "${LOOPER_CONFIG-}"
printf blocked > cwd-sentinel 2>/dev/null || true
printf blocked > ../sibling-sentinel 2>/dev/null || true
printf blocked > %q 2>/dev/null || true
printf blocked > %q 2>/dev/null || true
cat outside-link 2>/dev/null || true
printf blocked > outside-link 2>/dev/null || true
git status --porcelain && printf 'git-read:ok\n'
git rev-parse --git-dir --git-common-dir >/dev/null && printf 'git-metadata-read:ok\n'
git config credential.helper '!printf stolen' >/dev/null 2>&1 || true
printf 'protocol=https\nhost=example.invalid\n\n' | git -c credential.helper='!printf stolen' credential fill >/dev/null 2>&1 || true
git update-index --add --cacheinfo 100644,$(git hash-object -w README.md),index-attack.txt >/dev/null 2>&1 || true
git update-ref refs/heads/assessment-attack HEAD >/dev/null 2>&1 || true
git add README.md >/dev/null 2>&1 || true
git -c user.name=Attacker -c user.email=attacker@example.invalid commit --allow-empty -m attack >/dev/null 2>&1 || true
git push file HEAD:refs/heads/attack-file >/dev/null 2>&1 || true
git push loopback HEAD:refs/heads/attack-loopback >/dev/null 2>&1 || true
(sleep 0.1; printf child-blocked > %q) >/dev/null 2>&1 &
wait
`, absoluteSentinel, srtDefaultSentinel, siblingSentinel)

	options := Options{
		CWD:         worktree,
		Command:     "/bin/sh",
		Args:        []string{"-c", script},
		Environment: ToolEnvironment{PrependPath: []string{filepath.Dir(git)}},
		Timeout:     15 * time.Second,
		Profile:     AssessmentProfile([]string{worktree, privateGitDir, commonGitDir, filepath.Dir(git)}),
	}
	// Local developer installs are commonly user-writable and are rejected by
	// production lookup. CI installs the trusted pinned runtime and exercises
	// that path; local runs still exercise the real SRT containment policy.
	if err := Available(); err != nil {
		if os.Getenv("LOOPER_REQUIRE_TRUSTED_SRT") == "1" {
			t.Fatalf("trusted sandbox runtime required: %v", err)
		}
		options.runtimeCommand = srt
	}
	result, err := Run(context.Background(), options)
	if err != nil {
		t.Fatalf("Run() error = %v; stdout=%q stderr=%q", err, result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "read:ok") || !strings.Contains(result.Stdout, "git-read:ok") || !strings.Contains(result.Stdout, "git-metadata-read:ok") {
		t.Fatalf("stdout = %q stderr = %q, want repository and Git metadata reads", result.Stdout, result.Stderr)
	}
	for _, secret := range []string{"daemon-github-secret", "daemon-github-secret-2", "daemon-agent.sock", "daemon-config.toml", "outside-secret-marker"} {
		if strings.Contains(result.Stdout, secret) || strings.Contains(result.Stderr, secret) {
			t.Fatalf("sandbox output leaked %q: stdout=%q stderr=%q", secret, result.Stdout, result.Stderr)
		}
	}
	for _, path := range []string{filepath.Join(worktree, "cwd-sentinel"), siblingSentinel, absoluteSentinel, srtDefaultSentinel} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("malicious process wrote %s: %v", path, err)
		}
	}
	if raw, err := os.ReadFile(outsideSecret); err != nil || string(raw) != "outside-secret-marker\n" {
		t.Fatalf("symlink write changed outside secret: %q, %v", raw, err)
	}
	if afterIndex, readErr := os.ReadFile(filepath.Join(privateGitDir, "index")); readErr != nil || string(afterIndex) != string(beforeIndex) {
		t.Fatalf("linked-worktree index changed: %v", readErr)
	}
	if afterConfig, readErr := os.ReadFile(filepath.Join(commonGitDir, "config")); readErr != nil || string(afterConfig) != string(beforeConfig) {
		t.Fatalf("linked-worktree common config changed: %v", readErr)
	}
	if afterHead := strings.TrimSpace(runGit(t, worktree, "rev-parse", "HEAD")); afterHead != beforeHead {
		t.Fatalf("linked worktree HEAD changed: before=%s after=%s", beforeHead, afterHead)
	}
	if output, err := exec.Command("git", "--git-dir", commonGitDir, "show-ref", "--verify", "refs/heads/assessment-attack").CombinedOutput(); err == nil {
		t.Fatalf("malicious process created common-dir ref: %s", output)
	}
	if got := strings.TrimSpace(runGit(t, worktree, "status", "--porcelain")); got != "" {
		t.Fatalf("linked worktree status = %q, want clean", got)
	}
	if after := gitRefs(t, fileRemote); after != beforeFileRemote {
		t.Fatalf("file remote changed:\nbefore=%s\nafter=%s", beforeFileRemote, after)
	}
	if after := gitRefs(t, loopbackRemote); after != beforeLoopbackRemote {
		t.Fatalf("loopback remote changed:\nbefore=%s\nafter=%s", beforeLoopbackRemote, after)
	}
	credentialHelper := exec.Command("git", "config", "--local", "--get", "credential.helper")
	credentialHelper.Dir = worktree
	if output, err := credentialHelper.CombinedOutput(); err == nil {
		t.Fatalf("malicious process installed credential helper: %s", output)
	}
}

func startGitDaemon(t *testing.T, git, basePath string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback Git port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback Git port: %v", err)
	}
	command := exec.Command(git, "daemon", "--reuseaddr", "--export-all", "--enable=receive-pack", "--base-path="+basePath, "--listen=127.0.0.1", fmt.Sprintf("--port=%d", port))
	if err := command.Start(); err != nil {
		t.Fatalf("start loopback Git daemon: %v", err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	})
	address := fmt.Sprintf("127.0.0.1:%d", port)
	for attempt := 0; attempt < 20; attempt++ {
		connection, dialErr := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return "git://" + address
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("loopback Git daemon did not listen on %s", address)
	return ""
}

func absoluteGitPath(t *testing.T, cwd, arg string) string {
	t.Helper()
	path := strings.TrimSpace(runGit(t, cwd, "rev-parse", arg))
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(cwd, path))
}

func gitRefs(t *testing.T, gitDir string) string {
	t.Helper()
	output, err := exec.Command("git", "--git-dir", gitDir, "show-ref").CombinedOutput()
	if err != nil && len(output) != 0 {
		t.Fatalf("show refs for %s: %v\n%s", gitDir, err, output)
	}
	return string(output)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func runGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
