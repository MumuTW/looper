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
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	respBody := recorder.Body.String()
	if !strings.Contains(respBody, "occupied") && !strings.Contains(respBody, "loop_fixer_77") {
		t.Fatalf("body should indicate occupation, got: %s", respBody)
	}
}

func TestHandlerWorkerCreateAllowsDispatchWhenOnlyPlannerIsLive(t *testing.T) {
	fixture := newTestFixture(t)
	seedWorkerPlannerArtifactsData(t, fixture.runtime, fixture.now)
	repoPath := filepath.Join(fixture.rootDir, "repo-planner-live")
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", repoPath, err)
	}
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: repoPath, MetadataJSON: &metadata, CreatedAt: fixture.now.UTC().Format(javaScriptISOString), UpdatedAt: fixture.now.UTC().Format(javaScriptISOString)}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (planner live is OK)", recorder.Code)
	}
}

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

	body := `{"projectId":"project_1","repo":"acme/looper","issueNumber":77,"baseBranch":"main","force":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(body))
	req.Header.Set("x-request-id", "fixture-request-id")
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime, Now: func() time.Time { return fixture.now }}).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (force override)", recorder.Code)
	}
}
