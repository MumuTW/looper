package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/api"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

// The CLI builds request paths by string concatenation, so a table of expected
// path strings only ever proves the CLI agrees with itself. These tests run the
// paths routeForVerb produces through the daemon's real handler, which is the
// component that decides what a path means.

const (
	contractLoopID  = "loop_contract"
	contractLoopSeq = 12
)

func TestRoutedPathsReachTheDaemonRoute(t *testing.T) {
	fixture := newContractFixture(t)

	// Both selector spellings the usage text promises: the loop's sequence
	// number and its id.
	for _, selector := range []string{"12", contractLoopID} {
		for _, verb := range []string{"stop", "close", "takeover", "handback", "start", "pause"} {
			t.Run(verb+"/"+selector, func(t *testing.T) {
				request, err := routeForVerb(verb, []string{selector})
				if err != nil {
					t.Fatalf("routeForVerb(%q, %q) error = %v", verb, selector, err)
				}
				fixture.assertRouteResolves(t, request)
			})
		}

		t.Run("respond/"+selector, func(t *testing.T) {
			request, err := routeForVerb("respond", []string{selector, "ship it"})
			if err != nil {
				t.Fatalf("routeForVerb(respond) error = %v", err)
			}
			fixture.assertRouteResolves(t, request)
		})
	}
}

// TestSequenceNumberSelectorResolvesToTheLoop pins the half of the contract the
// route-shape assertions cannot see: the daemon turns the operator's "12" into
// the loop it names. That is why the CLI forwards selectors untranslated.
func TestSequenceNumberSelectorResolvesToTheLoop(t *testing.T) {
	fixture := newContractFixture(t)

	request, err := routeForVerb("stop", []string{"12"})
	if err != nil {
		t.Fatalf("routeForVerb(stop, 12) error = %v", err)
	}
	if code, body := fixture.post(t, request); code != "" {
		t.Fatalf("POST %s failed: %s (%s)", request.Path, code, body)
	}
	if fixture.stoppedLoopID != contractLoopID {
		t.Fatalf("daemon stopped %q, want %q", fixture.stoppedLoopID, contractLoopID)
	}
}

// TestStopAllReachesTheBulkSelector guards the one verb whose path is not the
// selector pattern: "all" must land on the daemon's bulk route, not on a
// single-loop stop for a loop literally named "all".
func TestStopAllReachesTheBulkSelector(t *testing.T) {
	fixture := newContractFixture(t)

	request, err := routeForVerb("stop", []string{"all"})
	if err != nil {
		t.Fatalf("routeForVerb(stop, all) error = %v", err)
	}
	if code, body := fixture.post(t, request); code != "" {
		t.Fatalf("POST %s failed: %s (%s)", request.Path, code, body)
	}
	if !fixture.stoppedAll {
		t.Fatal("stop all did not reach the daemon's bulk stop")
	}
	if fixture.stoppedLoopID != "" {
		t.Fatalf("stop all also stopped loop %q", fixture.stoppedLoopID)
	}
}

// TestPullRequestURLSelectorCannotRoute is the reason selectorPathSegment
// refuses a slash rather than escaping it. The usage text used to promise that
// a pull request URL worked as a selector; it never could, and percent-encoding
// does not rescue it, because the handler routes on the decoded path.
func TestPullRequestURLSelectorCannotRoute(t *testing.T) {
	fixture := newContractFixture(t)
	const prURL = "https://github.com/acme/looper/pull/42"

	if _, err := routeForVerb("stop", []string{prURL}); err == nil {
		t.Fatal("routeForVerb accepted a pull request URL selector; the daemon cannot resolve one")
	}

	// Prove the refusal is about the daemon and not a taste judgement: both the
	// raw concatenation the CLI used to emit and its percent-encoded form miss
	// the route entirely.
	for name, path := range map[string]string{
		"raw":     "/api/v1/runs/active/" + prURL + "/stop",
		"escaped": "/api/v1/runs/active/https%3A%2F%2Fgithub.com%2Facme%2Flooper%2Fpull%2F42/stop",
	} {
		t.Run(name, func(t *testing.T) {
			code, _ := fixture.post(t, apiRequest{Path: path})
			if code != pkgapi.ErrorCodeRouteNotFound {
				t.Fatalf("error code = %q, want %q", code, pkgapi.ErrorCodeRouteNotFound)
			}
		})
	}
}

// contractFixture is a real runtime plus the daemon's HTTP handler, with the
// control hooks recorded so a test can tell "the route matched" from "the
// selector resolved to the loop I meant".
type contractFixture struct {
	handler       http.Handler
	config        config.Config
	stoppedLoopID string
	stoppedAll    bool
}

// assertRouteResolves checks the two failures a routing bug produces: the path
// matched no route at all, or it matched but carried a selector the daemon
// could not resolve. Business-rule rejections (responding to a loop that is not
// awaiting a human) are left alone — reaching one proves the request arrived at
// the right handler with a resolved loop.
func (f *contractFixture) assertRouteResolves(t *testing.T, request apiRequest) {
	t.Helper()
	code, body := f.post(t, request)
	switch code {
	case pkgapi.ErrorCodeRouteNotFound, pkgapi.ErrorCodeLoopNotFound:
		t.Fatalf("POST %s did not reach its route: %s (%s)", request.Path, code, body)
	}
}

