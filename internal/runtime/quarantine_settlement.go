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

// SettlementProvenance names why quarantine evidence was retired. It is
// recorded on the retirement event and is deliberately not collapsed into one
// "operator" label: an explicit retry/stop/close is a human act, while a loop
// that a Role drove to a terminal status is not, and the audit trail should not
// claim otherwise.
type SettlementProvenance string

const (
	// SettlementByOperatorRetry / Stop are explicit human verbs. They authorize
	// settling a loop that is still parked, because the operator's own action is
	// the disposition — this is what makes `looper retry` on a quarantined loop
	// work at all (#149).
	SettlementByOperatorRetry SettlementProvenance = "operator_retry"
	SettlementByOperatorStop  SettlementProvenance = "operator_stop"
	// SettlementByLoopDisposed covers the periodic backstop: the loop is no
	// longer parked, so whatever moved it — an operator verb whose settlement
	// did not run, or a Role reaching a terminal status — there is no work left
	// for this execution to describe. It never settles a parked loop.
	SettlementByLoopDisposed SettlementProvenance = "loop_disposed"
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
	// because nothing has disposed of their loop yet.
	ParkedExecutionsRetained int64    `json:"parkedExecutionsRetained"`
	EventsWritten            int64    `json:"eventsWritten"`
	ExecutionIDs             []string `json:"executionIds,omitempty"`
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

// settlementCandidate is one quarantined execution considered for settlement,
// carrying the classification and the run-level context needed to decide.
type settlementCandidate struct {
	execution     storage.AgentExecutionRecord
	loop          *storage.LoopRecord
	quarantinedAt string
}

// SettleQuarantineForLoop retires the quarantine evidence on one loop on the
// Authority of an explicit operator verb, and is the reason `looper retry`
// works on a quarantined loop at all.
//
// Without it the recovery path deadlocks: quarantine parks the loop at `paused`
// and leaves its run at `running`, assertLoopRetryPreconditions refuses any
// retry whose loop has a running run, and the periodic backstop only settles
// loops that are no longer parked. Nothing could move first. The operator's
// verb breaks that cycle — it is a disposition in its own right, so this path
// does not consult quarantineParkedLoopStatus.
//
// It still refuses to settle an execution whose process is observed live: an
// operator asking to retry does not make a running agent go away, and the retry
// must fail loudly instead.
//
// Callers run this before publishing replacement work, so the settled run is
// durable before any claim can race it onto idx_runs_one_running_per_loop.
func (r *Runtime) SettleQuarantineForLoop(ctx context.Context, loopID string, provenance SettlementProvenance) (QuarantineSettlementSummary, error) {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	r.mu.RUnlock()
	if repositories == nil {
		return QuarantineSettlementSummary{}, nil
	}
	if now == nil {
		return QuarantineSettlementSummary{}, fmt.Errorf("runtime clock is not configured")
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	return r.settleQuarantine(ctx, repositories, nowISO, strings.TrimSpace(loopID), provenance)
}

// settleDisposedQuarantine is the periodic backstop: it retires quarantine
// evidence for every loop that is no longer parked, so evidence an operator
// resolved through a path that did not settle inline still stops counting
// (#149 / #150).
//
// What it must not do, and does not do:
//   - it never kills or signals a process;
//   - it never treats a missing PID as confirmed-dead Authority. A quarantined
//     execution whose loop is still parked keeps counting as debt no matter what
//     the PID probe says, and one whose process still matches is retained even
//     after disposition;
//   - it never closes a run that any live execution still belongs to;
//   - it never deletes the original quarantine event.
//
// Executions already at a durable terminal status (including the "timeout" rows
// #150 asks about) are out of scope by construction: ListActive only returns
// running/cancelling, so they never counted toward debt in the first place.
func (r *Runtime) settleDisposedQuarantine(ctx context.Context, repositories *storage.Repositories, nowISO string) (QuarantineSettlementSummary, error) {
	return r.settleQuarantine(ctx, repositories, nowISO, "", SettlementByLoopDisposed)
}

// settleQuarantine is the shared core. When loopIDFilter is empty it runs the
// parked gate (periodic backstop); when set it settles that loop only, on the
// caller's provenance, without the parked gate.
func (r *Runtime) settleQuarantine(ctx context.Context, repositories *storage.Repositories, nowISO, loopIDFilter string, provenance SettlementProvenance) (QuarantineSettlementSummary, error) {
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

	// Liveness is classified for every active execution first, not just the
	// candidates: a run is only safe to close when no execution on it is live,
	// including siblings this pass is not settling.
	liveRunIDs, err := r.liveRunIDs(ctx, activeExecutions)
	if err != nil {
		return QuarantineSettlementSummary{}, err
	}

	var candidates []settlementCandidate
	for _, execution := range activeExecutions {
		quarantinedAt, quarantined := quarantinedAtByExecutionID[execution.ID]
		if !quarantined {
			continue
		}
		loopID := strings.TrimSpace(derefString(execution.LoopID))
		if loopIDFilter != "" && loopID != loopIDFilter {
			continue
		}
		var loop *storage.LoopRecord
		if loopID != "" {
			loop, err = repositories.Loops.GetByID(ctx, loopID)
			if err != nil {
				return QuarantineSettlementSummary{}, err
			}
		}
		// An execution with no loop row has nothing that could have disposed of
		// it, so it is retained rather than settled by default.
		if loop == nil {
			summary.ParkedExecutionsRetained++
			continue
		}
		// The backstop only touches loops that already moved on. An explicit
		// operator verb is itself the disposition and skips this gate.
		if loopIDFilter == "" && quarantineParkedLoopStatus(loop.Status) {
			summary.ParkedExecutionsRetained++
			continue
		}
		candidates = append(candidates, settlementCandidate{execution: execution, loop: loop, quarantinedAt: quarantinedAt})
	}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return QuarantineSettlementSummary{}, err
		}
		// Disposition is not permission to forget a process that is demonstrably
		// still there.
		classification, err := r.classifyStartupExecution(ctx, candidate.execution, nil)
		if err != nil {
			return QuarantineSettlementSummary{}, err
		}
		if classification.Class == ContainmentObservedLive {
			summary.LiveExecutionsRetained++
			continue
		}
		runID := strings.TrimSpace(derefString(candidate.execution.RunID))
		_, runHasLiveExecution := liveRunIDs[runID]

		settledRuns, events, err := r.commitQuarantineSettlement(ctx, candidate, classification, provenance, runHasLiveExecution, nowISO)
		if err != nil {
			return QuarantineSettlementSummary{}, err
		}
		summary.SettledExecutions++
		summary.SettledRuns += settledRuns
		summary.EventsWritten += events
		summary.ExecutionIDs = append(summary.ExecutionIDs, candidate.execution.ID)
	}

	if summary.SettledExecutions > 0 && r.logger != nil {
		r.logger.Info("retired quarantine evidence", map[string]any{
			"provenance":        string(provenance),
			"settledExecutions": summary.SettledExecutions,
			"settledRuns":       summary.SettledRuns,
			"retainedLive":      summary.LiveExecutionsRetained,
			"retainedParked":    summary.ParkedExecutionsRetained,
		})
	}
	return summary, nil
}

