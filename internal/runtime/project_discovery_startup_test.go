package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	networkclient "github.com/MumuTW/looper/internal/network/client"
	"github.com/MumuTW/looper/internal/projects"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/webhookforward"
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

func TestRuntimeDiscoveryResumeFailureStopsStartedResources(t *testing.T) {
	t.Parallel()

	network := &startupRollbackNetworkManager{started: make(chan struct{}), stopped: make(chan struct{})}
	forwarder := &startupRollbackWebhookForwarder{canceled: make(chan struct{}), closed: make(chan struct{})}
	rt, _ := startRuntimeWithPendingDiscovery(t, "rollback", func(options *Options) {
		options.WebhookForwarder = forwarder
	})
	// Kept white-box under #121: networkManager is wired inside start() from
	// runtime internals and typed as an unexported interface — exporting a
	// construction seam for one rollback-observation test would be a prop.
	// The swap installs the observable fake and stops the real manager the
	// boot started, so the rollback assertion below sees only the fake.
	rt.mu.Lock()
	previousNetwork := rt.networkManager
	rt.networkManager = network
	rt.mu.Unlock()
	previousNetwork.Stop()

	startupErr := errors.New("resume discovery failed")
	var schedulerDone, cleanupDone <-chan struct{}
	rt.resumeProjectDiscoveries = func(context.Context, *projects.Service) error {
		rt.mu.RLock()
		schedulerDone = rt.schedulerDone
		cleanupDone = rt.worktreeCleanupDone
		rt.mu.RUnlock()
		return startupErr
	}

	if err := rt.CompleteStartup(context.Background()); !errors.Is(err, startupErr) {
		t.Fatalf("CompleteStartup() error = %v, want %v", err, startupErr)
	}
	for name, done := range map[string]<-chan struct{}{
		"network start":  network.started,
		"network stop":   network.stopped,
		"scheduler stop": schedulerDone,
		"cleanup stop":   cleanupDone,
		"webhook cancel": forwarder.canceled,
		"webhook close":  forwarder.closed,
		"runtime stop":   rt.shutdownCh,
	} {
		if done == nil {
			t.Fatalf("%s channel = nil, want resource assembled before resume failure", name)
		}
		select {
		case <-done:
		default:
			t.Fatalf("%s was not completed before CompleteStartup returned", name)
		}
	}
	if services := rt.Services(); services != (Services{}) {
		t.Fatalf("Services() = %#v, want released services after startup rollback", services)
	}
	if state := rt.AdmissionState(); state != AdmissionStopping {
		t.Fatalf("AdmissionState() = %q, want stopping", state)
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

func startRuntimeWithPendingDiscovery(t *testing.T, projectID string, customize ...func(*Options)) (*Runtime, *projects.Service) {
	t.Helper()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	options := Options{
		Config:           cfg,
		Logger:           &testLogger{},
		DeferRecovery:    true,
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	}
	for _, apply := range customize {
		apply(&options)
	}
	rt := New(options)
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

type startupRollbackNetworkManager struct {
	started chan struct{}
	stopped chan struct{}
}

func (m *startupRollbackNetworkManager) Start(context.Context) error {
	close(m.started)
	return nil
}
func (m *startupRollbackNetworkManager) Stop() { close(m.stopped) }
func (*startupRollbackNetworkManager) Status() networkclient.Status {
	return networkclient.Status{}
}
func (*startupRollbackNetworkManager) UpdateConfig(config.Config) {}

type startupRollbackWebhookForwarder struct {
	canceled chan struct{}
	closed   chan struct{}
}

func (*startupRollbackWebhookForwarder) Forward(context.Context, webhookforward.DeliveryRequest) (webhookforward.ForwardResult, error) {
	return webhookforward.ForwardResult{}, nil
}
func (*startupRollbackWebhookForwarder) Stats() webhookforward.Stats { return webhookforward.Stats{} }
func (f *startupRollbackWebhookForwarder) CancelExecute()            { close(f.canceled) }
func (f *startupRollbackWebhookForwarder) Close()                    { close(f.closed) }