// post issues the CLI's request against the real handler and returns the
// envelope's error code (empty when the call succeeded).
func (f *contractFixture) post(t *testing.T, request apiRequest) (pkgapi.ErrorCode, string) {
	t.Helper()

	var body io.Reader
	if request.Body != nil {
		body = bytes.NewReader(request.Body)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, request.Path, body)
	httpRequest.Header.Set("Content-Type", "application/json")
	// Host is what the CLI would actually send, so the daemon's Host guard is
	// part of the contract under test rather than something the test waves away.
	httpRequest.Host = strings.TrimPrefix(daemonBaseURL(f.config), "http://")

	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httpRequest)

	var envelope struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code    pkgapi.ErrorCode `json:"code"`
			Message string           `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response for %s: %v (body %s)", request.Path, err, recorder.Body.String())
	}
	if envelope.Error == nil {
		return "", recorder.Body.String()
	}
	return envelope.Error.Code, envelope.Error.Message
}

func newContractFixture(t *testing.T) *contractFixture {
	t.Helper()

	rootDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	cfg, err := config.DefaultConfig(rootDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Network = config.NetworkConfig{}
	cfg.Storage.DBPath = filepath.Join(rootDir, "state", "looper.sqlite")
	cfg.Daemon.LogDir = filepath.Join(rootDir, "logs")
	cfg.Daemon.WorkingDirectory = rootDir

	runtime := looperdruntime.New(looperdruntime.Options{
		Config:           cfg,
		Logger:           contractLogger{},
		RunSchedulerTick: func(context.Context, looperdruntime.Services) error { return nil },
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	if err := runtime.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("Runtime.CompleteStartup() error = %v", err)
	}
	t.Cleanup(func() { runtime.Stop("test cleanup") })

	seedContractLoop(t, runtime)

	fixture := &contractFixture{config: cfg}
	fixture.handler = api.NewHandler(api.Context{
		Config:  cfg,
		Runtime: runtime,
		StopLoop: func(_ context.Context, loopID string, _ string) (any, error) {
			fixture.stoppedLoopID = loopID
			return map[string]any{"stopped": true}, nil
		},
		CloseLoop: func(_ context.Context, loopID string, _ string) (any, error) {
			return map[string]any{"closed": loopID}, nil
		},
		StopAll: func(context.Context, string) (any, error) {
			fixture.stoppedAll = true
			return map[string]any{"stopped": 0}, nil
		},
		TakeoverLoop: func(_ context.Context, loopID string, _ string) (api.TakeoverResult, error) {
			return api.TakeoverResult{LoopID: loopID}, nil
		},
	})
	return fixture
}

func seedContractLoop(t *testing.T, runtime *looperdruntime.Runtime) {
	t.Helper()

	services := runtime.Services()
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000Z")
	projectID := "project_contract"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: contractLoopID, Seq: contractLoopSeq, ProjectID: projectID, Type: "worker",
		TargetType: "project", TargetID: &targetID, Status: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
}

type contractLogger struct{}

func (contractLogger) Debug(string, map[string]any) {}
func (contractLogger) Info(string, map[string]any)  {}
func (contractLogger) Warn(string, map[string]any)  {}
func (contractLogger) Error(string, map[string]any) {}

var _ bootstrap.Logger = contractLogger{}

// TestRetryReachesTheDaemonRoutes covers the one verb that never reaches
// routeForVerb. runRetry builds its own two paths — a GET preflight on
// /worktree and the POST on /retry — so nothing in the routing table proves
// either of them resolves. retry_test.go drives them against hand-written
// http.HandlerFunc stubs, which can only confirm the CLI agrees with the test's
// own idea of the route; this drives the real daemon handler instead.
func TestRetryReachesTheDaemonRoutes(t *testing.T) {
	fixture := newContractFixture(t)

	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := httptest.NewRecorder()
		fixture.handler.ServeHTTP(recorder, r)

		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = recorder.Code
		mu.Unlock()

		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	defer server.Close()

	configPath := writeContractConfig(t, server.URL)
	t.Setenv("HOME", t.TempDir())

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	runRetry(context.Background(), []string{"retry", "12", "--config", configPath}, stdout, stderr)

	mu.Lock()
	defer mu.Unlock()

	// The preflight must resolve. A 404 here is the failure the dashboard
	// tolerates on older daemons, so a broken path would silently degrade into
	// "no preflight" rather than erroring — which is the whole risk this guards.
	worktree, ok := seen["GET /api/v1/loops/12/worktree"]
	if !ok {
		t.Fatalf("retry never requested the worktree preflight; saw %v", seen)
	}
	if worktree == http.StatusNotFound {
		t.Fatalf("GET /api/v1/loops/12/worktree returned 404: the preflight path does not exist on the daemon")
	}

	if status, ok := seen["POST /api/v1/loops/12/retry"]; ok && status == http.StatusNotFound {
		t.Fatalf("POST /api/v1/loops/12/retry returned 404 (stderr %q)", stderr.String())
	}
	if strings.Contains(stderr.String(), "Unknown route") {
		t.Fatalf("retry hit an unknown route: %s", stderr.String())
	}
}

// writeContractConfig points the CLI's own config loading at a running server,
// so the request goes through loadConfig and daemonBaseURL rather than around
// them.
func writeContractConfig(t *testing.T, baseURL string) string {
	t.Helper()

	dir := t.TempDir()
	cfg, err := config.DefaultConfig(dir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Server.BaseURL = &baseURL
	cfg.Network = config.NetworkConfig{}
	cfg.Storage.DBPath = filepath.Join(dir, "state", "looper.sqlite")

	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
