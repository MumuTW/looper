package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/projects"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

// contractRegenerateCommand is quoted verbatim by the CI staleness check and by
// the artifacts' own provenance headers; keep the three in sync.
const contractRegenerateCommand = "go generate ./internal/api/..."

var regenerateContracts = flag.Bool(
	"contracts.regenerate",
	false,
	"rewrite internal/api/testdata/contracts from live handler responses",
)

// TestRegenerateContractArtifacts replays the frozen request shapes against the
// in-process handler and rewrites the response and error compat artifacts from
// what the handler actually returns. It is a no-op without -contracts.regenerate
// so a normal `go test ./...` never rewrites checked-in fixtures.
//
// The request artifact (daemon-http.requests.compat.json) and the route/auth map
// (daemon-http.compat.json) stay hand-maintained: the former is the input this
// generator replays, and the latter describes behavior rather than payloads.
func TestRegenerateContractArtifacts(t *testing.T) {
	if !*regenerateContracts {
		t.Skipf("pass -contracts.regenerate, or run: %s", contractRegenerateCommand)
	}

	responses := captureResponseArtifactRoutes(t)
	errorCases := captureErrorArtifactCases(t)
	if t.Failed() {
		return
	}

	writeContractArtifact(t, "daemon-http.responses.compat.json", buildResponsesArtifact(t, responses))
	writeContractArtifact(t, "daemon-http.errors.compat.json", buildErrorsArtifact(t, errorCases))
}

type capturedResponse struct {
	status int
	body   any
}

type responseArtifactEntry struct {
	id string
	// declared holds an entry that cannot be captured deterministically and is
	// therefore authored here instead.
	declared string
	// placeholders masks runtime-generated leaves by JSON pointer.
	placeholders map[string]string
}

// responseArtifactOrder fixes the artifact's route order so regeneration never
// reshuffles the file.
var responseArtifactOrder = []responseArtifactEntry{
	{id: "healthz.get", placeholders: map[string]string{
		"/data/storage/lastUpdatedAt": "<generated-timestamp>",
	}},
	{id: "status.get", placeholders: map[string]string{
		"/data/service/binary/path":          "<daemon-executable-path>",
		"/data/service/binary/currentTarget": "<current-target>",
		"/data/service/binary/artifactName":  "<artifact-name>",
	}},
	{id: "config.get"},
	{id: "config.patch"},
	{id: "events.list"},
	{id: "events.entity"},
	{id: "pullRequests.list"},
	{id: "pullRequests.detail"},
	{id: "pullRequests.status"},
	{id: "loops.list"},
	{id: "loops.create", placeholders: map[string]string{
		"/data/id":        "<uuid>",
		"/data/nextRunAt": "<generated-timestamp>",
		"/data/createdAt": "<generated-timestamp>",
		"/data/updatedAt": "<generated-timestamp>",
	}},
	{id: "loop.detail"},
	{id: "loop.logs"},
	{id: "loop.logs.follow", declared: loopLogsFollowDeclaredEntry},
	{id: "loop.start", placeholders: map[string]string{
		"/data/nextRunAt": "<generated-timestamp>",
		"/data/updatedAt": "<generated-timestamp>",
	}},
	{id: "loop.pause", placeholders: map[string]string{
		"/data/updatedAt": "<generated-timestamp>",
	}},
	{id: "loop.retry", placeholders: map[string]string{
		"/data/loop/nextRunAt": "<generated-timestamp>",
		"/data/loop/updatedAt": "<generated-timestamp>",
		"/data/queueItemId":    "<uuid>",
	}},
	{id: "loop.takeover"},
	{id: "loop.handback", placeholders: map[string]string{
		"/data/loop/nextRunAt": "<generated-timestamp>",
		"/data/loop/updatedAt": "<generated-timestamp>",
		"/data/queueItemId":    "<uuid>",
	}},
	{id: "workers.create", placeholders: map[string]string{
		"/data/id":        "<uuid>",
		"/data/createdAt": "<generated-timestamp>",
		"/data/updatedAt": "<generated-timestamp>",
	}},
	{id: "planners.create", placeholders: map[string]string{
		"/data/id":        "<uuid>",
		"/data/nextRunAt": "<generated-timestamp>",
		"/data/createdAt": "<generated-timestamp>",
		"/data/updatedAt": "<generated-timestamp>",
	}},
	{id: "projects.list"},
	{id: "projects.create"},
	{id: "projects.update"},
	{id: "runs.list"},
	{id: "run.logs"},
	{id: "runs.active.list", placeholders: map[string]string{
		"/data/items/1/loopId":    "<uuid>",
		"/data/items/1/startedAt": "<generated-timestamp>",
	}},
	{id: "runs.active.detail"},
	{id: "runs.active.stop"},
	{id: "dashboard.bootstrap.code", placeholders: map[string]string{
		"/data/code":      "<bootstrap-code>",
		"/data/expiresAt": "<generated-timestamp>",
	}},
	{id: "dashboard.bootstrap.exchange", placeholders: map[string]string{
		"/data/token": "<local-token>",
	}},
}

