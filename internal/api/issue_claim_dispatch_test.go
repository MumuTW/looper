package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

func strPtr(s string) *string { return &s }

// Verifies that when an active fixer loop already targets an issue,
// POST /api/v1/workers returns 409 StatusConflict with loop ID.
func TestHandlerWorkerCreateRefusesCollisionWithActiveFixerLoop(t *testing.T) {
	fixture := newTestFixture(t)
	repoPath := filepath.Join(fixture.rootDir, "repo-worker-held-issue")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repos := fixture.runtime.Services().Repositories
	issueTarget := "issue:acme/looper:77"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_77",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       string(domain.LoopTypeFixer),
		TargetType: string(domain.LoopTargetTypeIssue),
		TargetID:   &issueTarget,
		Repo:       strPtr("acme/looper"),
		Status:     string(domain.LoopStatusRunning),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
		UpdatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("Loops.Upsert() error: %v", err)
	}

	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	respBody := recorder.Body.String()
	if !strings.Contains(respBody, "occupied") || !strings.Contains(respBody, "loop_fixer_77") {
		t.Fatalf("expected occupation info in body, got: %s", respBody)
	}
}

// Verifies that when an active reviewer loop already targets an issue,
// POST /api/v1/workers returns 409 StatusConflict with loop ID.
func TestHandlerWorkerCreateRefusesCollisionWithActiveReviewerLoop(t *testing.T) {
	fixture := newTestFixture(t)
	repoPath := filepath.Join(fixture.rootDir, "repo-reviewer-held-issue")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repos := fixture.runtime.Services().Repositories
	issueTarget := "issue:acme/looper:77"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_reviewer_77",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       string(domain.LoopTypeReviewer),
		TargetType: string(domain.LoopTargetTypeIssue),
		TargetID:   &issueTarget,
		Repo:       strPtr("acme/looper"),
		Status:     string(domain.LoopStatusRunning),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
		UpdatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("Loops.Upsert() error: %v", err)
	}

	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	respBody := recorder.Body.String()
	if !strings.Contains(respBody, "occupied") || !strings.Contains(respBody, "loop_reviewer_77") {
		t.Fatalf("expected occupation info in body, got: %s", respBody)
	}
}

// Verifies that force=true overrides the collision check.
func TestHandlerWorkerCreateForceOverridesCollision(t *testing.T) {
	fixture := newTestFixture(t)
	repoPath := filepath.Join(fixture.rootDir, "repo-force-override")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repos := fixture.runtime.Services().Repositories
	issueTarget := "issue:acme/looper:77"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_fixer_77",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       string(domain.LoopTypeFixer),
		TargetType: string(domain.LoopTargetTypeIssue),
		TargetID:   &issueTarget,
		Repo:       strPtr("acme/looper"),
		Status:     string(domain.LoopStatusRunning),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
		UpdatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("Loops.Upsert() error: %v", err)
	}

	// With force=true, dispatch should skip collision check. The exact outcome
	// depends on other validations (gh lookup), but it must NOT be 409.
	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main","force":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code == http.StatusConflict {
		t.Fatalf("force=true should skip collision check, got 409; body=%s", recorder.Body.String())
	}
}

// Verifies that terminated/failed/completed loops do not trigger collision.
func TestHandlerWorkerCreateIgnoresTerminalLoops(t *testing.T) {
	fixture := newTestFixture(t)
	repoPath := filepath.Join(fixture.rootDir, "repo-terminal-loop")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repos := fixture.runtime.Services().Repositories
	issueTarget := "issue:acme/looper:77"
	// Terminal states: failed, completed, terminated.
	var seq int64 = 0
	for _, status := range []string{"failed", "completed", "terminated"} {
		seq++
		if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
			ID:         "loop_fixer_77_" + status,
			Seq:        seq,
			ProjectID:  "project_1",
			Type:       string(domain.LoopTypeFixer),
			TargetType: string(domain.LoopTargetTypeIssue),
			TargetID:   &issueTarget,
			Repo:       strPtr("acme/looper"),
			Status:     status,
			CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
			UpdatedAt:  fixture.now.UTC().Format(javaScriptISOString),
		}); err != nil {
			t.Fatalf("Loops.Upsert() error: %v", err)
		}
	}

	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code == http.StatusConflict {
		t.Fatalf("terminal loops should not trigger collision, got 409; body=%s", recorder.Body.String())
	}
}

// Verifies that planner loops are NOT treated as collisions (planner is the
// normal upstream of a worker).
func TestHandlerWorkerCreateAllowsWhenOnlyPlannerIsLive(t *testing.T) {
	fixture := newTestFixture(t)
	repoPath := filepath.Join(fixture.rootDir, "repo-planner-live")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repos := fixture.runtime.Services().Repositories
	issueTarget := "issue:acme/looper:77"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_planner_77",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       string(domain.LoopTypePlanner),
		TargetType: string(domain.LoopTargetTypeIssue),
		TargetID:   &issueTarget,
		Repo:       strPtr("acme/looper"),
		Status:     string(domain.LoopStatusRunning),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
		UpdatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("Loops.Upsert() error: %v", err)
	}

	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code == http.StatusConflict {
		t.Fatalf("planner loop should not trigger collision, got 409; body=%s", recorder.Body.String())
	}
}

// Verifies that worker loops are NOT treated as collisions (loop reuse
// handles them upstream).
func TestHandlerWorkerCreateAllowsWhenOnlyWorkerIsLive(t *testing.T) {
	fixture := newTestFixture(t)
	repoPath := filepath.Join(fixture.rootDir, "repo-worker-live")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	repos := fixture.runtime.Services().Repositories
	issueTarget := "issue:acme/looper:77"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID:         "loop_worker_77",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       string(domain.LoopTypeWorker),
		TargetType: string(domain.LoopTargetTypeIssue),
		TargetID:   &issueTarget,
		Repo:       strPtr("acme/looper"),
		Status:     string(domain.LoopStatusRunning),
		CreatedAt:  fixture.now.UTC().Format(javaScriptISOString),
		UpdatedAt:  fixture.now.UTC().Format(javaScriptISOString),
	}); err != nil {
		t.Fatalf("Loops.Upsert() error: %v", err)
	}

	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code == http.StatusConflict {
		t.Fatalf("worker loop should not trigger collision (reuse handles it), got 409; body=%s", recorder.Body.String())
	}
}
