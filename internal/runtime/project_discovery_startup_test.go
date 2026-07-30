package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
)

func TestRuntimeResumesIncompleteDiscoveryOnlyAfterStartupReady(t *testing.T) {
	t.Parallel()

	rt, projectService := startRuntimeWithPendingDiscovery(t, "ready")
	discoveryStarted := make(chan AdmissionState, 1)
	projectService.ListWorktrees = func(context.Context, string) ([]projects.WorktreeListEntry, error) {
		discoveryStarted <- rt.AdmissionState()
		return nil, nil
	}

	select {
	case state := <-discoveryStarted:
		t.Fatalf("discovery started before CompleteStartup with admission %q", state)
	default:
	}
	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	select {
	case state := <-discoveryStarted:
		if state != AdmissionReady {
			t.Fatalf("discovery started with admission %q, want ready", state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending discovery was not resumed after startup became ready")
	}
}

func TestRuntimeFailedStartupDoesNotResumeIncompleteDiscovery(t *testing.T) {
	t.Parallel()

	rt, projectService := startRuntimeWithPendingDiscovery(t, "failed")
	discoveryStarted := make(chan struct{}, 1)
	projectService.ListWorktrees = func(context.Context, string) ([]projects.WorktreeListEntry, error) {
		discoveryStarted <- struct{}{}
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.CompleteStartup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CompleteStartup() error = %v, want context canceled", err)
	}
	select {
	case <-discoveryStarted:
		t.Fatal("incomplete discovery resumed despite failed startup")
	case <-time.After(100 * time.Millisecond):
	}
}

func startRuntimeWithPendingDiscovery(t *testing.T, projectID string) (*Runtime, *projects.Service) {
	t.Helper()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	rt := New(Options{
		Config:           cfg,
		Logger:           &testLogger{},
		DeferRecovery:    true,
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	services := rt.Services()
	nowISO := formatJavaScriptISOString(time.Now().UTC())
	baseBranch := "main"
	metadata := `{"registrationDiscovery":{"status":"pending","snapshotMode":"off"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID:           projectID,
		Name:         projectID,
		RepoPath:     workingDir,
		BaseBranch:   &baseBranch,
		MetadataJSON: &metadata,
		CreatedAt:    nowISO,
		UpdatedAt:    nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return rt, services.Projects
}
