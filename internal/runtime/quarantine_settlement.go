package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	// recoveryExecutionQuarantineRetiredEventType records that quarantine
	// evidence stopped describing an ongoing condition. The original
	// looperd.recovery.execution_quarantined event is never deleted, so the
	// pair is the audit trail: quarantined at T1, retired at T2 for a named
	// reason.
	recoveryExecutionQuarantineRetiredEventType = "looperd.recovery.execution_quarantine_retired"

	// executionStatusQuarantineSettled leaves an execution row inactive without
	// claiming confirmed-dead Authority. It is deliberately absent from
	// durableTerminalExecution: settling says "this row no longer describes work
	// the daemon is waiting on", not "the process is proven drained" (ADR-0015).
	executionStatusQuarantineSettled = "quarantine_settled"
)

// ErrQuarantineEvidenceChanged reports that durable state moved between the
// liveness probe and the write. It aborts the caller's transaction rather than
// letting a stale snapshot overwrite a concurrent heartbeat or finalization.
var ErrQuarantineEvidenceChanged = errors.New("quarantine evidence changed after it was assessed")

// SettlementProvenance names why quarantine evidence was retired. It is
// recorded on the retirement event and is deliberately not collapsed into one
// "operator" label: an explicit retry/stop/close is a human act, while a loop
// that a Role drove to a terminal status is not, and the audit trail should not
// claim otherwise.
type SettlementProvenance string

const (
	// SettlementByOperatorRetry is an explicit human verb. It authorizes
	// settling a loop that is still parked, because the operator's own action is
	// the disposition — this is what makes `looper retry` on a quarantined loop
	// work at all (#149).
	SettlementByOperatorRetry SettlementProvenance = "operator_retry"
	// SettlementByLoopDisposed covers the periodic backstop: the loop is no
	// longer parked, so whatever moved it — an operator verb, or a Role reaching
	// a terminal status — there is no work left for this execution to describe.
	// It never settles a parked loop.
	SettlementByLoopDisposed SettlementProvenance = "loop_disposed"
)

// QuarantineSettlementSummary reports one settlement.
type QuarantineSettlementSummary struct {
	SettledExecutions int64 `json:"settledExecutions"`
	SettledRuns       int64 `json:"settledRuns"`
	// LiveExecutionsRetained counts executions left alone because a probe still
	// matched their process.
	LiveExecutionsRetained int64    `json:"liveExecutionsRetained"`
	EventsWritten          int64    `json:"eventsWritten"`
	ExecutionIDs           []string `json:"executionIds,omitempty"`
}

// plannedSettlement is one execution the probe cleared for settling, carrying
// the exact row version it was assessed against.
type plannedSettlement struct {
	execution      storage.AgentExecutionRecord
	loopStatus     string
	quarantinedAt  string
	classification ContainmentClassification
	// runObservedUpdatedAt is the run's version at probe time, empty when there
	// is no run to close or a live sibling still holds it.
	runObservedUpdatedAt string
	closeRun             bool
}

// QuarantineSettlementPlan is the decision a liveness probe reached, ready to
// be applied inside a caller's transaction.
//
// Planning and applying are split because the two have incompatible needs:
// probing inspects processes and must not hold a write transaction open, while
// the write must be atomic with whatever the caller is doing. Splitting them
// introduces a window, which is why every write in Apply is conditional on the
// row version recorded here.
type QuarantineSettlementPlan struct {
	provenance SettlementProvenance
	planned    []plannedSettlement
	retained   QuarantineSettlementSummary
}

// Empty reports whether the plan would write nothing.
func (p QuarantineSettlementPlan) Empty() bool { return len(p.planned) == 0 }

