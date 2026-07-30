package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

type quarantineDebtQueryCounter struct {
	db            *sql.DB
	queryCalls    int
	queryRowCalls int
}

func (q *quarantineDebtQueryCounter) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q *quarantineDebtQueryCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queryCalls++
	return q.db.QueryContext(ctx, query, args...)
}

func (q *quarantineDebtQueryCounter) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.queryRowCalls++
	return q.db.QueryRowContext(ctx, query, args...)
}

func TestCountOutstandingQuarantineDebtUsesConstantQueryShape(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	repos := storage.NewRepositories(coordinator.DB())
	nowISO := formatJavaScriptISOString(time.Date(2026, time.July, 30, 3, 0, 0, 0, time.UTC))
	projectID := "project_query_shape"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Query shape", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	const scale = 32
	entityType := "agent_execution"
	for i := 0; i < scale; i++ {
		loopID := fmt.Sprintf("loop_query_shape_%d", i)
		runID := fmt.Sprintf("run_query_shape_%d", i)
		executionID := fmt.Sprintf("execution_query_shape_%d", i)
		if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: int64(i + 1), ProjectID: projectID, Type: "worker", TargetType: "project", Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Loops.Upsert(%d) error = %v", i, err)
		}
		if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Runs.Upsert(%d) error = %v", i, err)
		}
		pid := int64(8000 + i)
		if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
			ID: executionID, ProjectID: &projectID, LoopID: &loopID, RunID: &runID, Vendor: "codex", Status: "running",
			PID: &pid, CommandJSON: stringPtr(`{"command":"codex"}`), CWD: stringPtr(t.TempDir()),
			StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
		}); err != nil {
			t.Fatalf("AgentExecutions.Upsert(%d) error = %v", i, err)
		}
		if err := repos.Events.Append(ctx, storage.EventLogRecord{
			ID: fmt.Sprintf("event_query_shape_%d", i), EventType: recoveryExecutionQuarantinedEventType,
			ProjectID: &projectID, LoopID: &loopID, RunID: &runID,
			EntityType: &entityType, EntityID: &executionID, PayloadJSON: `{}`, CreatedAt: nowISO,
		}); err != nil {
			t.Fatalf("Events.Append(%d) error = %v", i, err)
		}
	}

	queryCounter := &quarantineDebtQueryCounter{db: coordinator.DB()}
	countedRepos := storage.NewRepositories(queryCounter)
	debt, err := CountOutstandingQuarantineDebt(ctx, countedRepos)
	if err != nil {
		t.Fatalf("CountOutstandingQuarantineDebt() error = %v", err)
	}
	if debt.QuarantinedActiveExecutions != scale || debt.QuarantinedRunningRuns != scale {
		t.Fatalf("debt = %#v, want %d executions and runs", debt, scale)
	}
	if len(debt.Loops) != scale {
		t.Fatalf("debt.Loops = %d entries, want %d from the same pass as the counters", len(debt.Loops), scale)
	}
	// Executions, quarantine evidence, loops, runs — one bulk query each. The
	// roster costs one more bulk query, never one per loop.
	if queryCounter.queryCalls != 4 || queryCounter.queryRowCalls != 0 {
		t.Fatalf("query shape = %d QueryContext + %d QueryRowContext, want 4 bulk queries and no per-row queries", queryCounter.queryCalls, queryCounter.queryRowCalls)
	}
}
