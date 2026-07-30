package processsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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

func TestRunReadOnlyContainsMaliciousProcessTree(t *testing.T) {
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
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
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
	if err := os.Symlink(outsideSecret, filepath.Join(repo, "outside-link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "--bare", remote)
	runGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "-c", "user.name=Looper Test", "-c", "user.email=test@example.invalid", "add", "README.md")
	runGit(t, repo, "-c", "user.name=Looper Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial")
	runGit(t, repo, "remote", "add", "origin", "file://"+remote)
	runGit(t, repo, "push", "origin", "HEAD:refs/heads/main")
	before := strings.TrimSpace(runGit(t, remote, "rev-parse", "refs/heads/main"))

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("model-ok"))
	}))
	defer model.Close()
	modelURL := strings.Replace(model.URL, "127.0.0.1", "localhost", 1)

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
git config credential.helper '!printf stolen' >/dev/null 2>&1 || true
git add README.md >/dev/null 2>&1 || true
git -c user.name=Attacker -c user.email=attacker@example.invalid commit --allow-empty -m attack >/dev/null 2>&1 || true
git push origin HEAD:refs/heads/attack >/dev/null 2>&1 || true
(sleep 0.1; printf child-blocked > %q) >/dev/null 2>&1 &
wait
if curl -fsS %q >/dev/null 2>&1; then printf 'direct-network:open\n'; else printf 'direct-network:blocked\n'; fi
curl --noproxy '' -fsS %q
`, absoluteSentinel, srtDefaultSentinel, siblingSentinel, modelURL, modelURL)

	options := Options{
		CWD:         repo,
		Command:     "/bin/sh",
		Args:        []string{"-c", script},
		Environment: ToolEnvironment{PrependPath: []string{filepath.Dir(git), filepath.Dir(curl)}},
		Timeout:     15 * time.Second,
		Profile:     ReadOnlyProfile([]string{repo, filepath.Dir(git), filepath.Dir(curl)}, []string{"localhost"}),
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
	if !strings.Contains(result.Stdout, "read:ok") || !strings.Contains(result.Stdout, "git-read:ok") {
		t.Fatalf("stdout = %q stderr = %q, want repository and Git metadata reads", result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "direct-network:blocked") || strings.Contains(result.Stdout, "direct-network:open") || !strings.Contains(result.Stdout, "model-ok") {
		t.Fatalf("stdout = %q, want direct network denied and brokered model network allowed", result.Stdout)
	}
	for _, secret := range []string{"daemon-github-secret", "daemon-github-secret-2", "daemon-agent.sock", "daemon-config.toml", "outside-secret-marker"} {
		if strings.Contains(result.Stdout, secret) || strings.Contains(result.Stderr, secret) {
			t.Fatalf("sandbox output leaked %q: stdout=%q stderr=%q", secret, result.Stdout, result.Stderr)
		}
	}
	for _, path := range []string{filepath.Join(repo, "cwd-sentinel"), siblingSentinel, absoluteSentinel, srtDefaultSentinel} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("malicious process wrote %s: %v", path, err)
		}
	}
	if raw, err := os.ReadFile(outsideSecret); err != nil || string(raw) != "outside-secret-marker\n" {
		t.Fatalf("symlink write changed outside secret: %q, %v", raw, err)
	}
	after := strings.TrimSpace(runGit(t, remote, "rev-parse", "refs/heads/main"))
	if after != before {
		t.Fatalf("remote main changed from %s to %s", before, after)
	}
	if output, err := exec.Command("git", "--git-dir", remote, "show-ref", "--verify", "refs/heads/attack").CombinedOutput(); err == nil {
		t.Fatalf("malicious push created attack ref: %s", output)
	}
	credentialHelper := exec.Command("git", "config", "--local", "--get", "credential.helper")
	credentialHelper.Dir = repo
	if output, err := credentialHelper.CombinedOutput(); err == nil {
		t.Fatalf("malicious process installed credential helper: %s", output)
	}
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
