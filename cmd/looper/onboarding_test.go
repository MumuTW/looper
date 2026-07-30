// Onboarding verbs (init, status, project add|list|discover) and the shared fake daemon
// they are exercised against. Kept apart from main_test.go, which covers the
// argument-to-request mapping for the control verbs: the two surfaces fail in
// different ways — routing is a pure function, onboarding is several daemon
// calls plus the filesystem — and neither reads better interleaved with the
// other.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/version"
)

// fakeDaemon stands in for looperd. Only the routes the onboarding verbs call
// are served; anything else fails the test, so a verb that starts talking to a
// different endpoint cannot pass silently.
type fakeDaemon struct {
	server       *httptest.Server
	healthy      bool
	projects     []map[string]any
	createBody   map[string]any
	createStatus int
	createError  string
	// createDelay stands in for the worktree and pull request discovery the
	// daemon runs after it has already committed the project.
	createDelay time.Duration

	// Optional /api/v1/status fields for ops-readiness lines on `looper status`.
	looperPath                  string
	includeReviewPublish        bool
	reviewKnown                 bool
	reviewCapable               bool
	reviewCapability            string
	reviewPublishingDisabled    bool
	reviewReason                string
	quarantinedActiveExecutions int
	quarantinedRunningRuns      int
	degradedReasons             []string

	// labelsInit captures the body of POST /api/v1/labels/init and replies with
	// labelsInitResult (or labelsInitStatus when non-zero). labelsInitRepo is
	// echoed back so a test can assert the slug the CLI forwarded.
	labelsInitBody   map[string]any
	labelsInitResult map[string]any
	labelsInitStatus int
	labelsInitError  string
	labelsInitRepo   string
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	daemon := &fakeDaemon{healthy: true, projects: []map[string]any{}}
	daemon.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/healthz" && r.Method == http.MethodGet:
			writeEnvelope(w, http.StatusOK, map[string]any{"healthy": daemon.healthy})
		case r.URL.Path == "/api/v1/status" && r.Method == http.MethodGet:
			tools := map[string]any{"looperPath": daemon.looperPath}
			if daemon.includeReviewPublish {
				tools["reviewPublish"] = map[string]any{
					"known":              daemon.reviewKnown,
					"capable":            daemon.reviewCapable,
					"capability":         daemon.reviewCapability,
					"publishingDisabled": daemon.reviewPublishingDisabled,
					"reason":             daemon.reviewReason,
				}
			}
			writeEnvelope(w, http.StatusOK, map[string]any{
				"service": map[string]any{
					"healthy":         daemon.healthy,
					"degradedReasons": daemon.degradedReasons,
					"recovery": map[string]any{
						"outstanding": map[string]any{
							"quarantinedActiveExecutions": daemon.quarantinedActiveExecutions,
							"quarantinedRunningRuns":      daemon.quarantinedRunningRuns,
						},
					},
				},
				"tools": tools,
			})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			writeEnvelope(w, http.StatusOK, map[string]any{"items": daemon.projects})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodPost:
			body := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create project body: %v", err)
			}
			daemon.createBody = body
			if daemon.createStatus != 0 {
				w.WriteHeader(daemon.createStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":    false,
					"error": map[string]any{"code": "PROJECT_ID_CONFLICT", "message": daemon.createError},
				})
				return
			}
			repoPath, _ := body["repoPath"].(string)
			writeEnvelope(w, http.StatusOK, map[string]any{
				"id":         "repo",
				"name":       "repo",
				"repoPath":   repoPath,
				"baseBranch": "main",
				"archived":   false,
				"discovery":  map[string]any{"status": "pending"},
				"warnings":   []string{"Could not detect repository from git remote"},
			})
		case r.URL.Path == "/api/v1/projects/repo/discover" && r.Method == http.MethodPost:
			writeEnvelope(w, http.StatusOK, map[string]any{
				"id": "repo",
				"discovery": map[string]any{
					"status":                 "succeeded",
					"discoveredWorktrees":    2,
					"discoveredPullRequests": 1,
				},
			})
		case r.URL.Path == "/api/v1/labels/init" && r.Method == http.MethodPost:
			body := map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode labels init body: %v", err)
			}
			daemon.labelsInitBody = body
			if daemon.labelsInitStatus != 0 {
				w.WriteHeader(daemon.labelsInitStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok":    false,
					"error": map[string]any{"code": "VALIDATION_FAILED", "message": daemon.labelsInitError},
				})
				return
			}
			result := daemon.labelsInitResult
			if result == nil {
				repo, _ := body["repo"].(string)
				daemon.labelsInitRepo = repo
				result = map[string]any{
					"repo":   repo,
					"dryRun": false,
					"labels": []map[string]any{
						{"name": "looper:worker-ready", "status": "created", "color": "0052cc", "description": "Picked up automatically by worker"},
						{"name": "looper:plan", "status": "skipped", "color": "5319e7", "description": "Picked up automatically by planner"},
					},
					"summary": map[string]any{"created": 1, "updated": 0, "skipped": 1, "failed": 0},
				}
			}
			writeEnvelope(w, http.StatusOK, result)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(daemon.server.Close)
	return daemon
}

