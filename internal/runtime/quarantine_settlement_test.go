package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

// Contract for #150: settlement is asymmetric. Absence of the recorded process
// is conclusive; presence never is. Leader absence alone is not authority
// though: a background descendant that outlived the leader can keep mutating
// the worktree, so the leader's whole process group must be gone first
// (ADR-0015 R8).
func TestRecordedProcessVerifiablyGoneIsAsymmetric(t *testing.T) {
	t.Parallel()

	livePID := int64(4242)
	identity := stringPtr(`{"processIdentity":{"startTime":2201,"bootId":"boot-a"}}`)
	// Deterministic descendant probe: by default the group is empty so the
	// leader-absence cases settle; individual cases override it to simulate a
	// live descendant or an uncertain probe.
	noDescendants := func(int) (bool, bool) { return false, true }

	for _, tc := range []struct {
		name      string
		execution storage.AgentExecutionRecord
		command   func(context.Context, int) (string, error)
		start     func(context.Context, int) (int64, error)
		bootID    func(context.Context, int) (string, error)
		groupLive func(int) (bool, bool)
		want      processAbsenceReason
	}{
		{
			name:      "no process holds the pid and group empty",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "", nil },
			want:      processAbsenceRecordedProcessAbsent,
		},
		{
			// Rows written before process identity existed still settle when the
			// PID holds nothing: there is no other process to confuse ours with.
			name:      "no recorded identity and no process holds the pid",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`)},
			command:   func(context.Context, int) (string, error) { return "", nil },
			want:      processAbsenceRecordedProcessAbsent,
		},
		{
			name:      "pid reused by a different birth and group empty",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "codex exec", nil },
			start:     func(context.Context, int) (int64, error) { return 9999, nil },
			want:      processAbsenceRecordedProcessReplaced,
		},
		{
			name:      "recorded birth still matches the live process",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "codex exec", nil },
			start:     func(context.Context, int) (int64, error) { return 2201, nil },
			want:      processAbsenceRecordedProcessLive,
		},
		{
			// Present but unidentifiable: settling this would settle something we
			// cannot identify, which is exactly what the asymmetry forbids.
			name:      "no recorded identity while the pid is live",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`)},
			command:   func(context.Context, int) (string, error) { return "codex exec", nil },
			want:      processAbsenceIdentityUnavailable,
		},
		{
			name:      "probe failure",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "", errors.New("ps unavailable") },
			want:      processAbsenceIdentityUnavailable,
		},
		{
			name:      "no recorded pid",
			execution: storage.AgentExecutionRecord{CommandJSON: stringPtr(`{"command":"codex"}`)},
			command:   func(context.Context, int) (string, error) { return "", nil },
			want:      processAbsenceNoRecordedPID,
		},
		{
			// Leader gone but a descendant still lives in its process group: leader
			// exit alone is not containment proof, so the debt stays.
			name:      "leader gone but descendant live blocks settlement",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "", nil },
			groupLive: func(int) (bool, bool) { return true, true },
			want:      processAbsenceDescendantsLive,
		},
		{
			// Leader gone but group liveness uncertain: asymmetry keeps the debt.
			name:      "leader gone but group liveness uncertain blocks settlement",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "", nil },
			groupLive: func(int) (bool, bool) { return false, false },
			want:      processAbsenceDescendantsUncertain,
		},
		{
			// PID reused by a different birth but a descendant of ours still lives
			// in the original group: replacement alone is not containment proof.
			name:      "pid reused but descendant live blocks settlement",
			execution: storage.AgentExecutionRecord{PID: &livePID, CommandJSON: stringPtr(`{"command":"codex"}`), MetadataJSON: identity},
			command:   func(context.Context, int) (string, error) { return "codex exec", nil },
			start:     func(context.Context, int) (int64, error) { return 9999, nil },
			groupLive: func(int) (bool, bool) { return true, true },
			want:      processAbsenceDescendantsLive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			start := tc.start
			if start == nil {
				start = func(context.Context, int) (int64, error) { return 0, errors.New("unexpected start read") }
			}
			bootID := tc.bootID
			if bootID == nil {
				bootID = func(context.Context, int) (string, error) { return "boot-a", nil }
			}
			groupLive := tc.groupLive
			if groupLive == nil {
				groupLive = noDescendants
			}
			rt := &Runtime{readProcessCommand: tc.command, readProcessStart: start, readProcessBootID: bootID, readProcessGroupLive: groupLive}
			got := rt.recordedProcessVerifiablyGone(context.Background(), tc.execution)
			if got != tc.want {
				t.Fatalf("recordedProcessVerifiablyGone() = %q, want %q", got, tc.want)
			}
			if got.provesAbsence() != (tc.want == processAbsenceRecordedProcessAbsent || tc.want == processAbsenceRecordedProcessReplaced) {
				t.Fatalf("provesAbsence() disagrees with reason %q", got)
			}
		})
	}
}

