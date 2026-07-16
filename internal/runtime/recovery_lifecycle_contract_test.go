package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	shellinfra "github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func TestStartupRecoveryReapsTermResistantOrphanBeforeSchedulerTick(t *testing.T) {
	workingDir := t.TempDir()
	script := filepath.Join(workingDir, "term-resistant-orphan.sh")
	readyFile := filepath.Join(workingDir, "ready")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\n: > \"$READY_FILE\"\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatalf("write orphan script: %v", err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(), "READY_FILE="+readyFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orphan: %v", err)
	}
	pid := cmd.Process.Pid
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-reaped:
		case <-time.After(time.Second):
		}
	})
	waitForRecoveryContractFile(t, readyFile)

	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Name: "Project", RepoPath: workingDir}}
	now := time.Now().UTC()
	nowISO := formatJavaScriptISOString(now.Add(-time.Hour))
	seed := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	repos := storage.NewRepositories(seed.DB())
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: workingDir, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_orphan"
	runID := "run_orphan"
	projectID := "project_1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	commandJSON, err := json.Marshal(map[string]any{"command": "/bin/sh", "args": []string{script}})
	if err != nil {
		t.Fatalf("marshal command metadata: %v", err)
	}
	pid64 := int64(pid)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "exec_orphan", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running", PID: &pid64, CommandJSON: stringPtr(string(commandJSON)), CWD: &workingDir, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed coordinator close: %v", err)
	}

	ticked := make(chan struct{}, 1)
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error {
			if recoveryProcessGroupExists(pid) {
				t.Errorf("scheduler ticked while orphan process group %d was still live", pid)
			}
			select {
			case ticked <- struct{}{}:
			default:
			}
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not start after recovery")
	}
	select {
	case <-reaped:
	default:
		t.Fatal("recovery made scheduler visible before orphan was reaped")
	}
	execution, err := rt.Services().Repositories.AgentExecutions.GetByID(context.Background(), "exec_orphan")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if execution == nil || execution.Status != "killed" {
		t.Fatalf("recovered execution = %#v, want killed", execution)
	}
}