// PlanQuarantineSettlementForLoop probes one loop's quarantined executions and
// returns what may be settled on the Authority of the operator verb being
// served. It reads and probes only that loop: a single retry must not make the
// daemon inspect every process it has ever recorded.
//
// It does not consult quarantineParkedLoopStatus. Quarantine parks the loop at
// `paused` and leaves its run at `running`, and assertLoopRetryPreconditions
// refuses any retry whose loop has a running run — so waiting for the loop to
// be non-parked would deadlock. The operator's verb is what breaks that cycle.
//
// A live process is still refused: asking to retry does not make a running
// agent go away, and the retry must fail loudly instead.
func (r *Runtime) PlanQuarantineSettlementForLoop(ctx context.Context, loopID string, provenance SettlementProvenance) (QuarantineSettlementPlan, error) {
	r.mu.RLock()
	repositories := r.services.Repositories
	r.mu.RUnlock()
	loopID = strings.TrimSpace(loopID)
	if repositories == nil || loopID == "" {
		return QuarantineSettlementPlan{provenance: provenance}, nil
	}
	loop, err := repositories.Loops.GetByID(ctx, loopID)
	if err != nil {
		return QuarantineSettlementPlan{}, err
	}
	if loop == nil {
		return QuarantineSettlementPlan{provenance: provenance}, nil
	}
	return r.planQuarantineSettlement(ctx, repositories, []storage.LoopRecord{*loop}, provenance)
}

// planQuarantineSettlement is the shared planner over an explicit loop set.
func (r *Runtime) planQuarantineSettlement(ctx context.Context, repositories *storage.Repositories, loops []storage.LoopRecord, provenance SettlementProvenance) (QuarantineSettlementPlan, error) {
	plan := QuarantineSettlementPlan{provenance: provenance}
	if repositories == nil || repositories.AgentExecutions == nil || repositories.Events == nil || len(loops) == 0 {
		return plan, nil
	}
	loopStatusByID := make(map[string]string, len(loops))
	for _, loop := range loops {
		loopStatusByID[loop.ID] = loop.Status
	}

	activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
	if err != nil {
		return QuarantineSettlementPlan{}, fmt.Errorf("list active agent executions: %w", err)
	}
	scoped := make([]storage.AgentExecutionRecord, 0, len(activeExecutions))
	for _, execution := range activeExecutions {
		if _, wanted := loopStatusByID[strings.TrimSpace(derefString(execution.LoopID))]; wanted {
			scoped = append(scoped, execution)
		}
	}
	if len(scoped) == 0 {
		return plan, nil
	}
	executionIDs := make([]string, 0, len(scoped))
	for _, execution := range scoped {
		executionIDs = append(executionIDs, execution.ID)
	}
	quarantinedAtByExecutionID, err := repositories.Events.ListFirstEventTimestampsByType(ctx, recoveryExecutionQuarantinedEventType, "agent_execution", executionIDs)
	if err != nil {
		return QuarantineSettlementPlan{}, fmt.Errorf("list quarantine recovery evidence: %w", err)
	}
	if len(quarantinedAtByExecutionID) == 0 {
		return plan, nil
	}

	// Liveness is classified across every execution in scope, not only the
	// quarantined ones: a run is safe to close only when nothing on it is live,
	// including a sibling this pass is not settling.
	classifications := make(map[string]ContainmentClassification, len(scoped))
	liveRunIDs := make(map[string]struct{})
	for _, execution := range scoped {
		classification, err := r.classifyStartupExecution(ctx, execution, nil)
		if err != nil {
			return QuarantineSettlementPlan{}, err
		}
		classifications[execution.ID] = classification
		if classification.Class == ContainmentObservedLive {
			if runID := strings.TrimSpace(derefString(execution.RunID)); runID != "" {
				liveRunIDs[runID] = struct{}{}
			}
		}
	}

	runVersions := make(map[string]string)
	for _, execution := range scoped {
		quarantinedAt, quarantined := quarantinedAtByExecutionID[execution.ID]
		if !quarantined {
			continue
		}
		if classifications[execution.ID].Class == ContainmentObservedLive {
			plan.retained.LiveExecutionsRetained++
			continue
		}
		entry := plannedSettlement{
			execution:      execution,
			loopStatus:     loopStatusByID[strings.TrimSpace(derefString(execution.LoopID))],
			quarantinedAt:  quarantinedAt,
			classification: classifications[execution.ID],
		}
		runID := strings.TrimSpace(derefString(execution.RunID))
		if _, live := liveRunIDs[runID]; runID != "" && !live && repositories.Runs != nil {
			version, ok := runVersions[runID]
			if !ok {
				run, err := repositories.Runs.GetByID(ctx, runID)
				if err != nil {
					return QuarantineSettlementPlan{}, err
				}
				if run != nil && run.Status == string(domain.RunStatusRunning) {
					version = run.UpdatedAt
				}
				runVersions[runID] = version
			}
			if version != "" {
				entry.runObservedUpdatedAt = version
				entry.closeRun = true
			}
		}
		plan.planned = append(plan.planned, entry)
	}
	return plan, nil
}

