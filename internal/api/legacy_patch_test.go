package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestHandlerProjectsPatchRepairsLegacyProjectWithoutValidationStance(t *testing.T) {
	fixture := newTestFixture(t)
	fixture.runtime.Services().Projects.ScheduleDiscovery = func(func()) {}
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	baseBranch := "develop"
	// Legacy API project registered before #329: no validation field at all
	metadata := `{"repo":null,"worktreeRoot":"/tmp/worktrees","source":"api","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := fixture.runtime.Services().Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "legacy_inert", Name: "Legacy", RepoPath: "/tmp/legacy", BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/legacy_inert", bytes.NewReader([]byte(`{"repo":"acme/app"}`)))
	NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, data["repo"], "acme/app")
	stored, err := fixture.runtime.Services().Repositories.Projects.GetByID(context.Background(), "legacy_inert")
	if err != nil || stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() = %#v, %v", stored, err)
	}
	if !strings.Contains(*stored.MetadataJSON, `"repo":"acme/app"`) {
		t.Fatalf("metadata = %s, want repo set", *stored.MetadataJSON)
	}
}
