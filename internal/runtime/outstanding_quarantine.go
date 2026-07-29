package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

// OutstandingQuarantineDebt is live (not startup-snapshot) quarantine/orphan
// debt visible after recovery or live/manual stale reconcile. Quarantine parks
// loops/queues but deliberately leaves agent_executions (and often runs) as
// still-running evidence without process kill.
type OutstandingQuarantineDebt struct {
	// QuarantinedActiveExecutions counts active agent_executions tied to parked
	// work (paused loop and/or manual_intervention queue).
	QuarantinedActiveExecutions int `json:"quarantinedActiveExecutions"`
	// QuarantinedRunningRuns counts runs still status=running that are linked to
	// those quarantined active executions (the activeRuns inflation ops miss).
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

	loopCache := map[string]*storage.LoopRecord{}
	queueCache := map[string]*storage.QueueItemRecord{}
	runningRunIDs := map[string]struct{}{}

	for _, execution := range activeExecutions {
		parked, err := executionLooksQuarantined(ctx, repositories, execution, loopCache, queueCache)
		if err != nil {
			return OutstandingQuarantineDebt{}, err
		}
		if !parked {
			continue
		}
		debt.QuarantinedActiveExecutions++
		if execution.RunID == nil {
			continue
		}
		runID := strings.TrimSpace(*execution.RunID)
		if runID == "" {
			continue
		}
		if _, seen := runningRunIDs[runID]; seen {
			continue
		}
		if repositories.Runs == nil {
			continue
		}
		run, err := repositories.Runs.GetByID(ctx, runID)
		if err != nil {
			return OutstandingQuarantineDebt{}, fmt.Errorf("get run %s: %w", runID, err)
		}
		if run == nil || run.Status != string(domain.RunStatusRunning) {
			continue
		}
		runningRunIDs[runID] = struct{}{}
		debt.QuarantinedRunningRuns++
	}

	return debt, nil
}

func executionLooksQuarantined(
	ctx context.Context,
	repositories *storage.Repositories,
	execution storage.AgentExecutionRecord,
	loopCache map[string]*storage.LoopRecord,
	queueCache map[string]*storage.QueueItemRecord,
) (bool, error) {
	if execution.LoopID == nil {
		return false, nil
	}
	loopID := strings.TrimSpace(*execution.LoopID)
	if loopID == "" {
		return false, nil
	}

	if repositories.Loops != nil {
		loop, ok := loopCache[loopID]
		if !ok {
			got, err := repositories.Loops.GetByID(ctx, loopID)
			if err != nil {
				return false, fmt.Errorf("get loop %s: %w", loopID, err)
			}
			loop = got
			loopCache[loopID] = got
		}
		if loop != nil && loop.Status == "paused" {
			return true, nil
		}
	}

	if repositories.Queue == nil {
		return false, nil
	}
	queue, ok := queueCache[loopID]
	if !ok {
		got, err := repositories.Queue.GetLatestByLoopID(ctx, loopID)
		if err != nil {
			return false, fmt.Errorf("get latest queue for loop %s: %w", loopID, err)
		}
		queue = got
		queueCache[loopID] = got
	}
	return queue != nil && queue.Status == "manual_intervention", nil
}
