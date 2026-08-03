package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestCompleteStartupVerifyHoldDefersRecoveryMutations(t *testing.T) {
	t.Setenv("LOOPER_UPGRADE_VERIFY_HOLD", "1")

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	oldISO := formatJavaScriptISOString(startedAt.Add(-time.Minute))

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	if acquired, err := seedRepos.Locks.Acquire(context.Background(), storage.LockRecord{
		Key: "verify-hold-expired-lock", Owner: "old-daemon", ExpiresAt: oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil || !acquired {
		t.Fatalf("seed expired lock = (%v, %t)", err, acquired)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed coordinator Close() error = %v", err)
	}

	rt := New(Options{
		Config:        cfg,
		Logger:        &testLogger{},
		Now:           func() time.Time { return startedAt },
		DeferRecovery: true,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	if err := rt.CompleteStartup(context.Background()); err != nil {
		t.Fatalf("CompleteStartup() error = %v", err)
	}
	if got := rt.AdmissionState(); got != AdmissionDraining {
		t.Fatalf("AdmissionState() = %q, want draining", got)
	}
	if got := rt.RecoverySummary(); got != createEmptyRecoverySummary() {
		t.Fatalf("RecoverySummary() = %#v, want empty summary while held", got)
	}

	services := rt.Services()
	lock, err := services.Repositories.Locks.Get(context.Background(), "verify-hold-expired-lock")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock == nil {
		t.Fatal("expired lock was removed under verify hold")
	}
	events, err := services.Repositories.Events.List(context.Background(), 20)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if containsEventType(events, "looperd.recovery.lock_released") || containsEventType(events, "looperd.started") {
		t.Fatalf("events = %#v, want no startup recovery or started event while held", events)
	}
}
