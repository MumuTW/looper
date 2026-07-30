package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	// quarantineSettledEventType records that a parked agent_executions row was
	// finalized because its recorded process is verifiably gone. The original
	// looperd.recovery.execution_quarantined event is never removed: settlement
	// adds a second entry so the history reads "quarantined, then settled".
	quarantineSettledEventType = "looperd.recovery.execution_quarantine_settled"

	// quarantineSettledStatus is an existing terminal agent_executions status.
	// Settlement deliberately does not introduce a new status value: the row is
	// terminal, and the event log carries why.
	quarantineSettledStatus = "failed"

	quarantineSettledMessage = "Quarantined agent execution settled: the recorded process is no longer present. looperd did not signal it."
)

// processAbsenceReason is the machine-oriented outcome of comparing an
// execution's recorded process birth against the live process table.
type processAbsenceReason string

const (
	// processAbsenceNoRecordedPID means the row never recorded a PID, so there is
	// nothing to compare. Not settleable.
	processAbsenceNoRecordedPID processAbsenceReason = "no_recorded_pid"
	// processAbsenceIdentityUnavailable means the probe failed, or the live PID is
	// occupied by a process we cannot identify because the row predates recorded
	// process identity. Presence without identity is never conclusive.
	processAbsenceIdentityUnavailable processAbsenceReason = "process_identity_unavailable"
	// processAbsenceRecordedProcessLive means the recorded birth still matches the
	// live process. Conclusively still ours: real, current debt.
	processAbsenceRecordedProcessLive processAbsenceReason = "recorded_process_live"
	// processAbsenceRecordedProcessAbsent means no process holds the PID at all.
	processAbsenceRecordedProcessAbsent processAbsenceReason = "recorded_process_absent"
	// processAbsenceRecordedProcessReplaced means the PID is held by a process
	// with a different birth token, which proves ours exited.
	processAbsenceRecordedProcessReplaced processAbsenceReason = "recorded_process_replaced"
	// processAbsenceDescendantsLive means the recorded leader process is gone but
	// its process group still has a live member. Agents spawn as their own process
	// group leader (Setpgid, so pgid == pid), and a background descendant that
	// outlived the leader can keep mutating the worktree. Leader exit alone is not
	// containment proof (ADR-0015 R8), so this stays as debt rather than settling.
	processAbsenceDescendantsLive processAbsenceReason = "descendants_live"
	// processAbsenceDescendantsUncertain means the recorded leader process is gone
	// but the process group liveness could not be determined. Settlement is
	// asymmetric: uncertainty leaves the debt standing.
	processAbsenceDescendantsUncertain processAbsenceReason = "descendants_uncertain"
)

func (reason processAbsenceReason) provesAbsence() bool {
	return reason == processAbsenceRecordedProcessAbsent || reason == processAbsenceRecordedProcessReplaced
}

// recordedProcessVerifiablyGone compares an execution's durable process birth
// against the live process table.
//
// Absence is conclusive, presence is not — but only after the leader's whole
// process group is gone. Agents spawn as their own process-group leader
// (Setpgid, so pgid == pid), and a background descendant that outlived the
// leader can keep mutating the worktree. ADR-0015 R8 keeps leader-exit-only
// evidence uncertain until descendant/containment proof exists, so an empty PID
// probe or a reused PID is not settlement authority while the group still has a
// live member.
//
//   - no process holds the PID and the group is empty -> the recorded process
//     is gone, whether or not the row carries a recorded birth token (there is
//     nothing to confuse it with, so rows written before process identity
//     existed still settle);
//   - the PID is held by a process with a different birth and the group is
//     empty -> ours exited and the PID was reused;
//   - the leader is gone but the group still has a live member (or group
//     liveness is uncertain) -> descendants may remain; leave the debt standing;
//   - anything else (identity matches, identity missing while the PID is live,
//     probe error) leaves the execution counted as debt.
//
// This never upgrades a durable observation to ContainmentConfirmedDead. That
// class authorizes requeue and overlapping work and still requires containment
// proof; settlement only finalizes a row whose work is already parked.
func (r *Runtime) recordedProcessVerifiablyGone(ctx context.Context, execution storage.AgentExecutionRecord) processAbsenceReason {
	pid := 0
	if execution.PID != nil && *execution.PID > 0 {
		pid = int(*execution.PID)
	}
	if pid <= 0 {
		return processAbsenceNoRecordedPID
	}
	matches, running, err := r.executionMatchesProcess(ctx, execution, pid)
	if err != nil {
		return processAbsenceIdentityUnavailable
	}
	if running && matches {
		return processAbsenceRecordedProcessLive
	}
	// The leader is gone (no process holds the PID) or replaced (different
	// birth). Neither alone is containment proof: the leader was its own
	// process-group leader, so descendants may still live in pgid == pid.
	descendantReason := r.processGroupSettlementStatus(pid)
	if descendantReason != processAbsenceRecordedProcessAbsent {
		return descendantReason
	}
	if !running {
		return processAbsenceRecordedProcessAbsent
	}
	return processAbsenceRecordedProcessReplaced
}