// loop.logs.follow is an SSE stream whose chunk and end events depend on writes
// racing the poll loop, so it cannot be captured deterministically. Its wire
// format is documented here instead of replayed.
const loopLogsFollowDeclaredEntry = `{
  "id": "loop.logs.follow",
  "status": 200,
  "contentType": "text/event-stream",
  "notes": [
    "GET /api/v1/loops/{id}/logs?follow=1 returns text/event-stream (not a JSON envelope).",
    "Wire format: event: <name>\\ndata: <json>\\n\\n",
    "Event order: snapshot (once), zero or more chunk, optional end then close.",
    "snapshot data shape matches loop.logs body.data.",
    "chunk data shape: { content: string, runId?, currentStep?, executionId?, vendor?, pid?, status? }.",
    "end data shape: { reason?: string } (e.g. run_completed)."
  ],
  "events": {
    "snapshot": {
      "event": "snapshot",
      "data": {
        "seq": 1,
        "loopId": "loop_1",
        "loopType": "reviewer",
        "loopStatus": "running",
        "run": {
          "runId": "run_1",
          "status": "running",
          "currentStep": "review",
          "startedAt": "2026-04-11T12:00:00.000Z",
          "endedAt": null,
          "summary": null,
          "errorMessage": null
        },
        "agent": null
      }
    },
    "chunk": {
      "event": "chunk",
      "data": {
        "content": "appended log text",
        "runId": "run_1",
        "currentStep": "review",
        "executionId": "exec_1",
        "vendor": "opencode",
        "pid": 12345,
        "status": "running"
      }
    },
    "end": {
      "event": "end",
      "data": {
        "reason": "run_completed"
      }
    }
  }
}`

const contractProvenance = `{
  "generatedFrom": [
    "internal/api/handler.go",
    "internal/api/bootstrap_routes.go",
    "internal/api/loop_logs_stream.go",
    "internal/api/loop_retry_discard.go",
    "internal/api/request_decode.go",
    "pkg/api/envelope.go",
    "internal/api/contract_artifact_regen_test.go"
  ],
  "generatedBy": "go generate ./internal/api/..."
}`

const responsesArtifactNotes = `[
  "This artifact freezes the current /api/v1 success-envelope JSON shapes captured by replaying the frozen request fixtures against the in-process Go handler.",
  "Generated, not hand-edited: change the handler, then run go generate ./internal/api/... in the same commit.",
  "Values normalized as placeholders are environment- or runtime-generated but the surrounding JSON shape is part of the compatibility boundary.",
  "Every route carries its own captured body: two routes that happen to agree today are still two independent boundaries.",
  "loop.logs.follow documents the text/event-stream wire format; its samples are declared in internal/api/contract_artifact_regen_test.go because the stream is timing-dependent."
]`

const responsesArtifactPlaceholders = `{
  "<tmp-root>": "Temporary fixture root directory used by the test harness.",
  "<home>": "Current user home directory.",
  "<uuid>": "Runtime-generated UUID.",
  "<generated-timestamp>": "Runtime-generated ISO-8601 timestamp.",
  "<current-target>": "Current looperd build target for the running platform.",
  "<artifact-name>": "Compiled looperd artifact name for the current target.",
  "<daemon-executable-path>": "Resolved executable path of the running looperd process.",
  "<bootstrap-code>": "Runtime-generated one-shot dashboard bootstrap code.",
  "<local-token>": "Configured server.localToken returned by a successful bootstrap exchange."
}`

const errorsArtifactNotes = `[
  "This artifact freezes the current /api/v1 error envelope and error-code compatibility boundary.",
  "Generated, not hand-edited: change the handler, then run go generate ./internal/api/... in the same commit.",
  "Each case replays the recorded request against the in-process Go handler and captures the exact status plus JSON body.",
  "When requestId is omitted from the request, the daemon generates one; the fixture normalizes generated UUID values as <uuid>.",
  "error.details is omitted from these envelopes because the current daemon does not include details for these failures.",
  "errorCodes is the complete set of daemon error codes and their HTTP statuses, read from pkg/api.AllErrorCodes; exact message variants remain frozen by the per-case fixtures."
]`

const errorsArtifactPlaceholders = `{
  "<uuid>": "Runtime-generated UUID for requestId when x-request-id is not supplied."
}`

const errorsArtifactSharedBehavior = `{
  "responseHeaders": {
    "content-type": "application/json; charset=utf-8"
  },
  "requestIdHeader": {
    "name": "x-request-id"
  },
  "envelope": {
    "ok": false,
    "requestId": "always present on error responses",
    "error": {
      "requiredFields": [
        "code",
        "message"
      ],
      "details": "omitted when undefined"
    }
  }
}`

// contractConfigFixture reproduces, for the contract fixtures, the two halves
// of a config PATCH that production wires separately: PatchConfig hands the
// mutation to the configuration authority, and the handler then republishes the
// applied configuration through ConfigSnapshot before building the response. A
// stub that only returns nil leaves the PATCH response equal to the preceding
// GET and freezes that false equality into the artifact.
type contractConfigFixture struct {
	config config.Config
}

// newContractConfigFixture seeds from the runtime's own config because that is
// what the handler reads for /api/v1/config when no ConfigSnapshot is wired, so
// GET responses stay identical once one is.
func newContractConfigFixture(rt interface{ Config() config.Config }) *contractConfigFixture {
	return &contractConfigFixture{config: rt.Config()}
}

