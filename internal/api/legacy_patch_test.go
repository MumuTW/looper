package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/storage"
)

func TestHandlerProjectsPatchRepairsLegacyProjectWithoutValidationStance(t *testing.T) {
	runtime, cfg := startLegacyProjectRuntime(t)
	runtime.Services().Projects.ScheduleDiscovery = func(func()) {}
	handler := legacyProjectHandler(runtime, cfg)
	if got := runtime.Config().Projects; len(got) != 0 {
		t.Fatalf("runtime catalog projects = %#v, want legacy inert project quarantined", got)
	}

	workerRecorder := httptest.NewRecorder()
	workerRequest := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader([]byte(`{"projectId":"legacy_inert","repo":"acme/app","baseBranch":"main","prompt":"repair me"}`)))
	NewHandler(Context{Config: cfg, Runtime: runtime}).ServeHTTP(workerRecorder, workerRequest)
	if workerRecorder.Code != http.StatusBadRequest || !strings.Contains(workerRecorder.Body.String(), "validation policy is repaired") {
		t.Fatalf("manual worker without ConfigSnapshot status = %d body=%s, want quarantined project rejection", workerRecorder.Code, workerRecorder.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/legacy_inert", bytes.NewReader([]byte(`{"repo":"acme/app"}`)))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status without validation stance = %d, want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	stored, err := runtime.Services().Repositories.Projects.GetByID(context.Background(), "legacy_inert")
	if err != nil || stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() after rejected patch = %#v, %v", stored, err)
	}
	if strings.Contains(*stored.MetadataJSON, `"repo":"acme/app"`) {
		t.Fatalf("metadata after rejected patch = %s, want legacy inert record unchanged", *stored.MetadataJSON)
	}

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPatch, "/api/v1/projects/legacy_inert", bytes.NewReader([]byte(`{"repo":"acme/app","validation":{"optOut":true}}`)))
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	assertEqual(t, data["repo"], "acme/app")
	stored, err = runtime.Services().Repositories.Projects.GetByID(context.Background(), "legacy_inert")
	if err != nil || stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() = %#v, %v", stored, err)
	}
	if !strings.Contains(*stored.MetadataJSON, `"repo":"acme/app"`) || !strings.Contains(*stored.MetadataJSON, `"validation":{"optOut":true}`) {
		t.Fatalf("metadata = %s, want repo and explicit validation opt-out", *stored.MetadataJSON)
	}
	if got := runtime.Config().Projects; len(got) != 1 || got[0].ID != "legacy_inert" {
		t.Fatalf("runtime catalog projects after repair = %#v, want repaired project published", got)
	}
}

func TestHandlerProjectsPatchRejectsInvalidAuthoredPolicyWithoutAgent(t *testing.T) {
	runtime, cfg := startLegacyProjectRuntimeWithAgent(t, false)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/legacy_inert", bytes.NewReader([]byte(`{"validation":{"commands":[]}}`)))
	NewHandler(Context{Config: cfg, Runtime: runtime}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	stored, err := runtime.Services().Repositories.Projects.GetByID(context.Background(), "legacy_inert")
	if err != nil || stored == nil || stored.MetadataJSON == nil {
		t.Fatalf("Projects.GetByID() = %#v, %v", stored, err)
	}
	if strings.Contains(*stored.MetadataJSON, `"validation"`) {
		t.Fatalf("metadata = %s, invalid authored policy was persisted", *stored.MetadataJSON)
	}
}

func TestLegacyProjectRuntimeReloadKeepsQuarantine(t *testing.T) {
	runtime, _ := startLegacyProjectRuntime(t)

	if err := runtime.ReloadConfig(context.Background()); err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	if got := runtime.Config().Projects; len(got) != 0 {
		t.Fatalf("runtime catalog projects after reload = %#v, want quarantine preserved", got)
	}
}

func startLegacyProjectRuntime(t *testing.T) (*looperdruntime.Runtime, config.Config) {
	return startLegacyProjectRuntimeWithAgent(t, true)
}

func legacyProjectHandler(runtime *looperdruntime.Runtime, cfg config.Config) *Handler {
	return NewHandler(Context{
		Config:  cfg,
		Runtime: runtime,
		ConfigSnapshot: func() (config.Config, ConfigMetadata) {
			return runtime.Config(), ConfigMetadata{}
		},
	})
}

func startLegacyProjectRuntimeWithAgent(t *testing.T, withAgent bool) (*looperdruntime.Runtime, config.Config) {
	t.Helper()

	rootDir := t.TempDir()
	homeDir := t.TempDir()
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", homeDir, err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("LOOPER_HOME", filepath.Join(homeDir, ".looper"))
	cfg, err := config.DefaultConfig(rootDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	backupDir := filepath.Join(rootDir, "backups")
	cfg.Storage.DBPath = filepath.Join(rootDir, "state", "looper.sqlite")
	cfg.Storage.BackupDir = &backupDir
	cfg.Daemon.LogDir = filepath.Join(rootDir, "logs")
	cfg.Daemon.WorkingDirectory = rootDir
	if withAgent {
		vendor := config.AgentVendorOpenCode
		cfg.Agent.Vendor = &vendor
	}

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), cfg.Storage.DBPath, storage.SQLiteCoordinatorOptions{BackupDir: backupDir})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		_ = coordinator.Close()
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	nowISO := now.Format(javaScriptISOString)
	baseBranch := "develop"
	// A project saved before #329 has neither repository metadata nor a
	// validation stance. Seed it before the upgraded daemon starts.
	metadata := `{"repo":null,"worktreeRoot":"/tmp/worktrees","source":"api","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	repositories := storage.NewRepositories(coordinator.DB())
	if err := repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "legacy_inert", Name: "Legacy", RepoPath: "/tmp/legacy", BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		_ = coordinator.Close()
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("SQLiteCoordinator.Close() error = %v", err)
	}

	loaded := config.LoadedFileConfig{Config: cfg}
	runtime := looperdruntime.New(looperdruntime.Options{
		Config:        cfg,
		InitialConfig: loaded,
		ReloadConfig: func() (config.LoadedFileConfig, error) {
			return loaded, nil
		},
		Logger: noopLogger{},
		Now: func() time.Time {
			return now
		},
		RunSchedulerTick: func(context.Context, looperdruntime.Services) error { return nil },
	})
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	if err := runtime.CompleteStartup(context.Background()); err != nil {
		runtime.Stop("test cleanup")
		t.Fatalf("Runtime.CompleteStartup() error = %v", err)
	}
	t.Cleanup(func() { runtime.Stop("test cleanup") })
	return runtime, cfg
}