// Terminal rows (including the status='timeout' rows seen in #150) are already
// outside the active set, so settlement has nothing to do and must not rewrite
// an immutable terminal observation.
func TestSettleQuarantinedExecutionSkipsTerminalRows(t *testing.T) {
	t.Parallel()

	pid := int64(4242)
	rt := &Runtime{
		readProcessCommand: func(context.Context, int) (string, error) {
			t.Fatal("probed a terminal row")
			return "", nil
		},
	}
	for _, status := range []string{"timeout", "failed", "completed", "killed"} {
		settled, events, err := rt.settleQuarantinedExecution(context.Background(), nil, storage.AgentExecutionRecord{
			ID: "execution_terminal", Status: status, PID: &pid,
		}, "2026-07-30T12:00:00.000Z")
		if settled || events != 0 || err != nil {
			t.Fatalf("settleQuarantinedExecution(%q) = (%v, %d, %v), want no-op", status, settled, events, err)
		}
	}
}

// newSettlementTestRuntime boots a manual-reconcile runtime with a real
// coordinator so settlement commits through a transaction, and returns its
// repositories plus the configured process probes.
func newSettlementTestRuntime(t *testing.T, processGroupLive func(int) (bool, bool)) (*Runtime, *storage.Repositories) {
	t.Helper()
	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt := newManualReconcileRuntime(Options{
		Config: cfg,
		Logger: &testLogger{},
		Now:    func() time.Time { return now },
		ReadProcessCommand: func(context.Context, int) (string, error) {
			return "", nil // recorded leader PID holds no process
		},
		ReadProcessGroupLive: processGroupLive,
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	return rt, rt.Services().Repositories
}

func seedQuarantineEvent(t *testing.T, repos *storage.Repositories, executionID string, nowISO string) {
	t.Helper()
	if err := repos.Events.Append(context.Background(), storage.EventLogRecord{
		ID:         newRuntimeEventID(),
		EventType:  recoveryExecutionQuarantinedEventType,
		EntityType: stringPtr("agent_execution"),
		EntityID:   stringPtr(executionID),
		CreatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}
}

// Contract: a live descendant in the leader's process group blocks settlement.
// Leader exit alone is not containment proof (ADR-0015 R8), so the row stays
// active and the debt is not retired while an orphan may still mutate the
// worktree.
func TestSettleQuarantinedExecutionBlockedByLiveDescendant(t *testing.T) {
	t.Parallel()

	rt, repos := newSettlementTestRuntime(t, func(int) (bool, bool) { return true, true })
	nowISO := formatJavaScriptISOString(rt.now())
	pid := int64(5555)
	executionID := "execution_descendant_live"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, Vendor: "codex", Status: "running", PID: &pid,
		CommandJSON: stringPtr(`{"command":"codex"}`), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	execution, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	settled, events, err := rt.settleQuarantinedExecution(context.Background(), repos, *execution, nowISO)
	if err != nil {
		t.Fatalf("settleQuarantinedExecution() error = %v", err)
	}
	if settled || events != 0 {
		t.Fatalf("settleQuarantinedExecution() = (%v, %d, nil), want no settlement while descendant live", settled, events)
	}
	row, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if row == nil || row.Status != "running" {
		t.Fatalf("execution = %#v, want still running (descendant containment not proven)", row)
	}
}

// Contract: settlement preserves native-resume eligibility. When native resume
// is enabled and the row captured a session, the settled terminal row must read
// native_resume/pending so a later operator retry resumes the captured agent
// conversation instead of silently restarting from the checkpoint.
func TestSettleQuarantinedExecutionPreservesNativeResumeEligibility(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	backupDir := filepath.Join(workingDir, "backups")
	cfg.Storage.BackupDir = &backupDir
	cfg.Agent.NativeResume.Enabled = true
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	rt := newManualReconcileRuntime(Options{
		Config:               cfg,
		Logger:               &testLogger{},
		Now:                  func() time.Time { return now },
		ReadProcessCommand:   func(context.Context, int) (string, error) { return "", nil },
		ReadProcessGroupLive: func(int) (bool, bool) { return false, true },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { rt.Stop("test cleanup") })
	repos := rt.Services().Repositories
	nowISO := formatJavaScriptISOString(now)
	pid := int64(6666)
	sessionID := "sess-native-123"
	executionID := "execution_native_resume"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, Vendor: "codex", Status: "running", PID: &pid,
		CommandJSON: stringPtr(`{"command":"codex"}`), NativeSessionID: &sessionID,
		NativeResumeStatus: stringPtr("unavailable"),
		StartedAt:          nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	execution, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	settled, _, err := rt.settleQuarantinedExecution(context.Background(), repos, *execution, nowISO)
	if err != nil || !settled {
		t.Fatalf("settleQuarantinedExecution() = (%v, _, %v), want settled", settled, err)
	}
	row, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if row == nil || row.Status != "failed" {
		t.Fatalf("execution = %#v, want settled terminal row", row)
	}
	if row.NativeResumeMode == nil || *row.NativeResumeMode != "native_resume" || row.NativeResumeStatus == nil || *row.NativeResumeStatus != "pending" {
		t.Fatalf("native resume = (mode=%v, status=%v), want native_resume/pending so retry resumes the captured session", row.NativeResumeMode, row.NativeResumeStatus)
	}
}

// Contract: the terminal status transition and the settlement audit event are
// committed together. A successful settlement leaves both the failed row and the
// looperd.recovery.execution_quarantine_settled event durably recorded.
func TestSettleQuarantinedExecutionCommitsRowAndEventAtomically(t *testing.T) {
	t.Parallel()

	rt, repos := newSettlementTestRuntime(t, func(int) (bool, bool) { return false, true })
	nowISO := formatJavaScriptISOString(rt.now())
	pid := int64(7777)
	executionID := "execution_atomic_settle"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, Vendor: "codex", Status: "running", PID: &pid,
		CommandJSON: stringPtr(`{"command":"codex"}`), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	execution, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	settled, events, err := rt.settleQuarantinedExecution(context.Background(), repos, *execution, nowISO)
	if err != nil || !settled || events != 1 {
		t.Fatalf("settleQuarantinedExecution() = (%v, %d, %v), want settled with one event", settled, events, err)
	}
	row, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if row == nil || row.Status != "failed" || row.EndedAt == nil {
		t.Fatalf("execution = %#v, want durably terminal row", row)
	}
	evs, err := repos.Events.ListByEntity(context.Background(), "agent_execution", executionID)
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	if !containsEventType(evs, quarantineSettledEventType) {
		t.Fatalf("events = %#v, want the settlement audit event committed alongside the terminal row", evs)
	}
}

// Contract for the orphan pass (#150): a dead quarantined execution with no run
// ID is never visited by the running-run loop, yet CountOutstandingQuarantineDebt
// counts it. The live/manual reconcile must settle it so the daemon can leave
// degraded.
func TestReconcileSettlesOrphanedQuarantinedExecutionWithoutRun(t *testing.T) {
	t.Parallel()

	rt, repos := newSettlementTestRuntime(t, func(int) (bool, bool) { return false, true })
	nowISO := formatJavaScriptISOString(rt.now())
	pid := int64(8888)
	executionID := "execution_orphan_no_run"
	if err := repos.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID: executionID, Vendor: "codex", Status: "running", PID: &pid,
		CommandJSON: stringPtr(`{"command":"codex"}`), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	seedQuarantineEvent(t, repos, executionID, nowISO)

	debtBefore, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debtBefore.QuarantinedActiveExecutions != 1 {
		t.Fatalf("debt before = %#v, want 1 quarantined active execution", debtBefore)
	}

	summary, err := rt.ReconcileStaleRunningRuns(context.Background())
	if err != nil {
		t.Fatalf("ReconcileStaleRunningRuns() error = %v", err)
	}
	if summary.SettledQuarantinedExecutions != 1 {
		t.Fatalf("summary = %#v, want the orphaned quarantined execution settled", summary)
	}

	debtAfter, err := CountOutstandingQuarantineDebt(context.Background(), repos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debtAfter.QuarantinedActiveExecutions != 0 {
		t.Fatalf("debt after = %#v, want 0 after orphan settlement", debtAfter)
	}
	row, err := repos.AgentExecutions.GetByID(context.Background(), executionID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if row == nil || row.Status != "failed" {
		t.Fatalf("execution = %#v, want settled terminal row", row)
	}
}
