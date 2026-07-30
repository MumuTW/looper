package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/storage"
)

// A pull request Worker can be pointed at is not the same thing as a pull
// request Looper has already snapshotted, and the spec path a Worker needs is
// not always written into the pull request body. These cover both.

func postWorker(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(body)))
	req.Header.Set("x-request-id", "worker-pr-target")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	return recorder
}

func seedPullRequestSnapshot(t *testing.T, fixture testFixture, id string, prNumber int64, payloadJSON *string) {
	t.Helper()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	record := storage.PullRequestSnapshotRecord{
		ID:          id,
		ProjectID:   "project_1",
		Repo:        "acme/looper",
		PRNumber:    prNumber,
		HeadSHA:     "head-sha",
		PayloadJSON: payloadJSON,
		CapturedAt:  nowISO,
		CreatedAt:   nowISO,
	}
	if err := fixture.runtime.Services().Repositories.PullRequestSnapshots.Upsert(context.Background(), record); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert(%s) error = %v", id, err)
	}
}

func newWorkerTargetHandler(fixture testFixture, lookup func(context.Context, string, int64, string) (PullRequestTarget, error)) *Handler {
	return NewHandler(Context{
		Config:            fixture.config,
		Runtime:           fixture.runtime,
		Now:               func() time.Time { return fixture.now.Add(2 * time.Minute) },
		LookupPullRequest: lookup,
	})
}

