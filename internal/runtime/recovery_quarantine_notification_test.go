package runtime

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// seedQuarantinableFixerLoop seeds one fixer loop whose in-flight execution
// startup recovery cannot prove dead, which is what parks it.
func seedQuarantinableFixerLoop(t *testing.T, repos *storage.Repositories, workingDir, projectID string, seq int64, prNumber int64, pid int64, nowISO, oldISO string) {
	t.Helper()

	loopID := loopIDForSeq(seq)
	runID := "run_quarantine_" + loopIDForSeq(seq)
	queueID := "queue_quarantine_" + loopIDForSeq(seq)
	executionID := "agent_quarantine_" + loopIDForSeq(seq)
	repo := "MumuTW/looper"
	targetID := "pr:MumuTW/looper:" + strconv.FormatInt(prNumber, 10)

	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: seq, ProjectID: projectID, Type: "fixer", TargetType: "pull_request",
		TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert(%d) error = %v", seq, err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"),
		StartedAt: oldISO, LastHeartbeatAt: &oldISO, CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Runs.Upsert(%d) error = %v", seq, err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "fixer", TargetType: "pull_request", TargetID: targetID,
		Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer:" + loopID, Priority: storage.QueuePriorityFixer, Status: "running",
		AvailableAt: oldISO, Attempts: 1, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(oldISO),
		StartedAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%d) error = %v", seq, err)
	}
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
		PID: &pid, CommandJSON: stringPtr(`{"command":"codex","args":["exec"]}`), CWD: stringPtr(workingDir),
		HeartbeatCount: 0, LastHeartbeatAt: &nowISO, StartedAt: oldISO, CreatedAt: oldISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert(%d) error = %v", seq, err)
	}
}

func loopIDForSeq(seq int64) string { return "loop_quarantine_" + strconv.FormatInt(seq, 10) }

// Contract: a recovery pass that parks work reports it exactly once, naming
// every loop it parked and the command that clears each one.
func TestStartupRecoveryQuarantineNotifiesOncePerPass(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.July, 30, 8, 18, 12, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)
	oldISO := formatJavaScriptISOString(startedAt.Add(-time.Hour))

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	projectID := "project_quarantine_notify"
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	seedQuarantinableFixerLoop(t, seedRepos, workingDir, projectID, 35, 126, 7771, nowISO, oldISO)
	seedQuarantinableFixerLoop(t, seedRepos, workingDir, projectID, 36, 125, 7772, nowISO, oldISO)
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed close error = %v", err)
	}

	rt := New(Options{
		Config:             cfg,
		Logger:             &testLogger{},
		Now:                func() time.Time { return startedAt },
		ReadProcessCommand: func(context.Context, int) (string, error) { return "", nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if quarantined := rt.RecoverySummary().OrphanAgentCleanup.QuarantinedCount; quarantined != 2 {
		t.Fatalf("QuarantinedCount = %d, want 2", quarantined)
	}

	notifications := listQuarantineNotifications(t, rt.Services().Repositories)
	if len(notifications) != 1 {
		t.Fatalf("in_app quarantine notifications = %d (%#v), want exactly 1 per recovery pass", len(notifications), notifications)
	}
	notification := notifications[0]
	if notification.Level != "warn" {
		t.Fatalf("notification.Level = %q, want warn", notification.Level)
	}
	for _, want := range []string{"loop 35", "MumuTW/looper#126", "looper retry 35", "loop 36", "MumuTW/looper#125", "looper retry 36", "fixer"} {
		if !strings.Contains(notification.Body, want) {
			t.Fatalf("notification.Body = %q, want it to contain %q", notification.Body, want)
		}
	}
}

// Contract: a recovery pass with nothing to park says nothing.
func TestStartupRecoveryWithoutQuarantineSendsNoNotification(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	startedAt := time.Date(2026, time.July, 30, 8, 18, 12, 0, time.UTC)
	nowISO := formatJavaScriptISOString(startedAt)

	seedCoordinator := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	seedRepos := storage.NewRepositories(seedCoordinator.DB())
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_quiet", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := seedCoordinator.Close(); err != nil {
		t.Fatalf("seed close error = %v", err)
	}

	rt := New(Options{Config: cfg, Logger: &testLogger{}, Now: func() time.Time { return startedAt }})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })

	if quarantined := rt.RecoverySummary().OrphanAgentCleanup.QuarantinedCount; quarantined != 0 {
		t.Fatalf("QuarantinedCount = %d, want 0", quarantined)
	}
	if notifications := listQuarantineNotifications(t, rt.Services().Repositories); len(notifications) != 0 {
		t.Fatalf("quarantine notifications = %#v, want none", notifications)
	}
}

func listQuarantineNotifications(t *testing.T, repositories *storage.Repositories) []storage.NotificationRecord {
	t.Helper()

	records, err := repositories.Notifications.List(context.Background(), 100)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	matched := make([]storage.NotificationRecord, 0, len(records))
	for _, record := range records {
		if record.Channel != "in_app" || record.DedupeKey == nil {
			continue
		}
		if strings.HasPrefix(*record.DedupeKey, "runtime.recovery.quarantined:") {
			matched = append(matched, record)
		}
	}
	return matched
}