// liveRunIDs classifies every active execution and returns the runs that still
// have at least one observed-live execution on them. Settling one uncertain
// execution must not close a run a live sibling is still working under, because
// closing it frees idx_runs_one_running_per_loop and lets replacement work start
// alongside a demonstrably live agent.
func (r *Runtime) liveRunIDs(ctx context.Context, activeExecutions []storage.AgentExecutionRecord) (map[string]struct{}, error) {
	live := make(map[string]struct{})
	for _, execution := range activeExecutions {
		runID := strings.TrimSpace(derefString(execution.RunID))
		if runID == "" {
			continue
		}
		if _, known := live[runID]; known {
			continue
		}
		classification, err := r.classifyStartupExecution(ctx, execution, nil)
		if err != nil {
			return nil, err
		}
		if classification.Class == ContainmentObservedLive {
			live[runID] = struct{}{}
		}
	}
	return live, nil
}

// commitQuarantineSettlement writes one execution's settlement in a single
// transaction: the execution leaves active, any run still stuck at running is
// closed, and the events land together.
//
// Atomicity is the point. Split across statements, a failure after the
// execution upsert would drop the row out of ListActive — so no later pass
// would ever reconsider it — while leaving the run at running and the audit
// trail incomplete. That failure is invisible and permanent, so the write is
// all-or-nothing.
func (r *Runtime) commitQuarantineSettlement(ctx context.Context, candidate settlementCandidate, classification ContainmentClassification, provenance SettlementProvenance, runHasLiveExecution bool, nowISO string) (int64, int64, error) {
	r.mu.RLock()
	coordinator := r.services.Coordinator
	r.mu.RUnlock()
	if coordinator == nil {
		return 0, 0, fmt.Errorf("storage coordinator is not configured")
	}

	var settledRuns, events int64
	err := storage.WithTransaction(ctx, coordinator.DB(), nil, func(tx *sql.Tx) error {
		settledRuns, events = 0, 0
		repos := storage.NewRepositories(tx)
		execution := candidate.execution
		loop := candidate.loop
		reason := quarantineSettlementReason(provenance, loop.Status)

		settled := execution
		settled.Status = executionStatusQuarantineSettled
		if settled.EndedAt == nil {
			settled.EndedAt = stringPtr(nowISO)
		}
		if settled.ErrorMessage == nil {
			settled.ErrorMessage = stringPtr(reason)
		}
		settled.UpdatedAt = nowISO
		if err := repos.AgentExecutions.Upsert(ctx, settled); err != nil {
			return err
		}

		runID := strings.TrimSpace(derefString(execution.RunID))
		if runID != "" && !runHasLiveExecution {
			run, err := repos.Runs.GetByID(ctx, runID)
			if err != nil {
				return err
			}
			// Leaving the run at running is what inflated activeRuns and what
			// the one-running-run-per-loop unique index trips over when the
			// operator retries, so close it with the status migration 0008 uses.
			if run != nil && run.Status == string(domain.RunStatusRunning) {
				if err := interruptRecoveryRun(ctx, repos, *run, *loop, nowISO, reason); err != nil {
					return err
				}
				settledRuns++
				events++
			}
		}

		payload := map[string]any{
			"reason":         reason,
			"provenance":     string(provenance),
			"executionId":    execution.ID,
			"quarantinedAt":  candidate.quarantinedAt,
			"loopStatus":     loop.Status,
			"identityReason": classification.Reason,
			"class":          string(classification.Class),
			"statusBefore":   execution.Status,
			"statusAfter":    executionStatusQuarantineSettled,
		}
		if runHasLiveExecution {
			payload["runRetained"] = "live_sibling_execution"
		}
		if classification.PID > 0 {
			payload["pid"] = classification.PID
		}
		if err := appendSystemEvent(ctx, repos, storage.EventLogRecord{
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
			return err
		}
		events++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return settledRuns, events, nil
}

func quarantineSettlementReason(provenance SettlementProvenance, loopStatus string) string {
	switch provenance {
	case SettlementByOperatorRetry:
		return "quarantine settled: operator retried this loop"
	case SettlementByOperatorStop:
		return "quarantine settled: operator stopped this loop"
	default:
		return fmt.Sprintf("quarantine settled: loop is no longer parked (loop status %s)", loopStatus)
	}
}