// patch applies the request through config.ApplyFieldPatch, the same canonical
// dotted-path applier the daemon's file-layer patch uses, so an invalid path or
// value fails here exactly as it would in production.
func (f *contractConfigFixture) patch(_ context.Context, request ConfigPatchRequest) error {
	partial, err := config.ApplyFieldPatch(config.PartialConfig{}, request.Set, request.Unset)
	if err != nil {
		return ConfigRequestError{
			Kind:    ConfigRequestErrorKindValidation,
			Message: "Invalid configuration patch",
			Issues:  []ConfigPatchIssue{{Code: "invalid_patch", Message: err.Error()}},
		}
	}

	// The patched partial carries only the fields the request touched, and its
	// JSON names match config.Config's, so decoding it over a copy of the
	// current config overlays exactly those fields.
	encoded, err := json.Marshal(partial)
	if err != nil {
		return err
	}
	applied := f.config
	if err := json.Unmarshal(encoded, &applied); err != nil {
		return err
	}
	f.config = applied

	return nil
}

func (f *contractConfigFixture) snapshot() (config.Config, ConfigMetadata) {
	return f.config, ConfigMetadata{}
}

func buildResponsesArtifact(t *testing.T, captured map[string]capturedResponse) *jsonObject {
	t.Helper()

	routes := make([]any, 0, len(responseArtifactOrder))

	for _, entry := range responseArtifactOrder {
		if entry.declared != "" {
			routes = append(routes, declaredContractObject(t, entry.id, entry.declared))
			continue
		}

		response, ok := captured[entry.id]
		if !ok {
			t.Fatalf("no captured response for route %q", entry.id)
		}
		for pointer, placeholder := range entry.placeholders {
			setJSONPointer(t, response.body, pointer, placeholder)
		}

		routes = append(routes, newJSONObject().
			set("id", entry.id).
			set("status", json.Number(strconv.Itoa(response.status))).
			set("body", response.body))
	}

	artifact := newJSONObject().set("artifactVersion", json.Number("1"))
	applyContractProvenance(t, artifact)

	return artifact.
		set("notes", declaredContractJSON(t, "responses.notes", responsesArtifactNotes)).
		set("placeholders", declaredContractJSON(t, "responses.placeholders", responsesArtifactPlaceholders)).
		set("routes", routes)
}

func buildErrorsArtifact(t *testing.T, cases []any) *jsonObject {
	t.Helper()

	codes := pkgapi.AllErrorCodes()
	sorted := make([]pkgapi.ErrorCode, len(codes))
	copy(sorted, codes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })

	errorCodes := make([]any, 0, len(sorted))
	for _, code := range sorted {
		errorCodes = append(errorCodes, newJSONObject().
			set("code", code.String()).
			set("status", json.Number(strconv.Itoa(code.Status()))))
	}

	artifact := newJSONObject().set("artifactVersion", json.Number("2"))
	applyContractProvenance(t, artifact)

	return artifact.
		set("notes", declaredContractJSON(t, "errors.notes", errorsArtifactNotes)).
		set("placeholders", declaredContractJSON(t, "errors.placeholders", errorsArtifactPlaceholders)).
		set("sharedBehavior", declaredContractJSON(t, "errors.sharedBehavior", errorsArtifactSharedBehavior)).
		set("errorCodes", errorCodes).
		set("cases", cases)
}

func applyContractProvenance(t *testing.T, artifact *jsonObject) {
	t.Helper()

	provenance := declaredContractObject(t, "provenance", contractProvenance)
	for _, key := range provenance.keys {
		artifact.set(key, provenance.values[key])
	}
}

func writeContractArtifact(t *testing.T, name string, artifact *jsonObject) {
	t.Helper()

	encoded, err := encodeOrderedJSON(artifact)
	if err != nil {
		t.Fatalf("encodeOrderedJSON(%s) error = %v", name, err)
	}

	path := filepath.Join("testdata", "contracts", name)
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// captureResponse serves req and decodes the response with the fixture's
// environment-specific paths swapped for their placeholder tokens, mirroring
// normalizeResponseValue in handler_test.go.
func captureResponse(t *testing.T, h *Handler, rootDir string, req *http.Request) capturedResponse {
	t.Helper()

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)

	raw := bytes.ReplaceAll(recorder.Body.Bytes(), []byte(rootDir), []byte("<tmp-root>"))
	if homeDir, err := os.UserHomeDir(); err == nil && strings.TrimSpace(homeDir) != "" {
		raw = bytes.ReplaceAll(raw, []byte(homeDir), []byte("<home>"))
	}

	return capturedResponse{status: recorder.Code, body: decodeContractJSON(t, req.URL.Path, raw)}
}

func captureSuccess(t *testing.T, h *Handler, rootDir string, req *http.Request) capturedResponse {
	t.Helper()

	response := captureResponse(t, h, rootDir, req)
	if response.status != http.StatusOK {
		encoded, _ := encodeOrderedJSON(response.body)
		t.Fatalf("%s %s status = %d, want 200\nbody=%s", req.Method, req.URL, response.status, encoded)
	}

	return response
}

func contractRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	var req *http.Request
	if reader == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, reader)
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("x-request-id", "fixture-request-id")

	return req
}

