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

func TestRuntimeFailedStartCancelsAndDrainsConfiguredProjectDiscovery(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	discoveryStarted := make(chan struct{})
	discoveryCanceled := make(chan struct{})
	startupErr := errors.New("configured project sync failed")
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		SyncConfiguredProjects: func(_ context.Context, service *projects.Service, _ config.Config, _ time.Time) error {
			service.ScheduleDiscovery(func() {
				close(discoveryStarted)
				<-service.DiscoveryContext().Done()
				close(discoveryCanceled)
			})
			<-discoveryStarted
			return startupErr
		},
	})

	if err := rt.Start(context.Background()); !errors.Is(err, startupErr) {
		t.Fatalf("Start() error = %v, want %v", err, startupErr)
	}
	select {
	case <-discoveryCanceled:
	default:
		t.Fatal("Start() returned before configured-project discovery was canceled and drained")
	}
}

func TestRuntimeShutdownCanceledDiscoveryResumesAfterRestart(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Daemon.ShutdownTimeoutMS = 500

	first := New(Options{Config: cfg, Logger: &testLogger{}, RunSchedulerTick: func(context.Context, Services) error { return nil }})
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	firstDiscoveryStarted := make(chan struct{})
	first.Services().Projects.DetectRepo = nil
	first.Services().Projects.ListWorktrees = func(ctx context.Context, _ string) ([]projects.WorktreeListEntry, error) {
		close(firstDiscoveryStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, err := first.Services().Projects.AddProject(context.Background(), projects.AddInput{
		ID: "restart", Name: "Restart", RepoPath: workingDir, BaseBranch: "main", SnapshotMode: projects.SnapshotModeOff,
	}); err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	select {
	case <-firstDiscoveryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first discovery did not start")
	}
	first.Stop("restart test")

	second := New(Options{
		Config: cfg, Logger: &testLogger{}, DeferRecovery: true,
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	t.Cleanup(func() { second.Stop("test cleanup") })
	resumed := make(chan struct{})
	second.Services().Projects.ListWorktrees = func(context.Context, string) ([]projects.WorktreeListEntry, error) {
		close(resumed)
		return nil, nil
	}
	if err := second.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("second CompleteStartup() error = %v", err)
	}
	select {
	case <-resumed:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown-canceled discovery was not resumed after restart")
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