func TestStartupRecoveryReapsDescendantAfterLeaderExitsOnTERMBeforeSchedulerTick(t *testing.T) {
	workingDir := t.TempDir()
	script := filepath.Join(workingDir, "leader-exits-orphan.sh")
	descendantPIDFile := filepath.Join(workingDir, "descendant.pid")
	readyFile := filepath.Join(workingDir, "ready")
	scriptBody := "#!/bin/sh\n" +
		"/bin/sh -c 'trap \"\" TERM HUP; echo $$ > \"$DESCENDANT_PID_FILE\"; while :; do sleep 1; done' &\n" +
		"while [ ! -s \"$DESCENDANT_PID_FILE\" ]; do sleep 0.01; done\n" +
		": > \"$READY_FILE\"\n" +
		"wait\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write orphan script: %v", err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(), "READY_FILE="+readyFile, "DESCENDANT_PID_FILE="+descendantPIDFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orphan: %v", err)
	}
	leaderPID := cmd.Process.Pid
	leaderReaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(leaderReaped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		select {
		case <-leaderReaped:
		case <-time.After(time.Second):
		}
	})
	waitForRecoveryContractFile(t, readyFile)
	rawDescendantPID, err := os.ReadFile(descendantPIDFile)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(rawDescendantPID)))
	if err != nil || descendantPID <= 0 {
		t.Fatalf("parse descendant pid %q: %v", rawDescendantPID, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })

	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_descendant", Name: "Project", RepoPath: workingDir}}
	nowISO := formatJavaScriptISOString(time.Now().UTC().Add(-time.Hour))
	seed := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	repos := storage.NewRepositories(seed.DB())
	projectID := "project_descendant"
	loopID := "loop_descendant"
	runID := "run_descendant"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: workingDir, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	commandJSON, err := json.Marshal(map[string]any{"command": "/bin/sh", "args": []string{script}})
	if err != nil {
		t.Fatalf("marshal command metadata: %v", err)
	}
	leaderPID64 := int64(leaderPID)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "exec_descendant", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running", PID: &leaderPID64, CommandJSON: stringPtr(string(commandJSON)), CWD: &workingDir, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed coordinator close: %v", err)
	}

	ticked := make(chan struct{}, 1)
	rt := New(Options{
		Config: cfg,
		Logger: &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error {
			if recoveryProcessGroupExists(leaderPID) {
				t.Errorf("scheduler ticked while orphan process group %d was still live", leaderPID)
			}
			if err := syscall.Kill(descendantPID, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
				t.Errorf("scheduler ticked while orphan descendant %d was still live: %v", descendantPID, err)
			}
			select {
			case ticked <- struct{}{}:
			default:
			}
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not start after descendant process-group recovery")
	}
	select {
	case <-leaderReaped:
	default:
		t.Fatal("recovery made scheduler visible before orphan leader was reaped")
	}
	if recoveryProcessGroupExists(leaderPID) {
		t.Fatalf("orphan process group %d survived startup recovery", leaderPID)
	}
}

// TestStartupRecoveryProtectsLeaderlessLiveProcessGroupAsUncertain covers the
// restart window where the agent leader has already exited but a process group
// under the persisted PGID is still live. Without a live identity-matched
// leader, recovery must not signal the group (PGID reuse risk) and must protect
// the run as uncertain instead of marking the execution terminal.
func TestStartupRecoveryProtectsLeaderlessLiveProcessGroupAsUncertain(t *testing.T) {
	workingDir := t.TempDir()
	script := filepath.Join(workingDir, "leader-already-exited-orphan.sh")
	descendantPIDFile := filepath.Join(workingDir, "descendant.pid")
	readyFile := filepath.Join(workingDir, "ready")
	// Leader exits immediately after spawning a TERM-resistant child that keeps
	// the process group alive under the leader's PGID.
	scriptBody := "#!/bin/sh\n" +
		"/bin/sh -c 'trap \"\" TERM HUP; echo $$ > \"$DESCENDANT_PID_FILE\"; : > \"$READY_FILE\"; while :; do sleep 1; done' &\n" +
		"while [ ! -s \"$DESCENDANT_PID_FILE\" ]; do sleep 0.01; done\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write orphan script: %v", err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(), "READY_FILE="+readyFile, "DESCENDANT_PID_FILE="+descendantPIDFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orphan: %v", err)
	}
	leaderPID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("leader Wait() error = %v", err)
	}
	waitForRecoveryContractFile(t, readyFile)
	rawDescendantPID, err := os.ReadFile(descendantPIDFile)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(string(rawDescendantPID)))
	if err != nil || descendantPID <= 0 {
		t.Fatalf("parse descendant pid %q: %v", rawDescendantPID, err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
	})
	if err := syscall.Kill(descendantPID, 0); err != nil {
		t.Fatalf("descendant %d not live before recovery: %v", descendantPID, err)
	}
	if !recoveryProcessGroupExists(leaderPID) {
		t.Fatalf("process group %d not live before recovery", leaderPID)
	}
	// Leader must already be gone so executionMatchesProcess reports running=false.
	if live, err := shellinfra.ProcessRunnable(leaderPID); err != nil {
		t.Fatalf("ProcessRunnable(leader) error = %v", err)
	} else if live {
		t.Fatalf("leader pid %d still runnable; test requires leader already exited", leaderPID)
	}

	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_leader_gone", Name: "Project", RepoPath: workingDir}}
	nowISO := formatJavaScriptISOString(time.Now().UTC().Add(-time.Hour))
	seed := openMigratedCoordinator(t, cfg.Storage.DBPath, backupDir)
	repos := storage.NewRepositories(seed.DB())
	projectID := "project_leader_gone"
	loopID := "loop_leader_gone"
	runID := "run_leader_gone"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: workingDir, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CurrentStep: stringPtr("execute"), StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	commandJSON, err := json.Marshal(map[string]any{"command": "/bin/sh", "args": []string{script}})
	if err != nil {
		t.Fatalf("marshal command metadata: %v", err)
	}
	leaderPID64 := int64(leaderPID)
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{ID: "exec_leader_gone", ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running", PID: &leaderPID64, CommandJSON: stringPtr(string(commandJSON)), CWD: &workingDir, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed coordinator close: %v", err)
	}

	logger := &testLogger{}
	ticked := make(chan struct{}, 1)
	rt := New(Options{
		Config: cfg,
		Logger: logger,
		RunSchedulerTick: func(context.Context, Services) error {
			select {
			case ticked <- struct{}{}:
			default:
			}
			return nil
		},
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Runtime.Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	select {
	case <-ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not start after leaderless uncertain recovery protection")
	}
	// Must not signal a leaderless group: descendants remain live under the PGID.
	if !recoveryProcessGroupExists(leaderPID) {
		t.Fatalf("process group %d was reaped; want leaderless group left unsignaled", leaderPID)
	}
	if err := syscall.Kill(descendantPID, 0); err != nil {
		t.Fatalf("descendant %d was killed during recovery: %v", descendantPID, err)
	}
	execution, err := rt.Services().Repositories.AgentExecutions.GetByID(context.Background(), "exec_leader_gone")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if execution == nil || execution.Status != "running" || execution.EndedAt != nil {
		t.Fatalf("recovered execution = %#v, want still running (uncertain, not killed)", execution)
	}
	run, err := rt.Services().Repositories.Runs.GetByID(context.Background(), runID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "running" || run.EndedAt != nil {
		t.Fatalf("run = %#v, want preserved running run for uncertain leaderless group", run)
	}
	events, err := rt.Services().Repositories.Events.ListByEntity(context.Background(), "agent_execution", "exec_leader_gone")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if !containsEventType(events, "looperd.recovery.process_identity_uncertain") {
		t.Fatalf("events = %#v, want uncertain-identity event for leaderless live group", events)
	}
	if recovery := rt.RecoverySummary(); recovery.OrphanAgentCleanup.CleanedCount != 0 {
		t.Fatalf("RecoverySummary() = %#v, want cleanedCount=0 for leaderless uncertain group", recovery)
	}
	if !logger.containsMessage("recovery skipped due to uncertain process identity") {
		t.Fatalf("logger entries = %#v, want uncertain process identity warning", logger.messages())
	}
}

func waitForRecoveryContractFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process readiness file %s was not written", path)
}

func recoveryProcessGroupExists(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

// TestRecoveredProcessGroupExitedTreatsZombieOnlyGroupAsExited locks the recovery
// barrier to the non-zombie probe: after SIGKILL, kill(-pgid, 0) can still
// succeed for unreaped zombies on Linux, but recovery must treat the group as
// exited so orphan cleanup marks the execution killed instead of uncertain.
func TestRecoveredProcessGroupExitedTreatsZombieOnlyGroupAsExited(t *testing.T) {
	// Leaf process only: a shell -c "sleep …" can leave a live sleep sibling in
	// the group after the shell is killed (dash forks), which is not the
	// zombie-only case under test.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL process group: %v", err)
	}
	// Wait until the leader is no longer runnable without reaping it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		live, err := shellinfra.ProcessRunnable(pgid)
		if err != nil {
			t.Fatalf("ProcessRunnable() settle error = %v", err)
		}
		if !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d remained runnable after SIGKILL", pgid)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// kill(0) may still succeed (or return EPERM) while the process is a zombie.
	if err := syscall.Kill(pgid, 0); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill(%d,0) error = %v", pgid, err)
	}

	rt := New(Options{Logger: &testLogger{}})
	startedAt := time.Now()
	exited, err := rt.recoveredProcessGroupExited(pgid)
	if err != nil {
		t.Fatalf("recoveredProcessGroupExited() error = %v", err)
	}
	if !exited {
		t.Fatalf("recoveredProcessGroupExited(%d) = false, want true for zombie-only group", pgid)
	}
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		t.Fatalf("recoveredProcessGroupExited() elapsed = %s, want immediate zombie-only success", elapsed)
	}
}