// processGroupSettlementStatus inspects the leader's process group (pgid == pid
// because agents spawn with Setpgid) for live descendants. It returns:
//   - processAbsenceRecordedProcessAbsent when the group has no live member,
//     so the caller may settle on leader absence/replacement;
//   - processAbsenceDescendantsLive when a live member remains;
//   - processAbsenceDescendantsUncertain when liveness cannot be determined.
func (r *Runtime) processGroupSettlementStatus(pid int) processAbsenceReason {
	probe := r.readProcessGroupLive
	if probe == nil {
		probe = defaultReadProcessGroupLive
	}
	hasLive, ok := probe(pid)
	if !ok {
		return processAbsenceDescendantsUncertain
	}
	if hasLive {
		return processAbsenceDescendantsLive
	}
	return processAbsenceRecordedProcessAbsent
}

// settleQuarantinedExecution finalizes an agent_executions row that recovery has
// already parked (loop paused, queue item failed with manual_intervention), once
// its recorded process is verifiably gone.
//
// It writes no process signal and requeues nothing: the loop stays parked, so a
// settled row cannot resume work. Because CountOutstandingQuarantineDebt counts
// only active rows, settlement is what lets outstanding debt reach zero without
// deleting the quarantine evidence.
//
// The status transition and the settlement audit event are written in one
// transaction: a crash between them would otherwise leave a durably terminal row
// with no settlement event, and a retry could not repair it (ListActive excludes
// terminal rows and this function skips them).
//
// Returns (settled, eventsWritten, error).
func (r *Runtime) settleQuarantinedExecution(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, nowISO string) (bool, int64, error) {
	// Terminal rows (including the status='timeout' rows that motivated #150) are
	// already outside the active set that outstanding debt counts, and terminal
	// status is immutable. Nothing to settle.
	if !storage.IsActiveAgentExecutionStatus(execution.Status) {
		return false, 0, nil
	}
	if repositories == nil || repositories.AgentExecutions == nil {
		return false, 0, nil
	}
	reason := r.recordedProcessVerifiablyGone(ctx, execution)
	if !reason.provesAbsence() {
		return false, 0, nil
	}

	settled := execution
	settled.Status = quarantineSettledStatus
	if settled.ErrorMessage == nil {
		settled.ErrorMessage = stringPtr(quarantineSettledMessage)
	}
	// Preserve native-resume eligibility, mirroring markRecoveredExecutionTerminal:
	// when native resume is enabled and the row captured a session, the settled
	// terminal row must read native_resume/pending so a later operator retry of
	// the paused loop resumes the captured agent conversation instead of silently
	// restarting from the checkpoint (resolveNativeResume only accepts the latest
	// execution when NativeResumeStatus == "pending").
	if r.nativeResumeEligibleForSettlement(settled) {
		settled.NativeResumeMode = stringPtr("native_resume")
		settled.NativeResumeStatus = stringPtr("pending")
	}
	settled.EndedAt = stringPtr(nowISO)
	settled.UpdatedAt = nowISO

	payload := map[string]any{
		"reason":           string(reason),
		"settledAt":        nowISO,
		"executionId":      execution.ID,
		"previousStatus":   execution.Status,
		"settledStatus":    quarantineSettledStatus,
		"processSignalled": false,
	}
	if execution.PID != nil && *execution.PID > 0 {
		payload["pid"] = *execution.PID
	}
	eventRecord := storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   quarantineSettledEventType,
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  stringPtr("agent_execution"),
		EntityID:    stringPtr(execution.ID),
		PayloadJSON: mustMarshalJSON(payload),
		CreatedAt:   nowISO,
	}

	if err := r.commitSettlement(ctx, repositories, settled, eventRecord); err != nil {
		return false, 0, err
	}
	if r.logger != nil {
		r.logger.Info("settled quarantined agent execution whose recorded process is gone", map[string]any{
			"executionId": execution.ID,
			"loopId":      execution.LoopID,
			"runId":       execution.RunID,
			"reason":      string(reason),
		})
	}
	return true, 1, nil
}

// nativeResumeEligibleForSettlement reports whether a settled row should be
// marked native_resume/pending so a later retry resumes the captured session.
// It mirrors markRecoveredExecutionTerminal's eligibility check.
func (r *Runtime) nativeResumeEligibleForSettlement(execution storage.AgentExecutionRecord) bool {
	cfg := r.Config()
	return cfg.Agent.NativeResume.Enabled &&
		runtimeNativeResumeSupported(execution.Vendor) &&
		execution.NativeSessionID != nil && strings.TrimSpace(*execution.NativeSessionID) != ""
}

