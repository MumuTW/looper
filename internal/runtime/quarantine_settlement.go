package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	// recoveryExecutionQuarantineRetiredEventType records that quarantine
	// evidence stopped describing an ongoing condition. The original
	// looperd.recovery.execution_quarantined event is never deleted, so the
	// pair is the audit trail: quarantined at T1, retired at T2 because the
	// operator disposed of the loop.
	recoveryExecutionQuarantineRetiredEventType = "looperd.recovery.execution_quarantine_retired"

	// executionStatusQuarantineSettled leaves an execution row inactive without
	// claiming confirmed-dead Authority. It is deliberately absent from
	// durableTerminalExecution: settling says "this row no longer describes work
	// the daemon is waiting on", not "the process is proven drained" (ADR-0015).
	executionStatusQuarantineSettled = "quarantine_settled"
)

// QuarantineSettlementSummary reports one settlement pass.
type QuarantineSettlementSummary struct {
	// SettledExecutions counts quarantined executions retired this pass.
	SettledExecutions int64 `json:"settledExecutions"`
	// SettledRuns counts still-running runs closed alongside those executions.
	SettledRuns int64 `json:"settledRuns"`
	// LiveExecutionsRetained counts quarantined executions left as debt because
	// a probe still matched their process.
	LiveExecutionsRetained int64 `json:"liveExecutionsRetained"`
	// ParkedExecutionsRetained counts quarantined executions left as debt
	// because no operator has disposed of their loop yet.
	ParkedExecutionsRetained int64    `json:"parkedExecutionsRetained"`
	EventsWritten            int64    `json:"eventsWritten"`
	ExecutionIDs             []string `json:"executionIds,omitempty"`
}

// quarantineParkedLoopStatus reports whether a loop is still parked by
// quarantine, i.e. nobody has decided what to do with it yet.
//
// quarantineRecoveryEvidence parks loops at "paused" and deliberately leaves an
// existing "human_takeover" alone. Every other status is a disposition an
// operator already made through an existing verb — retry/start requeue it,
// stop/close terminate it — and that disposition, not a PID probe, is the
// Authority for retiring the quarantine evidence behind it.
func quarantineParkedLoopStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "paused", "human_takeover":
		return true
	default:
		return false
	}
}

// settleDisposedQuarantine retires quarantine evidence whose loop an operator
// has already disposed of, so the daemon can leave the degraded state without a
// restart and without manual database edits (#149 / #150).
//
// What it must not do, and does not do:
//   - it never kills or signals a process;
//   - it never treats a missing PID as confirmed-dead Authority. A quarantined
//     execution whose loop is still parked keeps counting as debt no matter what
//     the PID probe says, and one whose process still matches is retained even
//     after disposition;
//   - it never deletes the original quarantine event.
//
// Executions already at a durable terminal status (including the "timeout" rows
// #150 asks about) are out of scope by construction: ListActive only returns
// running/cancelling, so they never counted toward debt in the first place.
func (r *Runtime) settleDisposedQuarantine(ctx context.Context, repositories *storage.Repositories, nowISO string) (QuarantineSettlementSummary, error) {
	var summary QuarantineSettlementSummary
	if repositories == nil || repositories.AgentExecutions == nil || repositories.Events == nil || repositories.Loops == nil {
		return summary, nil
	}

	activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
	if err != nil {
		return QuarantineSettlementSummary{}, fmt.Errorf("list active agent executions: %w", err)
	}
	if len(activeExecutions) == 0 {
		return summary, nil
	}
	executionIDs := make([]string, 0, len(activeExecutions))
	for _, execution := range activeExecutions {
		executionIDs = append(executionIDs, execution.ID)
	}
	quarantinedAtByExecutionID, err := repositories.Events.ListFirstEventTimestampsByType(ctx, recoveryExecutionQuarantinedEventType, "agent_execution", executionIDs)
	if err != nil {
		return QuarantineSettlementSummary{}, fmt.Errorf("list quarantine recovery evidence: %w", err)
	}
	if len(quarantinedAtByExecutionID) == 0 {
		return summary, nil
	}

	for _, execution := range activeExecutions {
		if err := ctx.Err(); err != nil {
			return QuarantineSettlementSummary{}, err
		}
		quarantinedAt, quarantined := quarantinedAtByExecutionID[execution.ID]
		if !quarantined {
			continue
		}

		loopID := strings.TrimSpace(derefString(execution.LoopID))
		var loop *storage.LoopRecord
		if loopID != "" {
			loop, err = repositories.Loops.GetByID(ctx, loopID)
			if err != nil {
				return QuarantineSettlementSummary{}, err
			}
		}
		// A loop still parked is real outstanding debt: work stopped and a human
		// has not decided anything yet. Keep the daemon degraded and say so.
		// An execution with no loop to dispose of has no disposition Authority
		// either, so it is retained rather than settled by default.
		if loop == nil || quarantineParkedLoopStatus(loop.Status) {
			summary.ParkedExecutionsRetained++
			continue
		}

		// Disposition is not permission to forget a process that is demonstrably
		// still there. A matching probe keeps the row as debt.
		classification, err := r.classifyStartupExecution(ctx, execution, nil)
		if err != nil {
			return QuarantineSettlementSummary{}, err
		}
		if classification.Class == ContainmentObservedLive {
			summary.LiveExecutionsRetained++
			continue
		}

		settledRuns, events, err := r.settleQuarantinedExecution(ctx, repositories, execution, loop, quarantinedAt, loop.Status, classification, nowISO)
		if err != nil {
			return QuarantineSettlementSummary{}, err
		}
		summary.SettledExecutions++
		summary.SettledRuns += settledRuns
		summary.EventsWritten += events
		summary.ExecutionIDs = append(summary.ExecutionIDs, execution.ID)
	}

	if summary.SettledExecutions > 0 && r.logger != nil {
		r.logger.Info("retired quarantine evidence for operator-disposed loops", map[string]any{
			"settledExecutions": summary.SettledExecutions,
			"settledRuns":       summary.SettledRuns,
			"retainedLive":      summary.LiveExecutionsRetained,
			"retainedParked":    summary.ParkedExecutionsRetained,
		})
	}
	return summary, nil
}