func captureResponseArtifactRoutes(t *testing.T) map[string]capturedResponse {
	t.Helper()

	captured := make(map[string]capturedResponse)
	groups := []struct {
		name string
		run  func(*testing.T, map[string]capturedResponse)
	}{
		{"core", captureCoreRouteResponses},
		{"events-pull-requests", captureEventAndPullRequestResponses},
		{"loops", captureLoopRouteResponses},
		{"workers-planners", captureWorkerPlannerResponses},
		{"projects", captureProjectListResponse},
		{"runs", captureRunRouteResponses},
		{"dashboard-bootstrap", captureBootstrapResponses},
	}
	for _, group := range groups {
		t.Run(group.name, func(t *testing.T) { group.run(t, captured) })
	}

	return captured
}

func captureCoreRouteResponses(t *testing.T, out map[string]capturedResponse) {
	t.Helper()

	fixture := newTestFixture(t)
	seedStatusData(t, fixture.runtime)
	requestRoutes := loadRequestArtifact(t)
	configFixture := newContractConfigFixture(fixture.runtime)

	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now },
		ProjectsService: fakeProjectService{
			addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
				nowISO := fixture.now.UTC().Format(javaScriptISOString)
				baseBranch := "main"
				metadataJSON := `{"repo":"MumuTW/looper","worktreeRoot":null,"source":"api"}`
				return projects.AddResult{
					Project: storage.ProjectRecord{
						ID:           "looper",
						Name:         "Looper",
						RepoPath:     "/tmp/repos/looper",
						BaseBranch:   &baseBranch,
						MetadataJSON: &metadataJSON,
						CreatedAt:    nowISO,
						UpdatedAt:    nowISO,
					},
					Discovery: projects.DiscoveryState{
						Status:       projects.DiscoveryStatusPending,
						SnapshotMode: projects.SnapshotModeAsync,
						UpdatedAt:    nowISO,
					},
					Warnings: []string{},
				}, nil
			},
			updateProject: func(context.Context, string, projects.UpdateInput) (storage.ProjectRecord, error) {
				nowISO := fixture.now.UTC().Format(javaScriptISOString)
				baseBranch := "main"
				metadataJSON := `{"repo":"nexu-io/looper","worktreeRoot":null,"source":"api"}`
				return storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch, MetadataJSON: &metadataJSON, CreatedAt: nowISO, UpdatedAt: nowISO}, nil
			},
		},
		PatchConfig:     configFixture.patch,
		ConfigSnapshot:  configFixture.snapshot,
		RecoverySummary: func() any { return map[string]any{"expiredLocksReleased": 1} },
	})

	routes := []struct {
		id     string
		method string
		path   string
		body   string
	}{
		{id: "healthz.get", method: http.MethodGet, path: "/api/v1/healthz"},
		{id: "status.get", method: http.MethodGet, path: "/api/v1/status"},
		{id: "config.get", method: http.MethodGet, path: "/api/v1/config"},
		{id: "config.patch", method: http.MethodPatch, path: "/api/v1/config", body: marshalArtifactRequestBody(t, requestRoutes, "config.patch")},
		{id: "projects.create", method: http.MethodPost, path: "/api/v1/projects", body: marshalArtifactRequestBody(t, requestRoutes, "projects.create")},
		{id: "projects.update", method: http.MethodPatch, path: "/api/v1/projects/project_1", body: marshalArtifactRequestBody(t, requestRoutes, "projects.update")},
	}
	for _, route := range routes {
		req := contractRequest(t, route.method, route.path, route.body)
		if route.id == "config.patch" {
			req.RemoteAddr = "127.0.0.1:17310"
		}
		out[route.id] = captureSuccess(t, h, fixture.rootDir, req)
	}
}

func captureEventAndPullRequestResponses(t *testing.T, out map[string]capturedResponse) {
	t.Helper()

	fixture := newTestFixture(t)
	seedEventAndPullRequestRouteData(t, fixture.runtime)

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }})

	routes := []struct {
		id   string
		path string
	}{
		{id: "events.list", path: "/api/v1/events?limit=1"},
		{id: "events.entity", path: "/api/v1/events/loop/loop_1"},
		{id: "pullRequests.list", path: "/api/v1/pull-requests"},
		{id: "pullRequests.detail", path: "/api/v1/pull-requests/acme%2Flooper/42"},
		{id: "pullRequests.status", path: "/api/v1/pull-requests/acme%2Flooper/42/status"},
	}
	for _, route := range routes {
		out[route.id] = captureSuccess(t, h, fixture.rootDir, contractRequest(t, http.MethodGet, route.path, ""))
	}
}

