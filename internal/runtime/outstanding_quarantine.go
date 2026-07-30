package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

const recoveryExecutionQuarantinedEventType = "looperd.recovery.execution_quarantined"

// OutstandingQuarantineDebt is live (not startup-snapshot) quarantine/orphan
// debt visible after recovery or live/manual stale reconcile. Quarantine parks
// loops/queues and never kills processes, so agent_executions stay active while
// the recorded process may still be running.
//
// Debt is scoped to active rows on purpose, and that is the exit: the stale-run
// reconcile settles a parked execution (see settleQuarantinedExecution) once its
// recorded process is verifiably gone, which drops it out of this count while
// the looperd.recovery.execution_quarantined event stays in the log. Executions
// whose process is still alive, or whose absence cannot be proven, keep counting.
type OutstandingQuarantineDebt struct {
	// QuarantinedActiveExecutions counts active agent_executions for which
	// recovery wrote explicit quarantine evidence.
	QuarantinedActiveExecutions int `json:"quarantinedActiveExecutions"`
	// QuarantinedRunningRuns counts runs still status=running that are linked to
	// those explicitly quarantined active executions (the activeRuns inflation
	// ops miss).
	QuarantinedRunningRuns int `json:"quarantinedRunningRuns"`
}

// CountOutstandingQuarantineDebt scans durable state for quarantine/orphan debt
// that outlives the one-shot startup recovery snapshot.
func CountOutstandingQuarantineDebt(ctx context.Context, repositories *storage.Repositories) (OutstandingQuarantineDebt, error) {
	var debt OutstandingQuarantineDebt
	if repositories == nil || repositories.AgentExecutions == nil {
		return debt, nil
	}

	activeExecutions, err := repositories.AgentExecutions.ListActive(ctx)
	if err != nil {
		return OutstandingQuarantineDebt{}, fmt.Errorf("list active agent executions: %w", err)
	}
	if len(activeExecutions) == 0 {
		return debt, nil
	}

	executionIDs := make([]string, 0, len(activeExecutions))
	for _, execution := range activeExecutions {
		executionIDs = append(executionIDs, execution.ID)
	}
	if repositories.Events == nil {
		return debt, nil
	}
	quarantinedExecutionIDs, err := repositories.Events.ListEntityIDsByType(ctx, recoveryExecutionQuarantinedEventType, "agent_execution", executionIDs)
	if err != nil {
		return OutstandingQuarantineDebt{}, fmt.Errorf("list quarantine recovery evidence: %w", err)
	}
	if len(quarantinedExecutionIDs) == 0 {
		return debt, nil
	}
	quarantinedRunIDs := make(map[string]struct{})
	for _, execution := range activeExecutions {
		if _, quarantined := quarantinedExecutionIDs[execution.ID]; !quarantined {
			continue
		}
		debt.QuarantinedActiveExecutions++
		if execution.RunID != nil && strings.TrimSpace(*execution.RunID) != "" {
			quarantinedRunIDs[strings.TrimSpace(*execution.RunID)] = struct{}{}
		}
	}
	if repositories.Runs == nil || len(quarantinedRunIDs) == 0 {
		return debt, nil
	}
	runs, err := repositories.Runs.ListByIDs(ctx, mapKeys(quarantinedRunIDs))
	if err != nil {
		return OutstandingQuarantineDebt{}, fmt.Errorf("list quarantined runs: %w", err)
	}
	for _, run := range runs {
		if run.Status == string(domain.RunStatusRunning) {
			debt.QuarantinedRunningRuns++
		}
	}

	return debt, nil
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	return keys
}
