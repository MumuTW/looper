package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

const recoveryExecutionQuarantinedEventType = "looperd.recovery.execution_quarantined"

// OutstandingQuarantinedLoop identifies one loop still parked by quarantine.
// It is the counter's roster, not a second source: both come from the same
// query pass over the same durable evidence.
type OutstandingQuarantinedLoop struct {
	LoopID string `json:"loopId"`
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	// Target is the forge target ("owner/repo#123") when the loop has one.
	Target string `json:"target,omitempty"`
	Status string `json:"status"`
	// QuarantinedAt is when recovery first wrote quarantine evidence for this
	// loop's execution.
	QuarantinedAt string `json:"quarantinedAt,omitempty"`
}

// OutstandingQuarantineDebt is live (not startup-snapshot) quarantine/orphan
// debt visible after recovery or live/manual stale reconcile. Quarantine parks
// loops/queues but deliberately leaves agent_executions (and often runs) as
// still-running evidence without process kill.
type OutstandingQuarantineDebt struct {
	// QuarantinedActiveExecutions counts active agent_executions for which
	// recovery wrote explicit quarantine evidence.
	QuarantinedActiveExecutions int `json:"quarantinedActiveExecutions"`
	// QuarantinedRunningRuns counts runs still status=running that are linked to
	// those explicitly quarantined active executions (the activeRuns inflation
	// ops miss).
	QuarantinedRunningRuns int `json:"quarantinedRunningRuns"`
	// Loops names the loops behind those counters so an operator can act
	// without joining event_logs by hand. Ordered by loop seq.
	Loops []OutstandingQuarantinedLoop `json:"loops,omitempty"`
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
	quarantinedAtByExecutionID, err := repositories.Events.ListFirstEventTimestampsByType(ctx, recoveryExecutionQuarantinedEventType, "agent_execution", executionIDs)
	if err != nil {
		return OutstandingQuarantineDebt{}, fmt.Errorf("list quarantine recovery evidence: %w", err)
	}
	if len(quarantinedAtByExecutionID) == 0 {
		return debt, nil
	}
	quarantinedRunIDs := make(map[string]struct{})
	quarantinedAtByLoopID := make(map[string]string)
	for _, execution := range activeExecutions {
		quarantinedAt, quarantined := quarantinedAtByExecutionID[execution.ID]
		if !quarantined {
			continue
		}
		debt.QuarantinedActiveExecutions++
		if execution.RunID != nil && strings.TrimSpace(*execution.RunID) != "" {
			quarantinedRunIDs[strings.TrimSpace(*execution.RunID)] = struct{}{}
		}
		if execution.LoopID == nil || strings.TrimSpace(*execution.LoopID) == "" {
			continue
		}
		loopID := strings.TrimSpace(*execution.LoopID)
		if existing, ok := quarantinedAtByLoopID[loopID]; !ok || (quarantinedAt != "" && quarantinedAt < existing) {
			quarantinedAtByLoopID[loopID] = quarantinedAt
		}
	}

	debt.Loops, err = outstandingQuarantinedLoops(ctx, repositories, quarantinedAtByLoopID)
	if err != nil {
		return OutstandingQuarantineDebt{}, err
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

func outstandingQuarantinedLoops(ctx context.Context, repositories *storage.Repositories, quarantinedAtByLoopID map[string]string) ([]OutstandingQuarantinedLoop, error) {
	if repositories.Loops == nil || len(quarantinedAtByLoopID) == 0 {
		return nil, nil
	}
	loops, err := repositories.Loops.ListByIDs(ctx, mapKeys(quarantinedAtByLoopID))
	if err != nil {
		return nil, fmt.Errorf("list quarantined loops: %w", err)
	}
	roster := make([]OutstandingQuarantinedLoop, 0, len(loops))
	for _, loop := range loops {
		roster = append(roster, OutstandingQuarantinedLoop{
			LoopID:        loop.ID,
			Seq:           loop.Seq,
			Type:          loop.Type,
			Target:        loopForgeTarget(loop),
			Status:        loop.Status,
			QuarantinedAt: quarantinedAtByLoopID[loop.ID],
		})
	}
	sort.Slice(roster, func(i, j int) bool { return roster[i].Seq < roster[j].Seq })
	return roster, nil
}

// loopForgeTarget renders a loop's target as "owner/repo#123" when the loop
// carries one, falling back to the raw target id and then the repo.
func loopForgeTarget(loop storage.LoopRecord) string {
	repo := strings.TrimSpace(derefString(loop.Repo))
	if repo != "" && loop.PRNumber != nil && *loop.PRNumber > 0 {
		return fmt.Sprintf("%s#%d", repo, *loop.PRNumber)
	}
	targetID := strings.TrimSpace(derefString(loop.TargetID))
	if repo != "" && loop.TargetType == string(domain.LoopTargetTypeIssue) {
		if issueNumber, err := parseIssueNumberFromTargetID(targetID); err == nil {
			return fmt.Sprintf("%s#%d", repo, issueNumber)
		}
	}
	if targetID != "" {
		return targetID
	}
	return repo
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	return keys
}
