package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/version"
)

// TestRouteForVerb pins the whole CLI-to-API contract in one table: every verb
// the usage text advertises, the selector that routes somewhere else entirely
// ("stop all"), and the argument shapes that must fail before a request is
// ever sent.
func TestRouteForVerb(t *testing.T) {
	tests := []struct {
		name     string
		verb     string
		args     []string
		wantPath string
		wantBody string
		wantErr  string
	}{
		{
			name:     "stop resolves a selector against active runs",
			verb:     "stop",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/runs/active/loop-1/stop",
		},
		{
			// The daemon's bulk stop is the selector "stop-all" on a
			// single-segment path. Routing "all" as a selector with a /stop
			// suffix instead lands in the single-loop branch, which 404s on a
			// loop named "all" — so this assertion is the bug guard, not a
			// restatement of the obvious.
			name:     "stop all is the daemon's bulk selector, not a loop named all",
			verb:     "stop",
			args:     []string{"all"},
			wantPath: "/api/v1/runs/active/stop-all",
		},
		{
			// There is no bulk close on the daemon, so this stays an ordinary
			// selector and fails loudly at resolution rather than quietly
			// closing everything.
			name:     "close all stays an ordinary selector",
			verb:     "close",
			args:     []string{"all"},
			wantPath: "/api/v1/runs/active/all/close",
		},
		{
			// See TestPullRequestURLSelectorCannotRoute: the daemon splits the
			// decoded request path, so a selector with a slash cannot reach a
			// route however it is encoded.
			name:    "close refuses a pull request URL the daemon cannot resolve",
			verb:    "close",
			args:    []string{"https://github.com/o/r/pull/7"},
			wantErr: "is not a loop id or sequence number",
		},
		{
			// Escaped rather than concatenated raw: a selector is one path
			// segment, and "#" would otherwise truncate the path to a fragment.
			name:     "close escapes a selector that would break the request path",
			verb:     "close",
			args:     []string{"loop#7"},
			wantPath: "/api/v1/runs/active/loop%237/close",
		},
		{
			name:     "takeover targets the loop",
			verb:     "takeover",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/takeover",
		},
		{
			// The daemon's own human_takeover message tells the operator to run
			// `looper handback`, so the verb has to exist for that message to be
			// true.
			name:     "handback targets the loop",
			verb:     "handback",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/handback",
		},
		{
			// retry is dispatched to runRetry before routing, so it is not a
			// verb this function knows. See TestRetryReachesTheDaemonRoutes for
			// the paths it actually builds.
			name:    "retry is not routed here",
			verb:    "retry",
			args:    []string{"loop-1"},
			wantErr: `unknown command "retry"`,
		},
		{
			name:     "start targets the loop",
			verb:     "start",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/start",
		},
		{
			name:     "pause targets the loop",
			verb:     "pause",
			args:     []string{"loop-1"},
			wantPath: "/api/v1/loops/loop-1/pause",
		},
		{
			name:     "respond carries the answer as a json body",
			verb:     "respond",
			args:     []string{"loop-1", "ship it"},
			wantPath: "/api/v1/loops/loop-1/respond",
			wantBody: `{"answer":"ship it"}`,
		},
		{
			name:     "respond escapes an answer containing quotes",
			verb:     "respond",
			args:     []string{"loop-1", `he said "no"`},
			wantPath: "/api/v1/loops/loop-1/respond",
			wantBody: `{"answer":"he said \"no\""}`,
		},
		{
			name:     "respond trims the answer the daemon would trim anyway",
			verb:     "respond",
			args:     []string{"loop-1", "  yes  "},
			wantPath: "/api/v1/loops/loop-1/respond",
			wantBody: `{"answer":"yes"}`,
		},
		{
			name:    "respond rejects a whitespace-only answer before the round trip",
			verb:    "respond",
			args:    []string{"loop-1", "   "},
			wantErr: "non-empty answer",
		},
		{
			name:    "respond rejects an unquoted multi-word answer rather than truncating it",
			verb:    "respond",
			args:    []string{"loop-1", "ship", "it"},
			wantErr: "one quoted answer",
		},
		{
			name:    "respond rejects a missing answer",
			verb:    "respond",
			args:    []string{"loop-1"},
			wantErr: "one quoted answer",
		},
		{
			name:    "stop rejects a missing selector",
			verb:    "stop",
			args:    nil,
			wantErr: "stop requires exactly one selector",
		},
		{
			name:    "close names itself in its arity error",
			verb:    "close",
			args:    []string{"a", "b"},
			wantErr: "close requires exactly one selector",
		},
		{
			name:    "pause names itself in its arity error",
			verb:    "pause",
			args:    []string{},
			wantErr: "pause requires exactly one loop id",
		},
		{
			name:    "takeover rejects a whitespace-only loop id",
			verb:    "takeover",
			args:    []string{"   "},
			wantErr: "takeover requires exactly one loop id",
		},
		{
			name:    "unknown verbs are refused",
			verb:    "resume",
			args:    []string{"loop-1"},
			wantErr: `unknown command "resume"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := routeForVerb(tc.verb, tc.args)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got request %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", got.Path, tc.wantPath)
			}
			if string(got.Body) != tc.wantBody {
				t.Errorf("body = %q, want %q", string(got.Body), tc.wantBody)
			}
		})
	}
}

// TestRouteForVerbBareVerbsSendNoBody guards the split between the verbs that
// carry a payload and those that do not: handback reaches a daemon path that
// peeks at the body for discardWorktreeChanges, so a stray payload on a bare
// verb would be interpreted rather than ignored.
func TestRouteForVerbBareVerbsSendNoBody(t *testing.T) {
	for _, verb := range []string{"stop", "close", "takeover", "handback", "start", "pause"} {
		t.Run(verb, func(t *testing.T) {
			got, err := routeForVerb(verb, []string{"loop-1"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Body != nil {
				t.Errorf("body = %q, want nil", string(got.Body))
			}
		})
	}
}

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
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	daemon := &fakeDaemon{healthy: true, projects: []map[string]any{}}
	daemon.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/healthz" && r.Method == http.MethodGet:
			writeEnvelope(w, http.StatusOK, map[string]any{"healthy": daemon.healthy})
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
				"warnings":   []string{"Could not detect repository from git remote"},
			})
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

func gitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	return root
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
		"looper stop", "looper close", "looper takeover", "looper handback",
		"looper retry", "looper start", "looper pause", "looper respond",
		"looper version",
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
	daemon.projects = []map[string]any{{"id": "alpha", "repoPath": "/repos/alpha", "baseBranch": "main"}}
	configForDaemon(t, daemon.server.URL)

	code, stdout, stderr := runCLI(t, "project", "list")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if !strings.Contains(stdout, "alpha") || !strings.Contains(stdout, "/repos/alpha") {
		t.Fatalf("stdout = %q, want the project id and path", stdout)
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
	if !strings.Contains(stderr, "not a git repository root") {
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