// commitSettlement writes the terminal execution row and its settlement audit
// event atomically. When the coordinator exposes a transaction, both writes share
// one transaction so a failure leaves the row active and retryable. It falls
// back to sequential writes only when no coordinator/transaction is available.
func (r *Runtime) commitSettlement(ctx context.Context, repositories *storage.Repositories, settled storage.AgentExecutionRecord, event storage.EventLogRecord) error {
	r.mu.RLock()
	coordinator := r.services.Coordinator
	r.mu.RUnlock()
	if coordinator != nil {
		return coordinator.WithTransaction(ctx, func(tx *sql.Tx) error {
			txRepos := storage.NewRepositories(tx)
			if err := txRepos.AgentExecutions.Upsert(ctx, settled); err != nil {
				return fmt.Errorf("settle agent execution: %w", err)
			}
			return appendSystemEvent(ctx, txRepos, event)
		})
	}
	// No coordinator (e.g. bare-Runtime unit fixtures): preserve the original
	// best-effort ordering against the supplied repositories.
	if err := repositories.AgentExecutions.Upsert(ctx, settled); err != nil {
		return err
	}
	return appendSystemEvent(ctx, repositories, event)
}

// settleOrphanedQuarantinedExecutions settles quarantined active executions that
// the running-run loop never visits. Settlement is reachable only while iterating
// Runs.ListByStatus("running"), and active executions with an empty RunID are
// skipped there; a dead quarantined execution with a missing/terminal run, no run
// ID, or a missing loop would otherwise leave the daemon permanently degraded
// even though CountOutstandingQuarantineDebt counts every active row carrying
// the quarantine event.
//
// processedExecutionIDs are executions the running-run loop already examined
// (and may have settled); they are skipped here to avoid redundant probes and
// double run interruption. Runs linked to a settled orphan are interrupted when
// they are still running so they stop inflating activeRuns; a missing loop is
// synthesized from the execution's own project/loop ids so the audit event still
// records provenance.
func (r *Runtime) settleOrphanedQuarantinedExecutions(ctx context.Context, repositories *storage.Repositories, nowISO string, processedExecutionIDs map[string]struct{}) (StaleRunReconcileSummary, error) {
	var added StaleRunReconcileSummary
	if repositories == nil || repositories.AgentExecutions == nil || repositories.Events == nil {
		return added, nil
	}
	activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
	if err != nil {
		return StaleRunReconcileSummary{}, fmt.Errorf("list active agent executions: %w", err)
	}
	if len(activeExecutions) == 0 {
		return added, nil
	}
	executionIDs := make([]string, 0, len(activeExecutions))
	for _, execution := range activeExecutions {
		executionIDs = append(executionIDs, execution.ID)
	}
	quarantinedIDs, err := repositories.Events.ListEntityIDsByType(ctx, recoveryExecutionQuarantinedEventType, "agent_execution", executionIDs)
	if err != nil {
		return StaleRunReconcileSummary{}, fmt.Errorf("list quarantine recovery evidence: %w", err)
	}
	if len(quarantinedIDs) == 0 {
		return added, nil
	}
	for _, execution := range activeExecutions {
		if _, quarantined := quarantinedIDs[execution.ID]; !quarantined {
			continue
		}
		if _, processed := processedExecutionIDs[execution.ID]; processed {
			continue
		}
		if err := ctx.Err(); err != nil {
			return StaleRunReconcileSummary{}, err
		}
		settled, eventsWritten, err := r.settleQuarantinedExecution(ctx, repositories, execution, nowISO)
		if err != nil {
			return StaleRunReconcileSummary{}, err
		}
		added.EventsWritten += eventsWritten
		if !settled {
			continue
		}
		added.SettledQuarantinedExecutions += 1
		added.ExecutionIDs = append(added.ExecutionIDs, execution.ID)
		if interrupted, err := r.interruptOrphanedSettledRun(ctx, repositories, execution, nowISO); err != nil {
			return StaleRunReconcileSummary{}, err
		} else if interrupted {
			added.InterruptedRuns += 1
			added.EventsWritten += 1
			if execution.RunID != nil {
				added.RunIDs = append(added.RunIDs, *execution.RunID)
			}
			if execution.LoopID != nil {
				added.LoopIDs = append(added.LoopIDs, *execution.LoopID)
			}
		}
	}
	return added, nil
}

// interruptOrphanedSettledRun interrupts a still-running run linked to a settled
// orphan execution so it stops inflating activeRuns. It reports whether a run was
// interrupted. A missing loop is synthesized from the execution's own ids.
func (r *Runtime) interruptOrphanedSettledRun(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, nowISO string) (bool, error) {
	if repositories == nil || repositories.Runs == nil || execution.RunID == nil || strings.TrimSpace(*execution.RunID) == "" {
		return false, nil
	}
	run, err := repositories.Runs.GetByID(ctx, *execution.RunID)
	if err != nil {
		return false, fmt.Errorf("load orphan settled run: %w", err)
	}
	if run == nil || run.Status != string(domain.RunStatusRunning) {
		return false, nil
	}
	loop := storage.LoopRecord{ID: run.LoopID}
	if execution.ProjectID != nil {
		loop.ProjectID = *execution.ProjectID
	}
	if repositories.Loops != nil {
		if loaded, err := repositories.Loops.GetByID(ctx, run.LoopID); err != nil {
			return false, fmt.Errorf("load orphan settled loop: %w", err)
		} else if loaded != nil {
			loop = *loaded
		}
	}
	return true, interruptRecoveryRun(ctx, repositories, *run, loop, nowISO, "Interrupted run whose quarantined agent execution was settled: the recorded process is gone")
}