func TestHandlerWorkerCreateAcceptsSpecPathAlongsidePullRequest(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	seedPullRequestSnapshot(t, fixture, "prs_open", 42, nil)

	recorder := postWorker(t, newWorkerTargetHandler(fixture, nil), `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"specPath":"specs/2026-07-30-thing.md","baseBranch":"main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	assertEqual(t, data["specPath"], "specs/2026-07-30-thing.md")
	assertEqual(t, data["targetType"], "pull_request")

	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 1 {
		t.Fatalf("Queue.List() = %#v, want one enqueued worker", queueItems)
	}
	payload := parseJSONObject(queueItems[0].PayloadJSON)
	assertEqual(t, payload["specPath"], "specs/2026-07-30-thing.md")
	if queueItems[0].LockKey == nil || *queueItems[0].LockKey != storage.PullRequestLockKey("project_1", "acme/looper", 42) {
		t.Fatalf("queueItems[0].LockKey = %#v, want the shared per-PR lock", queueItems[0].LockKey)
	}
}

func TestHandlerWorkerCreateResolvesUnsnapshottedPullRequestThroughForge(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	lookedUp := 0
	handler := newWorkerTargetHandler(fixture, func(_ context.Context, repo string, prNumber int64, cwd string) (PullRequestTarget, error) {
		lookedUp++
		assertEqual(t, repo, "acme/looper")
		assertEqual(t, cwd, "/tmp/repos/looper")
		return PullRequestTarget{Number: prNumber, State: "OPEN"}, nil
	})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","prNumber":77,"specPath":"specs/2026-07-30-thing.md","baseBranch":"main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if lookedUp != 1 {
		t.Fatalf("LookupPullRequest called %d times, want 1", lookedUp)
	}
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 1 || queueItems[0].PRNumber == nil || *queueItems[0].PRNumber != 77 {
		t.Fatalf("Queue.List() = %#v, want one worker item for PR 77", queueItems)
	}
}

func TestHandlerWorkerCreateKeepsSnapshotOnlyBehaviorWithoutForgeLookup(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	recorder := postWorker(t, newWorkerTargetHandler(fixture, nil), `{"projectId":"project_1","repo":"acme/looper","prNumber":77,"baseBranch":"main"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["error"].(map[string]any)["code"], "PULL_REQUEST_NOT_FOUND")
}

func TestHandlerWorkerCreateRejectsPullRequestTheForgeCannotFind(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	handler := newWorkerTargetHandler(fixture, func(context.Context, string, int64, string) (PullRequestTarget, error) {
		// The production lookup classifies a forge "does not exist" result with
		// errPullRequestNotFound; only that becomes a 404.
		return PullRequestTarget{}, fmt.Errorf("%w: no pull requests found for branch", errPullRequestNotFound)
	})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","prNumber":77,"baseBranch":"main"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["error"].(map[string]any)["code"], "PULL_REQUEST_NOT_FOUND")
	assertNoQueuedWork(t, fixture)
}

func TestHandlerWorkerCreateSurfacesForgeLookupFailuresAsServerError(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	handler := newWorkerTargetHandler(fixture, func(context.Context, string, int64, string) (PullRequestTarget, error) {
		// An operational failure (timeout, unavailable executable) is not a
		// missing PR and must stay a retryable server error, not a 404.
		return PullRequestTarget{}, errors.New("context deadline exceeded")
	})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","prNumber":77,"baseBranch":"main"}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["error"].(map[string]any)["code"], "INTERNAL_ERROR")
	assertNoQueuedWork(t, fixture)
}

func TestHandlerWorkerCreateRejectsMergedPullRequest(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	handler := newWorkerTargetHandler(fixture, func(_ context.Context, _ string, prNumber int64, _ string) (PullRequestTarget, error) {
		return PullRequestTarget{Number: prNumber, State: "MERGED", Merged: true}, nil
	})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","prNumber":77,"baseBranch":"main"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	assertNoQueuedWork(t, fixture)
}

func TestHandlerWorkerCreateRejectsClosedSnapshottedPullRequest(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	payload := `{"detail":{"state":"CLOSED"}}`
	seedPullRequestSnapshot(t, fixture, "prs_closed", 42, &payload)

	recorder := postWorker(t, newWorkerTargetHandler(fixture, nil), `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"baseBranch":"main"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	assertNoQueuedWork(t, fixture)
}

// A snapshot captured while open does not prove the PR is still open. The forge
// is the authority for current state, so a stale-open snapshot must be refreshed
// before the PR is accepted: a PR the forge now reports closed is rejected even
// though the snapshot still says open.
func TestHandlerWorkerCreateRefreshesStaleOpenSnapshotBeforeAccepting(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	openPayload := `{"detail":{"state":"OPEN"}}`
	seedPullRequestSnapshot(t, fixture, "prs_stale_open", 42, &openPayload)

	handler := newWorkerTargetHandler(fixture, func(_ context.Context, _ string, prNumber int64, _ string) (PullRequestTarget, error) {
		return PullRequestTarget{Number: prNumber, State: "closed"}, nil
	})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"baseBranch":"main"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	assertNoQueuedWork(t, fixture)
}

// When the forge refresh confirms a snapshotted PR is still open, the dispatch
// proceeds and the snapshot's PR number is the one queued.
func TestHandlerWorkerCreateAcceptsSnapshottedPullRequestFreshlyConfirmedOpen(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	openPayload := `{"detail":{"state":"OPEN"}}`
	seedPullRequestSnapshot(t, fixture, "prs_open_confirmed", 42, &openPayload)

	lookedUp := 0
	handler := newWorkerTargetHandler(fixture, func(_ context.Context, _ string, prNumber int64, _ string) (PullRequestTarget, error) {
		lookedUp++
		return PullRequestTarget{Number: prNumber, State: "open"}, nil
	})

	recorder := postWorker(t, handler, `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"specPath":"specs/thing.md","baseBranch":"main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if lookedUp != 1 {
		t.Fatalf("LookupPullRequest called %d times, want 1", lookedUp)
	}
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 1 || queueItems[0].PRNumber == nil || *queueItems[0].PRNumber != 42 {
		t.Fatalf("Queue.List() = %#v, want one worker item for PR 42", queueItems)
	}
}

func TestHandlerWorkerCreateStillRejectsConflictingTargets(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	handler := newWorkerTargetHandler(fixture, nil)

	for name, body := range map[string]string{
		"pr and issue":        `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"issueNumber":7,"baseBranch":"main"}`,
		"issue and spec path": `{"projectId":"project_1","repo":"acme/looper","issueNumber":7,"specPath":"specs/x.md","baseBranch":"main"}`,
		"no input at all":     `{"projectId":"project_1","repo":"acme/looper","baseBranch":"main"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := postWorker(t, handler, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	assertNoQueuedWork(t, fixture)
}

func assertNoQueuedWork(t *testing.T, fixture testFixture) {
	t.Helper()
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 0 {
		t.Fatalf("Queue.List() = %#v, want no enqueued work", queueItems)
	}
}