func captureLoopRouteResponses(t *testing.T, out map[string]capturedResponse) {
	t.Helper()

	requestArtifact := loadRequestArtifact(t)
	routes := []struct {
		id      string
		method  string
		path    string
		body    string
		prepare func(*testing.T, *Handler, *looperdruntime.Runtime)
	}{
		{id: "loops.list", method: http.MethodGet, path: "/api/v1/loops"},
		{id: "loop.detail", method: http.MethodGet, path: "/api/v1/loops/loop_1"},
		{id: "loop.logs", method: http.MethodGet, path: "/api/v1/loops/loop_1/logs"},
		{id: "loop.start", method: http.MethodPost, path: "/api/v1/loops/loop_1/start"},
		{id: "loop.pause", method: http.MethodPost, path: "/api/v1/loops/loop_1/pause", prepare: func(t *testing.T, h *Handler, _ *looperdruntime.Runtime) {
			t.Helper()
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/loops/loop_1/start", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("pre-start status = %d, want 200", recorder.Code)
			}
		}},
		{id: "loop.retry", method: http.MethodPost, path: "/api/v1/loops/loop_1/retry", body: marshalArtifactRequestBody(t, requestArtifact, "loop.retry"), prepare: func(t *testing.T, _ *Handler, rt *looperdruntime.Runtime) {
			t.Helper()
			prepareLoopRouteForRetry(t, rt, "paused")
		}},
		{id: "loop.takeover", method: http.MethodPost, path: "/api/v1/loops/loop_1/takeover"},
		{id: "loop.handback", method: http.MethodPost, path: "/api/v1/loops/loop_1/handback", body: marshalArtifactRequestBody(t, requestArtifact, "loop.handback"), prepare: func(t *testing.T, _ *Handler, rt *looperdruntime.Runtime) {
			t.Helper()
			prepareLoopRouteForRetry(t, rt, "human_takeover")
		}},
		{id: "loops.create", method: http.MethodPost, path: "/api/v1/loops", body: marshalArtifactRequestBody(t, requestArtifact, "loops.create")},
	}

	for _, route := range routes {
		t.Run(route.id, func(t *testing.T) {
			fixture := newTestFixture(t)
			seedLoopRouteData(t, fixture.runtime)
			h := NewHandler(Context{
				Config:  fixture.config,
				Runtime: fixture.runtime,
				Now:     func() time.Time { return fixture.now.Add(time.Minute) },
				TakeoverLoop: func(_ context.Context, loopID, _ string) (TakeoverResult, error) {
					return TakeoverResult{
						LoopID:       loopID,
						Vendor:       "codex",
						SessionID:    "session_fixture_1",
						WorktreePath: "/tmp/worktrees/loop_1",
					}, nil
				},
			})
			if route.prepare != nil {
				route.prepare(t, h, fixture.runtime)
			}
			out[route.id] = captureSuccess(t, h, fixture.rootDir, contractRequest(t, route.method, route.path, route.body))
		})
	}
}

func captureWorkerPlannerResponses(t *testing.T, out map[string]capturedResponse) {
	t.Helper()

	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	requestArtifact := loadRequestArtifact(t)

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})

	// workers.create resolves an existing planner loop for the same target, so
	// the artifact shape depends on this bootstrap loop already existing.
	bootstrap := httptest.NewRequest(http.MethodPost, "/api/v1/loops", strings.NewReader(marshalArtifactRequestBody(t, requestArtifact, "loops.create")))
	bootstrap.Header.Set("content-type", "application/json")
	bootstrapRecorder := httptest.NewRecorder()
	h.ServeHTTP(bootstrapRecorder, bootstrap)
	if bootstrapRecorder.Code != http.StatusOK {
		t.Fatalf("bootstrap loops.create status = %d, want 200", bootstrapRecorder.Code)
	}

	for _, route := range []struct{ id, path string }{
		{id: "workers.create", path: "/api/v1/workers"},
		{id: "planners.create", path: "/api/v1/planners"},
	} {
		req := contractRequest(t, http.MethodPost, route.path, marshalArtifactRequestBody(t, requestArtifact, route.id))
		out[route.id] = captureSuccess(t, h, fixture.rootDir, req)
	}
}

func captureProjectListResponse(t *testing.T, out map[string]capturedResponse) {
	t.Helper()

	fixture := newTestFixture(t)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	metadata := `{"repo":"acme/looper","worktreeRoot":null,"source":"api"}`
	baseBranch := "main"
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     "/tmp/looper",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert(project_1) error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	out["projects.list"] = captureSuccess(t, h, fixture.rootDir, contractRequest(t, http.MethodGet, "/api/v1/projects", ""))
}

func captureRunRouteResponses(t *testing.T, out map[string]capturedResponse) {
	t.Helper()

	routes := []struct {
		id     string
		method string
		path   string
		setup  func(testFixture) Context
	}{
		{id: "runs.list", method: http.MethodGet, path: "/api/v1/runs?loopId=loop_1"},
		{id: "run.logs", method: http.MethodGet, path: "/api/v1/runs/run_1/logs"},
		{id: "runs.active.list", method: http.MethodGet, path: "/api/v1/runs/active"},
		{id: "runs.active.detail", method: http.MethodGet, path: "/api/v1/runs/active/1"},
		{id: "runs.active.stop", method: http.MethodPost, path: "/api/v1/runs/active/1/stop", setup: func(fixture testFixture) Context {
			return Context{
				Config:  fixture.config,
				Runtime: fixture.runtime,
				Now:     func() time.Time { return fixture.now },
				StopLoop: func(_ context.Context, loopID, _ string) (any, error) {
					return stopLoopResponse{Stopped: true, LoopID: loopID}, nil
				},
			}
		}},
	}

	for _, route := range routes {
		t.Run(route.id, func(t *testing.T) {
			fixture := newTestFixture(t)
			seedRunRouteData(t, fixture.runtime)
			handlerContext := Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}
			if route.setup != nil {
				handlerContext = route.setup(fixture)
			}
			h := NewHandler(handlerContext)
			out[route.id] = captureSuccess(t, h, fixture.rootDir, contractRequest(t, route.method, route.path, ""))
		})
	}
}