func writeEnvelope(w http.ResponseWriter, status int, data any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data, "requestId": "test"})
}

// configForDaemon writes a config pointing at address and selects it for the
// process under test, the same way an operator's LOOPER_CONFIG would.
func configForDaemon(t *testing.T, address string) string {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatalf("parse daemon address %q: %v", address, err)
	}
	body := fmt.Sprintf("[server]\nhost = %q\nport = %s\n", parsed.Hostname(), parsed.Port())
	return writeConfigFile(t, body)
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("LOOPER_CONFIG", path)
	return path
}

// gitRepo creates a real repository, because project add now asks git whether
// the path is one. A hand-made .git directory is exactly the case the CLI is
// supposed to reject, so it cannot double as the fixture for the happy path.
//
// The returned path is the root as git reports it: t.TempDir() hands back a
// path under a symlinked prefix on macOS (/var -> /private/var), and git
// resolves that. Tests compare against what the CLI will actually send.
func gitRepo(t *testing.T) string {
	t.Helper()
	return gitRepoAt(t, t.TempDir())
}

func gitRepoAt(t *testing.T, root string) string {
	t.Helper()
	command := exec.Command("git", "init", "--quiet", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v (%s)", root, err, output)
	}
	return canonicalPath(t, root)
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("canonicalize %s: %v", path, err)
	}
	return resolved
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := run(args, strings.NewReader(""), stdout, stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunPrintsVersion(t *testing.T) {
	code, stdout, stderr := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := stdout, version.Value+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRunRejectsUnknownVerbWithUsage(t *testing.T) {
	code, _, stderr := runCLI(t, "bootstrap")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "bootstrap"`) {
		t.Fatalf("stderr = %q, want it to name the unknown command", stderr)
	}
	if !strings.Contains(stderr, "looper init") {
		t.Fatalf("stderr = %q, want usage text listing the onboarding verbs", stderr)
	}
}

func TestUsageListsEveryImplementedVerb(t *testing.T) {
	buffer := &bytes.Buffer{}
	usage(buffer)
	for _, verb := range []string{
		"looper init", "looper status", "looper project add", "looper project list",
		"looper labels init", "looper stop", "looper close", "looper takeover",
		"looper handback", "looper retry", "looper start", "looper pause",
		"looper respond", "looper version",
	} {
		if !strings.Contains(buffer.String(), verb) {
			t.Errorf("usage does not document %q", verb)
		}
	}
}

// TestOnboardingVerbsAcceptGlobalFlagsInEitherPosition is the onboarding half
// of the reachability fix. These verbs are routed after splitGlobalFlags, which
// is what lets --config work before the verb as well as after it; the failure
// it guards is silent, because a --config read as an operand would leave status
// reporting the default config while looking like it honoured the flag.
func TestOnboardingVerbsAcceptGlobalFlagsInEitherPosition(t *testing.T) {
	// The empty string is a placeholder for the config path, which only exists
	// once the fake daemon has an address.
	for _, args := range [][]string{
		{"--config", "", "status"},
		{"status", "--config", ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			daemon := newFakeDaemon(t)
			path := configForDaemon(t, daemon.server.URL)
			// LOOPER_CONFIG would answer for the flag, so clear it: only the
			// flag can select the file here.
			t.Setenv("LOOPER_CONFIG", "")

			invocation := append([]string(nil), args...)
			for index, arg := range invocation {
				if arg == "" {
					invocation[index] = path
				}
			}

			code, stdout, stderr := runCLI(t, invocation...)
			if code != 0 {
				t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
			}
			if !strings.Contains(stdout, path) {
				t.Fatalf("stdout = %q, want the config named by --config", stdout)
			}
			if !strings.Contains(stdout, "reachable") {
				t.Fatalf("stdout = %q, want the daemon the flag selected to be reached", stdout)
			}
		})
	}
}

// TestOnboardingVerbsRefuseUnknownFlags keeps the onboarding verbs inside
// splitGlobalFlags' refusal: a flag nobody acts on must not be swallowed as a
// repository path or silently ignored.
func TestOnboardingVerbsRefuseUnknownFlags(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--force"},
		{"init", "--force"},
		{"project", "add", "--force"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, stderr := runCLI(t, args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, `unknown flag "--force"`) {
				t.Fatalf("stderr = %q, want the flag refused by name", stderr)
			}
		})
	}
}

func TestInitWritesStarterConfigThatLoadsAndValidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOOPER_CONFIG", "")

	code, stdout, stderr := runCLI(t, "init")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}

	path := filepath.Join(home, ".looper", "config.toml")
	if !strings.Contains(stdout, path) {
		t.Fatalf("stdout = %q, want it to name %s", stdout, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read starter config: %v", err)
	}
	for _, placeholder := range []string{"vendor", "[[projects]]", "repoPath"} {
		if !strings.Contains(string(raw), placeholder) {
			t.Errorf("starter config has no %q placeholder", placeholder)
		}
	}

	loaded, err := config.LoadFile(config.LoadFileOptions{
		CWD:        home,
		ConfigPath: path,
		LookupEnv:  func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("starter config does not load: %v", err)
	}
	if !loaded.Metadata.ConfigFilePresent {
		t.Fatal("starter config was not seen as present")
	}
	if loaded.Config.Agent.Vendor == nil || *loaded.Config.Agent.Vendor != config.AgentVendorClaudeCode {
		t.Fatalf("agent.vendor = %v, want claude-code", loaded.Config.Agent.Vendor)
	}
}

func TestInitRefusesToOverwriteExistingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LOOPER_CONFIG", "")
	path := filepath.Join(home, ".looper", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create looper home: %v", err)
	}
	if err := os.WriteFile(path, []byte("# mine\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	code, _, stderr := runCLI(t, "init")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, path) {
		t.Fatalf("stderr = %q, want it to point at the existing file", stderr)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(raw) != "# mine\n" {
		t.Fatalf("existing config was rewritten: %q", string(raw))
	}
}

func TestInitRefusesNonTOMLConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("LOOPER_CONFIG", path)

	code, _, stderr := runCLI(t, "init")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "TOML") {
		t.Fatalf("stderr = %q, want it to explain the format limitation", stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("init created %s despite refusing", path)
	}
}

// TestInitRemovesPartiallyWrittenConfig covers the failure between O_EXCL and a
// complete file. Left behind, a truncated config is a trap with no way out from
// the CLI: init refuses to touch it because the path exists, and looperd fails
// fast trying to load it.
func TestInitRemovesPartiallyWrittenConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	err := writeNewConfigFile(path, io.MultiReader(
		strings.NewReader("[server]\n"),
		iotest.ErrReader(errors.New("no space left on device")),
	))
	if err == nil {
		t.Fatal("writeNewConfigFile() error = nil, want the read failure reported")
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Fatalf("error = %v, want the underlying cause named", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		raw, _ := os.ReadFile(path)
		t.Fatalf("a partial config survived at %s (%q); init would now refuse to repair it", path, raw)
	}
}

// TestEndpointFlagsOverrideConfiguredBaseURL guards the override the usage text
// promises. server.baseUrl is the endpoint the daemon is reachable at, so
// daemonBaseURL prefers it — but --host and --port only reach Server.Host and
// Server.Port, so without clearing it the flags are accepted, reported as
// honoured, and the request still goes to the daemon in the config file. For
// project add that means mutating the wrong catalog.
func TestEndpointFlagsOverrideConfiguredBaseURL(t *testing.T) {
	for _, name := range []string{"status", "project add"} {
		t.Run(name, func(t *testing.T) {
			wanted := newFakeDaemon(t)
			// The config names a daemon that must not be contacted: its handler
			// fails the test if it is.
			unwanted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("request reached the base-url daemon: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusTeapot)
			}))
			t.Cleanup(unwanted.Close)

			parsed, err := url.Parse(wanted.server.URL)
			if err != nil {
				t.Fatalf("parse daemon address: %v", err)
			}
			writeConfigFile(t, fmt.Sprintf("[server]\nbaseUrl = %q\n", unwanted.URL))

			args := []string{"--host", parsed.Hostname(), "--port", parsed.Port()}
			if name == "status" {
				args = append(args, "status")
			} else {
				args = append(args, "project", "add", gitRepo(t))
			}

			code, stdout, stderr := runCLI(t, args...)
			if code != 0 {
				t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
			}
			if strings.Contains(stdout, unwanted.URL) {
				t.Fatalf("stdout = %q, want the flag endpoint rather than the configured base url", stdout)
			}
		})
	}
}

func TestStatusReportsConfigDaemonAndProjects(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.projects = []map[string]any{
		{"id": "alpha", "repoPath": "/repos/alpha", "baseBranch": "main"},
		{"id": "beta", "repoPath": "/repos/beta", "baseBranch": "main", "archived": true},
	}
	path := configForDaemon(t, daemon.server.URL)

	code, stdout, stderr := runCLI(t, "status")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	for _, want := range []string{path, "(ok)", "reachable, healthy", "projects: 2", "alpha", "/repos/alpha", "beta", "(archived)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q\n%s", want, stdout)
		}
	}
}

func TestStatusReportsReviewPublishAndOrphanDebt(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.looperPath = "/opt/old-looper"
	daemon.includeReviewPublish = true
	daemon.reviewKnown = true
	daemon.reviewPublishingDisabled = true
	daemon.reviewReason = "`looper review capability` failed: exit status 1"
	daemon.quarantinedActiveExecutions = 2
	daemon.quarantinedRunningRuns = 2
	daemon.degradedReasons = []string{"review_publish_disabled", "quarantine_orphan_debt"}
	configForDaemon(t, daemon.server.URL)

	code, stdout, stderr := runCLI(t, "status")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	for _, want := range []string{
		"publishing disabled",
		"/opt/old-looper",
		"quarantinedActiveExecutions=2",
		"quarantinedRunningRuns=2",
		"review_publish_disabled",
		"quarantine_orphan_debt",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q\n%s", want, stdout)
		}
	}
}

func TestStatusReportsUnknownReviewPublishReadiness(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.looperPath = "/opt/looper"
	daemon.includeReviewPublish = true
	daemon.reviewReason = "capability has not been probed yet"
	configForDaemon(t, daemon.server.URL)

	code, stdout, stderr := runCLI(t, "status")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	for _, want := range []string{"publish readiness unknown", "capability has not been probed yet", "/opt/looper"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q\n%s", want, stdout)
		}
	}
}

func TestStatusReportsKnownIncapableReviewPublishReadiness(t *testing.T) {
	var stdout bytes.Buffer
	writeStatusOpsLines(&stdout, daemonStatusResponse{Tools: statusToolsView{
		ReviewPublish: &statusReviewPublishView{Known: true, Reason: "unsupported capability"},
	}})
	if got := stdout.String(); got != "review:   publish not ready (unsupported capability)\n" {
		t.Fatalf("status output = %q, want known-incapable readiness line", got)
	}
}

func TestStatusFailsWhenDaemonIsUnreachable(t *testing.T) {
	daemon := newFakeDaemon(t)
	address := daemon.server.URL
	daemon.server.Close()
	path := configForDaemon(t, address)

	code, stdout, stderr := runCLI(t, "status")
	if code == 0 {
		t.Fatalf("exit code = 0, want non-zero when the daemon is unreachable")
	}
	if !strings.Contains(stdout, "unreachable") {
		t.Fatalf("stdout = %q, want it to report the daemon as unreachable", stdout)
	}
	if !strings.Contains(stdout, path) {
		t.Fatalf("stdout = %q, want the config path reported even when the daemon is down", stdout)
	}
	if !strings.Contains(stderr, "not reachable") {
		t.Fatalf("stderr = %q, want an explanatory error", stderr)
	}
}

func TestStatusNamesTheConfigItCouldNotLoad(t *testing.T) {
	path := writeConfigFile(t, "[server]\nport = \"not-a-number\"\n")

	code, stdout, _ := runCLI(t, "status")
	if code == 0 {
		t.Fatal("exit code = 0, want non-zero for a config that does not load")
	}
	if !strings.Contains(stdout, path) || !strings.Contains(stdout, "FAILED") {
		t.Fatalf("stdout = %q, want the failing config path reported", stdout)
	}
	// A config that does not load is also the thing that says where the daemon
	// listens, so status must not probe a guessed address and call it the truth.
	if !strings.Contains(stdout, "daemon:   unknown") {
		t.Fatalf("stdout = %q, want the daemon reported as unknown", stdout)
	}
}

func TestStatusReportsDegradedDaemon(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.healthy = false
	configForDaemon(t, daemon.server.URL)

	code, stdout, _ := runCLI(t, "status")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for a reachable but degraded daemon", code)
	}
	if !strings.Contains(stdout, "degraded") {
		t.Fatalf("stdout = %q, want the degraded state reported", stdout)
	}
}

func TestProjectListPrintsRegisteredProjects(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.projects = []map[string]any{{
		"id": "alpha", "repoPath": "/repos/alpha", "baseBranch": "main",
		"discovery": map[string]any{"status": "failed", "error": "git worktree list failed\nretry needed"},
	}}
	configForDaemon(t, daemon.server.URL)

	code, stdout, stderr := runCLI(t, "project", "list")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if !strings.Contains(stdout, "alpha") || !strings.Contains(stdout, "/repos/alpha") {
		t.Fatalf("stdout = %q, want the project id and path", stdout)
	}
	if !strings.Contains(stdout, "discovery=failed: git worktree list failed retry needed") {
		t.Fatalf("stdout = %q, want persisted discovery failure rendered", stdout)
	}
}

func TestProjectListReportsEmptyCatalog(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)

	code, stdout, _ := runCLI(t, "project", "list")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "no projects registered") {
		t.Fatalf("stdout = %q, want an explicit empty-catalog line", stdout)
	}
}

func TestProjectAddRegistersRepositoryRootWithDaemon(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)
	repo := gitRepo(t)

	code, stdout, stderr := runCLI(t, "project", "add", repo)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if got, _ := daemon.createBody["repoPath"].(string); got != repo {
		t.Fatalf("daemon received repoPath %q, want %q", got, repo)
	}
	if !strings.Contains(stdout, "registered project repo") {
		t.Fatalf("stdout = %q, want the registered project id", stdout)
	}
	if !strings.Contains(stdout, "warning:") {
		t.Fatalf("stdout = %q, want the daemon's warnings surfaced", stdout)
	}
}

func TestProjectAddUsesClientPATHInsteadOfConfiguredDaemonGitPath(t *testing.T) {
	daemon := newFakeDaemon(t)
	writeConfigFile(t, fmt.Sprintf("[server]\nbaseUrl = %q\n[tools]\ngitPath = %q\n", daemon.server.URL, "/daemon-only/bin/git"))
	repo := gitRepo(t)

	code, _, stderr := runCLI(t, "project", "add", repo)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want client-local git from PATH", code, stderr)
	}
	if got, _ := daemon.createBody["repoPath"].(string); got != repo {
		t.Fatalf("daemon received repoPath %q, want %q", got, repo)
	}
}

func TestProjectAddResolvesRelativePathToAbsolute(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)
	repo := gitRepo(t)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("read working directory: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("enter repo: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	code, _, stderr := runCLI(t, "project", "add", ".")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	got, _ := daemon.createBody["repoPath"].(string)
	if !filepath.IsAbs(got) {
		t.Fatalf("daemon received relative repoPath %q", got)
	}
}

func TestProjectAddRejectsNonRepository(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)

	code, _, stderr := runCLI(t, "project", "add", t.TempDir())
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not a usable git repository") {
		t.Fatalf("stderr = %q, want a git-root explanation", stderr)
	}
	if daemon.createBody != nil {
		t.Fatal("a non-repository path was sent to the daemon")
	}
}

func TestProjectAddRejectsMissingPath(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)

	code, _, stderr := runCLI(t, "project", "add", filepath.Join(t.TempDir(), "absent"))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "does not exist") {
		t.Fatalf("stderr = %q, want a missing-path explanation", stderr)
	}
	if daemon.createBody != nil {
		t.Fatal("a missing path was sent to the daemon")
	}
}

func TestProjectAddRefusesAlreadyRegisteredCheckout(t *testing.T) {
	daemon := newFakeDaemon(t)
	repo := gitRepo(t)
	daemon.projects = []map[string]any{{"id": "alpha", "repoPath": repo + "/", "baseBranch": "main"}}
	configForDaemon(t, daemon.server.URL)

	code, _, stderr := runCLI(t, "project", "add", repo)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, `already registered as project "alpha"`) {
		t.Fatalf("stderr = %q, want the existing project named", stderr)
	}
	if daemon.createBody != nil {
		t.Fatal("a duplicate checkout was still sent to the daemon")
	}
}

func TestProjectAddSurfacesDaemonConflict(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.createStatus = http.StatusConflict
	daemon.createError = "project repo already exists"
	configForDaemon(t, daemon.server.URL)

	code, _, stderr := runCLI(t, "project", "add", gitRepo(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "project repo already exists") {
		t.Fatalf("stderr = %q, want the daemon's own message", stderr)
	}
}

// TestProjectAddRefusesSubdirectory pins that the root check survived the move
// from a .git stat to git itself: rev-parse answers from inside a subdirectory,
// so without the comparison a subdirectory would now register the root instead
// of being refused.
func TestProjectAddRefusesSubdirectory(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)
	repo := gitRepo(t)
	sub := filepath.Join(repo, "internal")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("create subdirectory: %v", err)
	}

	code, _, stderr := runCLI(t, "project", "add", sub)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not its root") || !strings.Contains(stderr, repo) {
		t.Fatalf("stderr = %q, want the refusal to name the real root", stderr)
	}
	if daemon.createBody != nil {
		t.Fatal("a subdirectory was sent to the daemon")
	}
}

// TestProjectAddRefusesUnusableGitMarker is the case the old stat-based check
// accepted: the name .git exists, but there is no repository behind it, and the
// daemon would only find that out after persisting the project.
func TestProjectAddRefusesUnusableGitMarker(t *testing.T) {
	for name, seed := range map[string]func(t *testing.T, root string){
		"empty .git directory": func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
				t.Fatalf("create .git: %v", err)
			}
		},
		"dangling worktree pointer": func(t *testing.T, root string) {
			pointer := []byte("gitdir: " + filepath.Join(root, "absent", ".git", "worktrees", "x") + "\n")
			if err := os.WriteFile(filepath.Join(root, ".git"), pointer, 0o600); err != nil {
				t.Fatalf("create .git file: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			daemon := newFakeDaemon(t)
			configForDaemon(t, daemon.server.URL)
			root := t.TempDir()
			seed(t, root)

			code, _, stderr := runCLI(t, "project", "add", root)
			if code != 1 {
				t.Fatalf("exit code = %d (stderr %q), want 1", code, stderr)
			}
			if !strings.Contains(stderr, "not a usable git repository") {
				t.Fatalf("stderr = %q, want git's own refusal", stderr)
			}
			if daemon.createBody != nil {
				t.Fatal("an unusable checkout was sent to the daemon")
			}
		})
	}
}

// TestProjectAddCanonicalizesAliasedPath covers the duplicate the string
// comparison misses: one checkout reached through a symlink is still one
// checkout, and two projects pointing at it would discover the same worktrees.
func TestProjectAddCanonicalizesAliasedPath(t *testing.T) {
	daemon := newFakeDaemon(t)
	repo := gitRepo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	// Older clients stored the spelling the operator supplied, so the catalog
	// can contain an alias even though new inputs are canonicalized by git.
	daemon.projects = []map[string]any{{"id": "alpha", "repoPath": alias, "baseBranch": "main"}}
	configForDaemon(t, daemon.server.URL)

	code, _, stderr := runCLI(t, "project", "add", repo)
	if code != 1 {
		t.Fatalf("exit code = %d (stderr %q), want 1", code, stderr)
	}
	if !strings.Contains(stderr, `already registered as project "alpha"`) {
		t.Fatalf("stderr = %q, want the aliased path recognised as the registered checkout", stderr)
	}
	if daemon.createBody != nil {
		t.Fatal("an aliased duplicate was sent to the daemon")
	}
}

// TestProjectAddSendsCanonicalRepoPath pins what reaches the daemon: git's
// root, not the operator's spelling of it. That is what makes the duplicate
// guard work on the next run, since the daemon stores what it is sent.
func TestProjectAddSendsCanonicalRepoPath(t *testing.T) {
	daemon := newFakeDaemon(t)
	repo := gitRepo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	configForDaemon(t, daemon.server.URL)

	code, _, stderr := runCLI(t, "project", "add", alias)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if got, _ := daemon.createBody["repoPath"].(string); got != repo {
		t.Fatalf("daemon received repoPath %q, want the canonical root %q", got, repo)
	}
}

// TestProjectAddReportsDiscoveryAsPostCommit guards the registration
// boundary: `project add` succeeds once the daemon has validated, committed,
// and published the project, and worktree/PR discovery is reported as
// pending post-commit work rather than something the CLI waits on.
func TestProjectAddReportsDiscoveryAsPostCommit(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)

	code, stdout, _ := runCLI(t, "project", "add", gitRepo(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "registered project repo") {
		t.Fatalf("stdout = %q, want registration success", stdout)
	}
	if !strings.Contains(stdout, "pending") || !strings.Contains(stdout, "looper project discover") {
		t.Fatalf("stdout = %q, want pending discovery with a discover retry hint", stdout)
	}
}

// TestProjectDiscoverRetriesDiscovery confirms the CLI can ask the daemon to
// re-run post-commit worktree/PR discovery for an existing project.
func TestProjectDiscoverRetriesDiscovery(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)

	code, stdout, _ := runCLI(t, "project", "discover", "repo")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "discovery for project repo: succeeded") {
		t.Fatalf("stdout = %q, want succeeded discovery", stdout)
	}
	if !strings.Contains(stdout, "worktrees:  2") {
		t.Fatalf("stdout = %q, want discovered worktree count", stdout)
	}
	if !strings.Contains(stdout, "pullRequests: 1") {
		t.Fatalf("stdout = %q, want discovered pull request count", stdout)
	}
}

func TestProjectDiscoverUsesExplicitLongTimeout(t *testing.T) {
	if projectDiscoveryTimeout != 10*time.Minute {
		t.Fatalf("projectDiscoveryTimeout = %s, want 10m", projectDiscoveryTimeout)
	}
	if projectDiscoveryTimeout <= requestTimeout {
		t.Fatalf("projectDiscoveryTimeout = %s, want longer than requestTimeout %s", projectDiscoveryTimeout, requestTimeout)
	}
}

func TestProjectDiscoverHonorsExplicitTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/repo/discover" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	configForDaemon(t, server.URL)
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}

	_, err = requestProjectDiscoveryWithin(context.Background(), 25*time.Millisecond, cfg, "repo")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("requestProjectDiscoveryWithin() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestProjectDiscoverHonorsCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/repo/discover" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	configForDaemon(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runProjectDiscover(ctx, nil, "repo", io.Discard)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("project discovery request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runProjectDiscover() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("project discovery did not stop after caller cancellation")
	}
}

func TestProjectDiscoverPrintsStructuredFailureBeforeReturningError(t *testing.T) {
	var stdout bytes.Buffer
	err := printProjectDiscoveryResult(createProjectResponse{
		projectResponse: projectResponse{ID: "repo"},
		Discovery: &discoveryResponse{
			Status:   "failed",
			Error:    "git worktree list failed",
			Warnings: []string{"partial discovery"},
		},
	}, &stdout)
	if err == nil || err.Error() != "git worktree list failed" {
		t.Fatalf("runProjectDiscover() error = %v, want discovery failure", err)
	}
	output := stdout.String()
	for _, want := range []string{"discovery for project repo: failed", "error:      git worktree list failed", "warning:    partial discovery"} {
		if !strings.Contains(output, want) {
			t.Fatalf("stdout = %q, want %q", output, want)
		}
	}
}

func TestProjectRejectsBadSubcommands(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{name: "missing subcommand", args: []string{"project"}},
		{name: "unknown subcommand", args: []string{"project", "remove", "x"}},
		{name: "add without path", args: []string{"project", "add"}},
		{name: "add with extra args", args: []string{"project", "add", "a", "b"}},
		{name: "list with args", args: []string{"project", "list", "x"}},
		{name: "discover without id", args: []string{"project", "discover"}},
		{name: "discover with extra args", args: []string{"project", "discover", "a", "b"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, testCase.args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, "looper project add") {
				t.Fatalf("stderr = %q, want usage text", stderr)
			}
		})
	}
}

func TestLabelsInitProvisionsRepoAndPrintsSummary(t *testing.T) {
	daemon := newFakeDaemon(t)
	configForDaemon(t, daemon.server.URL)

	code, stdout, stderr := runCLI(t, "labels", "init", "acme/looper")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if got, want := daemon.labelsInitBody["repo"], "acme/looper"; got != want {
		t.Fatalf("labels init body repo = %v, want %q", got, want)
	}
	for _, want := range []string{
		"provisioned labels for acme/looper:",
		"created=1", "skipped=1", "failed=0",
		"created: looper:worker-ready",
		"skipped: looper:plan",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, stdout)
		}
	}
}

func TestLabelsInitSurfacesDaemonFailure(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.labelsInitStatus = http.StatusBadRequest
	daemon.labelsInitError = "invalid GitHub repo: not-a-slug"
	configForDaemon(t, daemon.server.URL)

	code, _, stderr := runCLI(t, "labels", "init", "not-a-slug")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "invalid GitHub repo: not-a-slug") {
		t.Fatalf("stderr = %q, want the daemon's validation message", stderr)
	}
}

func TestLabelsInitExitsNonzeroWhenSomeLabelsFail(t *testing.T) {
	daemon := newFakeDaemon(t)
	daemon.labelsInitResult = map[string]any{
		"repo":   "acme/looper",
		"dryRun": false,
		"labels": []map[string]any{
			{"name": "looper:worker-ready", "status": "created", "color": "0052cc", "description": "Picked up automatically by worker"},
			{"name": "looper:hold:worker", "status": "failed", "color": "b60205", "description": "Block automatic worker activity", "error": "permission denied"},
		},
		"summary": map[string]any{"created": 1, "updated": 0, "skipped": 0, "failed": 1},
	}
	configForDaemon(t, daemon.server.URL)

	code, stdout, stderr := runCLI(t, "labels", "init", "acme/looper")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 when a label failed", code)
	}
	if !strings.Contains(stdout, "failed=1") || !strings.Contains(stdout, "failed: looper:hold:worker") {
		t.Fatalf("stdout = %q, want the failed label reported", stdout)
	}
	if !strings.Contains(stderr, "1 label(s) failed to provision") {
		t.Fatalf("stderr = %q, want a non-zero-exit explanation", stderr)
	}
}

func TestLabelsRejectsBadSubcommands(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{name: "missing subcommand", args: []string{"labels"}},
		{name: "unknown subcommand", args: []string{"labels", "remove", "x"}},
		{name: "init without repo", args: []string{"labels", "init"}},
		{name: "init with extra args", args: []string{"labels", "init", "a", "b"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, testCase.args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, "labels") {
				t.Fatalf("stderr = %q, want a labels usage message", stderr)
			}
		})
	}
}
