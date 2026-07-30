package runtime

import (
	"context"

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
)

func (reason processAbsenceReason) provesAbsence() bool {
	return reason == processAbsenceRecordedProcessAbsent || reason == processAbsenceRecordedProcessReplaced
}

// recordedProcessVerifiablyGone compares an execution's durable process birth
// against the live process table.
//
// Absence is conclusive, presence is not. Settlement is asymmetric on purpose:
//   - no process holds the PID -> the recorded process is gone, whether or not
//     the row carries a recorded birth token (there is nothing to confuse it
//     with, so rows written before process identity existed still settle);
//   - the PID is held by a process with a different birth -> ours exited and the
//     PID was reused;
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
	if !running {
		return processAbsenceRecordedProcessAbsent
	}
	if matches {
		return processAbsenceRecordedProcessLive
	}
	return processAbsenceRecordedProcessReplaced
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
	settled.EndedAt = stringPtr(nowISO)
	settled.UpdatedAt = nowISO
	if err := repositories.AgentExecutions.Upsert(ctx, settled); err != nil {
		return false, 0, err
	}

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
	if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   quarantineSettledEventType,
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  stringPtr("agent_execution"),
		EntityID:    stringPtr(execution.ID),
		PayloadJSON: mustMarshalJSON(payload),
		CreatedAt:   nowISO,
	}); err != nil {
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