func captureBootstrapResponses(t *testing.T, out map[string]capturedResponse) {
	t.Helper()

	fixture := newTestFixture(t)
	token := "local-token-fixture"
	cfg := fixture.config
	cfg.Server.AuthMode = config.AuthModeLocalToken
	cfg.Server.LocalToken = &token

	h := NewHandler(Context{
		Config:  cfg,
		Runtime: runtimeWithConfig(fixture.runtime, cfg),
		Now:     func() time.Time { return fixture.now },
	})

	mint := contractRequest(t, http.MethodPost, "/api/v1/dashboard/bootstrap/code", "")
	mint.Header.Set("Authorization", "Bearer "+token)
	minted := captureSuccess(t, h, fixture.rootDir, mint)
	out["dashboard.bootstrap.code"] = minted

	code := bootstrapCodeFromResponse(t, minted)
	exchangeBody, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatalf("json.Marshal(exchange body) error = %v", err)
	}
	out["dashboard.bootstrap.exchange"] = captureSuccess(t, h, fixture.rootDir,
		contractRequest(t, http.MethodPost, "/api/v1/dashboard/bootstrap/exchange", string(exchangeBody)))
}

func bootstrapCodeFromResponse(t *testing.T, response capturedResponse) string {
	t.Helper()

	envelope, ok := response.body.(*jsonObject)
	if !ok {
		t.Fatal("bootstrap mint response is not a JSON object")
	}
	data, ok := envelope.get("data")
	if !ok {
		t.Fatal("bootstrap mint response has no data")
	}
	dataObject, ok := data.(*jsonObject)
	if !ok {
		t.Fatal("bootstrap mint data is not a JSON object")
	}
	code, ok := dataObject.get("code")
	if !ok {
		t.Fatal("bootstrap mint data has no code")
	}
	value, ok := code.(string)
	if !ok || strings.TrimSpace(value) == "" {
		t.Fatalf("bootstrap code = %#v, want non-empty string", code)
	}

	return value
}

type errorContractCase struct {
	id      string
	fixture string
	method  string
	path    string
	// request is the recorded request block: it is emitted verbatim into the
	// artifact and is also what gets replayed, so the two cannot drift.
	request string
	// placeholders masks runtime-generated leaves by JSON pointer.
	placeholders map[string]string
	serve        func(*testing.T, *http.Request) capturedResponse
}

func captureErrorArtifactCases(t *testing.T) []any {
	t.Helper()

	cases := make([]any, 0, len(errorArtifactOrder()))
	for _, errorCase := range errorArtifactOrder() {
		entry := newJSONObject().
			set("id", errorCase.id).
			set("fixture", errorCase.fixture).
			set("method", errorCase.method).
			set("path", errorCase.path)

		var declared *jsonObject
		if errorCase.request != "" {
			declared = declaredContractObject(t, errorCase.id+".request", errorCase.request)
			entry.set("request", declared)
		}

		t.Run(errorCase.id, func(t *testing.T) {
			req := errorContractRequest(t, errorCase, declared)
			response := errorCase.serve(t, req)
			for pointer, placeholder := range errorCase.placeholders {
				setJSONPointer(t, response.body, pointer, placeholder)
			}
			entry.set("expectedStatus", json.Number(strconv.Itoa(response.status)))
			entry.set("body", response.body)
		})

		cases = append(cases, entry)
	}

	return cases
}

func errorContractRequest(t *testing.T, errorCase errorContractCase, declared *jsonObject) *http.Request {
	t.Helper()

	var body string
	if declared != nil {
		if raw, ok := declared.get("body"); ok {
			encoded, err := json.Marshal(orderedToPlain(raw))
			if err != nil {
				t.Fatalf("json.Marshal(%s request body) error = %v", errorCase.id, err)
			}
			body = string(encoded)
		}
	}

	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(errorCase.method, errorCase.path, nil)
	} else {
		req = httptest.NewRequest(errorCase.method, errorCase.path, strings.NewReader(body))
	}

	if declared != nil {
		if raw, ok := declared.get("headers"); ok {
			headers, ok := raw.(*jsonObject)
			if !ok {
				t.Fatalf("%s request.headers is not an object", errorCase.id)
			}
			for _, name := range headers.keys {
				value, ok := headers.values[name].(string)
				if !ok {
					t.Fatalf("%s request header %q is not a string", errorCase.id, name)
				}
				req.Header.Set(name, value)
			}
		}
	}

	return req
}

