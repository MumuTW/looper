package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newWorkerIssueOccupancyHandler builds a handler with a forged
// LookupIssueOccupancy so tests can exercise the dispatch-time forge check
// without shelling out to gh.
func newWorkerIssueOccupancyHandler(fixture testFixture, lookup func(context.Context, string, int64, string) (IssueOccupancy, error)) *Handler {
	return NewHandler(Context{
		Config:               fixture.config,
		Runtime:              fixture.runtime,
		Now:                  func() time.Time { return fixture.now.Add(2 * time.Minute) },
		LookupIssueOccupancy: lookup,
	})
}

func postWorkerIssue(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(body)))
	req.Header.Set("x-request-id", "worker-issue-occupancy")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	return recorder
}

// TestHandlerWorkerCreateRefusesClosedIssue refuses dispatch when the forge
// reports the target issue is closed, and writes no loop or queue item.
func TestHandlerWorkerCreateRefusesClosedIssue(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	lookup := func(_ context.Context, repo string, issueNumber int64, _ string) (IssueOccupancy, error) {
		return IssueOccupancy{Repo: repo, IssueNumber: issueNumber, State: "CLOSED"}, nil
	}
	recorder := postWorkerIssue(t, newWorkerIssueOccupancyHandler(fixture, lookup), `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("is closed")) {
		t.Fatalf("body = %s, want 'is closed' reason", recorder.Body.String())
	}
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 0 {
		t.Fatalf("queue items = %#v, want no enqueued work for a refused closed issue", queueItems)
	}
}

// TestHandlerWorkerCreateRefusesOpenReferencingPullRequest refuses dispatch
// when an open pull request already references the target issue, and names it.
func TestHandlerWorkerCreateRefusesOpenReferencingPullRequest(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	lookup := func(_ context.Context, repo string, issueNumber int64, _ string) (IssueOccupancy, error) {
		return IssueOccupancy{
			Repo:             repo,
			IssueNumber:      issueNumber,
			State:            "OPEN",
			OpenPullRequests: []IssueOccupantPullRequest{{Number: 181, State: "OPEN"}},
		}, nil
	}
	recorder := postWorkerIssue(t, newWorkerIssueOccupancyHandler(fixture, lookup), `{"projectId":"project_1","repo":"acme/looper","issueNumber":93,"baseBranch":"main"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("#181")) {
		t.Fatalf("body = %s, want open pull request #181 named", recorder.Body.String())
	}
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 0 {
		t.Fatalf("queue items = %#v, want no enqueued work for a refused occupied issue", queueItems)
	}
}

// TestHandlerWorkerCreateForceOverridesOccupiedIssue dispatches anyway when
// force is true, even though the forge reports the issue closed and an open PR.
func TestHandlerWorkerCreateForceOverridesOccupiedIssue(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	called := false
	lookup := func(_ context.Context, repo string, issueNumber int64, _ string) (IssueOccupancy, error) {
		called = true
		return IssueOccupancy{Repo: repo, IssueNumber: issueNumber, State: "CLOSED", OpenPullRequests: []IssueOccupantPullRequest{{Number: 181, State: "OPEN"}}}, nil
	}
	recorder := postWorkerIssue(t, newWorkerIssueOccupancyHandler(fixture, lookup), `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main","force":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if called {
		t.Fatal("LookupIssueOccupancy called for force=true, want skipped")
	}
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 1 {
		t.Fatalf("queue items = %d, want 1 enqueued worker for forced dispatch", len(queueItems))
	}
}

// TestHandlerWorkerCreateForgeUnreachableFallsBackToLocalCheck ensures a
// transient forge outage does not hard-fail dispatch: the local occupancy check
// from #319 keeps working, and prepare-work re-validates on claim.
func TestHandlerWorkerCreateForgeUnreachableFallsBackToLocalCheck(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	lookup := func(_ context.Context, _ string, _ int64, _ string) (IssueOccupancy, error) {
		return IssueOccupancy{}, fmt.Errorf("simulated transient forge outage")
	}
	recorder := postWorkerIssue(t, newWorkerIssueOccupancyHandler(fixture, lookup), `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (forge unreachable falls back to local check); body=%s", recorder.Code, recorder.Body.String())
	}
	queueItems, err := fixture.runtime.Services().Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(queueItems) != 1 {
		t.Fatalf("queue items = %d, want 1 enqueued worker when forge is unreachable", len(queueItems))
	}
}

// TestHandlerWorkerCreateIssueNotFoundRefused ensures a 404 from the forge
// refuses dispatch with a clear message rather than enqueueing phantom work.
func TestHandlerWorkerCreateIssueNotFoundRefused(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)

	lookup := func(_ context.Context, _ string, _ int64, _ string) (IssueOccupancy, error) {
		return IssueOccupancy{}, fmt.Errorf("%w: nope", errIssueNotFound)
	}
	recorder := postWorkerIssue(t, newWorkerIssueOccupancyHandler(fixture, lookup), `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("not found")) {
		t.Fatalf("body = %s, want 'not found' reason", recorder.Body.String())
	}
}

// TestIssueOccupancyOccupied covers the Occupied predicate directly.
func TestIssueOccupancyOccupied(t *testing.T) {
	tests := []struct {
		name     string
		occ      IssueOccupancy
		occupied bool
	}{
		{"open no PRs", IssueOccupancy{State: "OPEN"}, false},
		{"closed", IssueOccupancy{State: "CLOSED"}, true},
		{"open with PR", IssueOccupancy{State: "OPEN", OpenPullRequests: []IssueOccupantPullRequest{{Number: 1}}}, true},
		{"is PR", IssueOccupancy{IsPullRequest: true}, true},
		{"empty state no PRs", IssueOccupancy{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.occ.Occupied(); got != tt.occupied {
				t.Fatalf("Occupied() = %v, want %v", got, tt.occupied)
			}
		})
	}
}