// ApplyQuarantineSettlement writes a plan through the caller's repositories,
// which are expected to be transaction-scoped.
//
// Every write is conditional on the row version the plan recorded, and a miss
// returns ErrQuarantineEvidenceChanged rather than forcing the write. Two
// properties follow, both of which an earlier revision got wrong:
//
//   - Concurrent state is never overwritten. Settlement touches only the
//     columns it owns and only while the row is still the one that was probed,
//     so a heartbeat or a terminal write landing in the probe window wins.
//   - Evidence is never retired unless the caller's own work commits. Running
//     inside the retry transaction means a later precondition failure rolls the
//     settlement back with it, instead of leaving a loop whose quarantine was
//     cleared but which was never actually retried.
func ApplyQuarantineSettlement(ctx context.Context, repositories *storage.Repositories, plan QuarantineSettlementPlan, nowISO string) (QuarantineSettlementSummary, error) {
	summary := plan.retained
	if repositories == nil || len(plan.planned) == 0 {
		return summary, nil
	}
	for _, entry := range plan.planned {
		reason := quarantineSettlementReason(plan.provenance, entry.loopStatus)
		settled, err := repositories.AgentExecutions.SettleQuarantined(ctx, storage.SettleQuarantinedExecutionInput{
			ID:                entry.execution.ID,
			ObservedStatus:    entry.execution.Status,
			ObservedUpdatedAt: entry.execution.UpdatedAt,
			Status:            executionStatusQuarantineSettled,
			EndedAt:           nowISO,
			ErrorMessage:      reason,
			UpdatedAt:         nowISO,
		})
		if err != nil {
			return QuarantineSettlementSummary{}, err
		}
		if !settled {
			return QuarantineSettlementSummary{}, fmt.Errorf("%w: agent execution %s", ErrQuarantineEvidenceChanged, entry.execution.ID)
		}

		if entry.closeRun {
			runID := strings.TrimSpace(derefString(entry.execution.RunID))
			interrupted, err := repositories.Runs.InterruptQuarantinedIfUnchanged(ctx, runID, entry.runObservedUpdatedAt, reason, nowISO)
			if err != nil {
				return QuarantineSettlementSummary{}, err
			}
			if !interrupted {
				return QuarantineSettlementSummary{}, fmt.Errorf("%w: run %s", ErrQuarantineEvidenceChanged, runID)
			}
			summary.SettledRuns++
			if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
				ID:          newRuntimeEventID(),
				EventType:   "looperd.recovery.run_interrupted",
				ProjectID:   entry.execution.ProjectID,
				LoopID:      entry.execution.LoopID,
				RunID:       entry.execution.RunID,
				EntityType:  stringPtr("run"),
				EntityID:    stringPtr(runID),
				PayloadJSON: mustMarshalJSON(map[string]any{"previousStatus": "running", "recoveredStatus": "interrupted"}),
				CreatedAt:   nowISO,
			}); err != nil {
				return QuarantineSettlementSummary{}, err
			}
			summary.EventsWritten++
		}

		payload := map[string]any{
			"reason":         reason,
			"provenance":     string(plan.provenance),
			"executionId":    entry.execution.ID,
			"quarantinedAt":  entry.quarantinedAt,
			"loopStatus":     entry.loopStatus,
			"identityReason": entry.classification.Reason,
			"class":          string(entry.classification.Class),
			"statusBefore":   entry.execution.Status,
			"statusAfter":    executionStatusQuarantineSettled,
		}
		if !entry.closeRun && strings.TrimSpace(derefString(entry.execution.RunID)) != "" {
			payload["runRetained"] = "live_or_already_closed"
		}
		if entry.classification.PID > 0 {
			payload["pid"] = entry.classification.PID
		}
		if err := appendSystemEvent(ctx, repositories, storage.EventLogRecord{
			ID:          newRuntimeEventID(),
			EventType:   recoveryExecutionQuarantineRetiredEventType,
			ProjectID:   entry.execution.ProjectID,
			LoopID:      entry.execution.LoopID,
			RunID:       entry.execution.RunID,
			EntityType:  stringPtr("agent_execution"),
			EntityID:    stringPtr(entry.execution.ID),
			PayloadJSON: mustMarshalJSON(payload),
			CreatedAt:   nowISO,
		}); err != nil {
			return QuarantineSettlementSummary{}, err
		}
		summary.EventsWritten++
		summary.SettledExecutions++
		summary.ExecutionIDs = append(summary.ExecutionIDs, entry.execution.ID)
	}
	return summary, nil
}