func errorArtifactOrder() []errorContractCase {
	uuidRequestID := map[string]string{"/requestId": "<uuid>"}

	return []errorContractCase{
		{
			id: "auth-misconfigured", fixture: "auth-misconfigured", method: http.MethodGet, path: "/api/v1/status",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve:   serveWithStatusAuthConfig(func(cfg *config.Config) { cfg.Server.AuthMode = config.AuthModeLocalToken; cfg.Server.LocalToken = nil }),
		},
		{
			id: "unauthorized", fixture: "auth-required", method: http.MethodGet, path: "/api/v1/status",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve: serveWithStatusAuthConfig(func(cfg *config.Config) {
				token := "secret-token"
				cfg.Server.AuthMode = config.AuthModeLocalToken
				cfg.Server.LocalToken = &token
			}),
		},
		{
			id: "route-not-found", fixture: "default", method: http.MethodGet, path: "/api/v1/does-not-exist",
			placeholders: uuidRequestID,
			serve:        serveWithStatusAuthConfig(nil),
		},
		{
			id: "method-not-allowed", fixture: "default", method: http.MethodDelete, path: "/api/v1/status",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve:   serveWithStatusAuthConfig(nil),
		},
		{
			id: "validation-failed", fixture: "default", method: http.MethodGet, path: "/api/v1/events?limit=0",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve:   serveEventAndPullRequestError,
		},
		{
			id: "loop-not-found", fixture: "default", method: http.MethodGet, path: "/api/v1/loops/missing-loop",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve:   serveLoopRouteError,
		},
		{
			id: "agent-not-configured", fixture: "default", method: http.MethodPost, path: "/api/v1/workers",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "projectId": "project_1",
    "title": "Wire runtime",
    "prompt": "Wire runtime",
    "repo": "acme/looper",
    "baseBranch": "main"
  }
}`,
			serve: serveWorkerError(nil, func(cfg *config.Config) { cfg.Agent.Vendor = nil }),
		},
		{
			id: "runtime-control-unavailable", fixture: "runtime-control-disabled", method: http.MethodPost, path: "/api/v1/runs/active/1/stop",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve:   serveRunRouteError,
		},
		{
			id: "active-run-not-found", fixture: "inactive-run", method: http.MethodGet, path: "/api/v1/runs/active/1",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve:   serveRunRouteError,
		},
		{
			id: "projects-unavailable", fixture: "default", method: http.MethodPost, path: "/api/v1/projects",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "repoPath": "/tmp/repos/looper",
    "name": "Looper"
  }
}`,
			serve: serveProjectsError(func(fixture testFixture) Context {
				return Context{
					Config:  fixture.config,
					Runtime: fixedRuntimeState{services: looperdruntime.Services{Projects: nil}},
				}
			}),
		},
		{
			id: "project-not-found", fixture: "default", method: http.MethodPost, path: "/api/v1/loops",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "projectId": "missing-project",
    "type": "worker",
    "targetType": "project",
    "targetId": "missing-project"
  }
}`,
			serve: serveLoopRouteError,
		},
		{
			id: "loop-conflict", fixture: "default", method: http.MethodPost, path: "/api/v1/loops",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "projectId": "project_1",
    "type": "reviewer",
    "targetType": "pull_request",
    "repo": "acme/looper",
    "prNumber": 42
  }
}`,
			serve: serveLoopRouteError,
		},
		{
			id: "project-ambiguous", fixture: "ambiguous-project-repo", method: http.MethodPost, path: "/api/v1/workers",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "repo": "acme/looper",
    "prompt": "Wire runtime",
    "baseBranch": "main"
  }
}`,
			serve: serveWorkerError(upsertWorkerErrorProject("Looper 2", "/tmp/repos/looper-2", "acme/looper"), nil),
		},
		{
			id: "pull-request-not-found", fixture: "default", method: http.MethodPost, path: "/api/v1/workers",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "projectId": "project_1",
    "repo": "acme/looper",
    "prNumber": 999,
    "baseBranch": "main"
  }
}`,
			serve: serveWorkerError(nil, nil),
		},
		{
			id: "pull-request-project-mismatch", fixture: "pull-request-mismatch", method: http.MethodPost, path: "/api/v1/workers",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "projectId": "project_2",
    "repo": "acme/looper",
    "prNumber": 42,
    "baseBranch": "main"
  }
}`,
			serve: serveWorkerError(upsertWorkerErrorProject("Mismatch", "/tmp/repos/mismatch", "other/repo"), nil),
		},
		{
			id: "pr-not-found", fixture: "default", method: http.MethodGet, path: "/api/v1/pull-requests/acme%2Flooper/999",
			request: `{"headers": {"x-request-id": "error-request-id"}}`,
			serve:   serveEventAndPullRequestError,
		},
		{
			id: "invalid-project-id", fixture: "projects-invalid-id", method: http.MethodPost, path: "/api/v1/projects",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "repoPath": "/tmp/repos/looper",
    "id": "../../tmp",
    "name": "Looper"
  }
}`,
			serve: serveProjectsError(func(fixture testFixture) Context {
				return Context{
					Config:          fixture.config,
					Runtime:         fixture.runtime,
					ProjectsService: fixture.runtime.Services().Projects,
				}
			}),
		},
		{
			id: "project-id-conflict", fixture: "projects-id-conflict", method: http.MethodPost, path: "/api/v1/projects",
			request: `{
  "headers": {
    "content-type": "application/json",
    "x-request-id": "error-request-id"
  },
  "body": {
    "repoPath": "/tmp/repos/looper",
    "id": "looper",
    "name": "Looper"
  }
}`,
			serve: serveProjectsError(func(fixture testFixture) Context {
				return Context{
					Config:  fixture.config,
					Runtime: fixture.runtime,
					ProjectsService: fakeProjectService{addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
						return projects.AddResult{}, projects.ProjectIDCollisionError{ProjectID: "looper"}
					}},
				}
			}),
		},
		{
			id: "internal-error", fixture: "projects-internal-error", method: http.MethodPost, path: "/api/v1/projects",
			request: `{
  "headers": {
    "content-type": "application/json"
  },
  "body": {
    "repoPath": "/tmp/repos/looper",
    "id": "looper",
    "name": "Looper"
  }
}`,
			placeholders: uuidRequestID,
			serve: serveProjectsError(func(fixture testFixture) Context {
				return Context{
					Config:  fixture.config,
					Runtime: fixture.runtime,
					ProjectsService: fakeProjectService{addProject: func(context.Context, projects.AddInput) (projects.AddResult, error) {
						return projects.AddResult{}, errors.New("boom")
					}},
				}
			}),
		},
	}
}