// settleQuarantinedExecution writes the settlement for one execution: the
// execution row leaves active, any run still stuck at running is closed, and a
// retirement event records why.
func (r *Runtime) settleQuarantinedExecution(ctx context.Context, repositories *storage.Repositories, execution storage.AgentExecutionRecord, loop *storage.LoopRecord, quarantinedAt, disposition string, classification ContainmentClassification, nowISO string) (int64, int64, error) {
	reason := fmt.Sprintf("quarantine settled: loop disposed by operator (loop status %s)", disposition)

	settled := execution
	settled.Status = executionStatusQuarantineSettled
	if settled.EndedAt == nil {
		settled.EndedAt = stringPtr(nowISO)
	}
	if settled.ErrorMessage == nil {
		settled.ErrorMessage = stringPtr(reason)
	}
	settled.UpdatedAt = nowISO
	if err := repositories.AgentExecutions.Upsert(ctx, settled); err != nil {
		return 0, 0, err
	}

	var settledRuns int64
	var events int64
	runID := strings.TrimSpace(derefString(execution.RunID))
	if runID != "" && repositories.Runs != nil && loop != nil {
		run, err := repositories.Runs.GetByID(ctx, runID)
		if err != nil {
			return 0, 0, err
		}
		// Leaving the run at running is what inflated activeRuns and what the
		// one-running-run-per-loop unique index trips over when the operator
		// retries, so close it with the same status migration 0008 uses.
		if run != nil && run.Status == string(domain.RunStatusRunning) {
			if err := interruptRecoveryRun(ctx, repositories, *run, *loop, nowISO, reason); err != nil {
				return 0, 0, err
			}
			settledRuns++
			events++
		}
	}

	payload := map[string]any{
		"reason":         reason,
		"executionId":    execution.ID,
		"quarantinedAt":  quarantinedAt,
		"loopStatus":     disposition,
		"identityReason": classification.Reason,
		"class":          string(classification.Class),
		"statusBefore":   execution.Status,
		"statusAfter":    executionStatusQuarantineSettled,
	}
	if classification.PID > 0 {
		payload["pid"] = classification.PID
	}
	if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   recoveryExecutionQuarantineRetiredEventType,
		ProjectID:   execution.ProjectID,
		LoopID:      execution.LoopID,
		RunID:       execution.RunID,
		EntityType:  stringPtr("agent_execution"),
		EntityID:    stringPtr(execution.ID),
		PayloadJSON: mustMarshalJSON(payload),
		CreatedAt:   nowISO,
	}); err != nil {
		return settledRuns, events, err
	}
	events++
	return settledRuns, events, nil
}