// quarantineParkedLoopStatus reports whether a loop is still parked by
// quarantine, i.e. nothing has decided what to do with it yet.
//
// quarantineRecoveryEvidence parks loops at "paused" and deliberately leaves an
// existing "human_takeover" alone. Any other status means the loop already
// moved on, which the periodic backstop treats as reason enough to stop
// counting its quarantine evidence — see SettlementByLoopDisposed for why that
// is not described as an operator act.
func quarantineParkedLoopStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "paused", "human_takeover":
		return true
	default:
		return false
	}
}

// settleDisposedQuarantine is the periodic backstop for evidence whose loop
// already moved on by a path that did not settle inline. It never settles a
// parked loop, never kills a process, and never treats a missing PID as
// confirmed-dead Authority.
//
// Executions already at a durable terminal status (including the "timeout" rows
// #150 asks about) are out of scope by construction: ListActive only returns
// running/cancelling, so they never counted toward debt in the first place.
func (r *Runtime) settleDisposedQuarantine(ctx context.Context, repositories *storage.Repositories, nowISO string) (QuarantineSettlementSummary, error) {
	var summary QuarantineSettlementSummary
	if repositories == nil || repositories.Loops == nil {
		return summary, nil
	}
	loops, err := repositories.Loops.List(ctx)
	if err != nil {
		return QuarantineSettlementSummary{}, err
	}
	disposed := make([]storage.LoopRecord, 0, len(loops))
	for _, loop := range loops {
		if quarantineParkedLoopStatus(loop.Status) {
			continue
		}
		disposed = append(disposed, loop)
	}
	if len(disposed) == 0 {
		return summary, nil
	}
	plan, err := r.planQuarantineSettlement(ctx, repositories, disposed, SettlementByLoopDisposed)
	if err != nil {
		return QuarantineSettlementSummary{}, err
	}
	if plan.Empty() {
		return plan.retained, nil
	}

	r.mu.RLock()
	coordinator := r.services.Coordinator
	r.mu.RUnlock()
	if coordinator == nil {
		return QuarantineSettlementSummary{}, fmt.Errorf("storage coordinator is not configured")
	}
	summary, err = storage.WithTransactionValue(ctx, coordinator.DB(), nil, func(tx *sql.Tx) (QuarantineSettlementSummary, error) {
		return ApplyQuarantineSettlement(ctx, storage.NewRepositories(tx), plan, nowISO)
	})
	if err != nil {
		// Losing a race with concurrent execution state is expected on a busy
		// daemon and self-corrects: the next tick re-probes. It is not a
		// reconcile failure.
		if errors.Is(err, ErrQuarantineEvidenceChanged) {
			if r.logger != nil {
				r.logger.Info("quarantine settlement deferred to next tick", map[string]any{"reason": err.Error()})
			}
			return plan.retained, nil
		}
		return QuarantineSettlementSummary{}, err
	}
	if summary.SettledExecutions > 0 && r.logger != nil {
		r.logger.Info("retired quarantine evidence", map[string]any{
			"provenance":        string(SettlementByLoopDisposed),
			"settledExecutions": summary.SettledExecutions,
			"settledRuns":       summary.SettledRuns,
		})
	}
	return summary, nil
}

func quarantineSettlementReason(provenance SettlementProvenance, loopStatus string) string {
	switch provenance {
	case SettlementByOperatorRetry:
		return "quarantine settled: operator retried this loop"
	default:
		return fmt.Sprintf("quarantine settled: loop is no longer parked (loop status %s)", loopStatus)
	}
}