func serveWithStatusAuthConfig(configure func(*config.Config)) func(*testing.T, *http.Request) capturedResponse {
	return func(t *testing.T, req *http.Request) capturedResponse {
		t.Helper()
		fixture := newTestFixture(t)
		cfg := fixture.config
		if configure != nil {
			configure(&cfg)
		}
		h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(fixture.runtime, cfg)})

		return captureResponse(t, h, fixture.rootDir, req)
	}
}

func serveEventAndPullRequestError(t *testing.T, req *http.Request) capturedResponse {
	t.Helper()
	fixture := newTestFixture(t)
	seedEventAndPullRequestRouteData(t, fixture.runtime)
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }})

	return captureResponse(t, h, fixture.rootDir, req)
}

func serveLoopRouteError(t *testing.T, req *http.Request) capturedResponse {
	t.Helper()
	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now.Add(time.Minute) }})

	return captureResponse(t, h, fixture.rootDir, req)
}

func serveRunRouteError(t *testing.T, req *http.Request) capturedResponse {
	t.Helper()
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)
	completeRunRouteFixture(t, fixture)
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})

	return captureResponse(t, h, fixture.rootDir, req)
}

// completeRunRouteFixture retires loop_1 and run_1 so the active-run views see
// no live work, which is what both run-route error cases depend on.
func completeRunRouteFixture(t *testing.T, fixture testFixture) {
	t.Helper()

	completedAt := fixture.now.Add(10 * time.Minute).UTC().Format(javaScriptISOString)
	run, err := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), "run_1")
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID(run_1) = %v, %v", run, err)
	}
	completedRun := *run
	completedRun.Status = "completed"
	completedRun.EndedAt = &completedAt
	completedRun.UpdatedAt = completedAt
	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), completedRun); err != nil {
		t.Fatalf("Runs.Upsert(completed) error = %v", err)
	}

	loop, err := fixture.runtime.Services().Repositories.Loops.GetByID(context.Background(), "loop_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID(loop_1) = %v, %v", loop, err)
	}
	completedLoop := *loop
	completedLoop.Status = "completed"
	completedLoop.UpdatedAt = completedAt
	if err := fixture.runtime.Services().Repositories.Loops.Upsert(context.Background(), completedLoop); err != nil {
		t.Fatalf("Loops.Upsert(completed) error = %v", err)
	}
}

func serveWorkerError(seed func(*testing.T, testFixture), configure func(*config.Config)) func(*testing.T, *http.Request) capturedResponse {
	return func(t *testing.T, req *http.Request) capturedResponse {
		t.Helper()
		fixture := newTestFixture(t)
		seedLoopRouteData(t, fixture.runtime)
		seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

		nowISO := fixture.now.UTC().Format(javaScriptISOString)
		if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), storage.PullRequestSnapshotRecord{
			ID:         "prs_1",
			ProjectID:  "project_1",
			Repo:       "acme/looper",
			PRNumber:   42,
			HeadSHA:    "abc123",
			CapturedAt: nowISO,
			CreatedAt:  nowISO,
		}); err != nil {
			t.Fatalf("PullRequestSnapshots.Upsert(prs_1) error = %v", err)
		}
		if seed != nil {
			seed(t, fixture)
		}

		cfg := fixture.config
		if configure != nil {
			configure(&cfg)
		}
		h := NewHandler(Context{
			Config:  cfg,
			Runtime: runtimeWithConfig(fixture.runtime, cfg),
			Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		})

		return captureResponse(t, h, fixture.rootDir, req)
	}
}

func upsertWorkerErrorProject(name, repoPath, repo string) func(*testing.T, testFixture) {
	return func(t *testing.T, fixture testFixture) {
		t.Helper()
		nowISO := fixture.now.UTC().Format(javaScriptISOString)
		metadata := `{"repo":"` + repo + `","worktreeRoot":null,"source":"api"}`
		baseBranch := "main"
		if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
			ID:           "project_2",
			Name:         name,
			RepoPath:     repoPath,
			BaseBranch:   &baseBranch,
			Archived:     false,
			MetadataJSON: &metadata,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}); err != nil {
			t.Fatalf("Projects.Upsert(project_2) error = %v", err)
		}
	}
}

func serveProjectsError(build func(testFixture) Context) func(*testing.T, *http.Request) capturedResponse {
	return func(t *testing.T, req *http.Request) capturedResponse {
		t.Helper()
		fixture := newTestFixture(t)
		h := NewHandler(build(fixture))

		return captureResponse(t, h, fixture.rootDir, req)
	}
}

func orderedToPlain(value any) any {
	switch typed := value.(type) {
	case *jsonObject:
		plain := make(map[string]any, len(typed.keys))
		for _, key := range typed.keys {
			plain[key] = orderedToPlain(typed.values[key])
		}
		return plain
	case []any:
		plain := make([]any, len(typed))
		for i, item := range typed {
			plain[i] = orderedToPlain(item)
		}
		return plain
	default:
		return value
	}
}
