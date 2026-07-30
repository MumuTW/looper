package runtime

import (
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

// executionLivenessLeaseTTL bounds how long a run's own heartbeat keeps it out
// of live stale-run reconciliation. It is not containment Authority.
const executionLivenessLeaseTTL = 30 * time.Minute

// ContainmentClass is the startup-recovery classification of durable execution
// evidence after a daemon restart (ADR-0015 R8, revised for #149).
//
// There are two classes, and neither is an inference about a process. Recovery
// asks one question — "does this daemon own this execution right now?" — and
// answers it from its own in-memory handle registry. Everything else is
// confirmed dead, because containment is enforced by retiring the execution's
// worktree generation rather than by proving the old process exited.
//
// PID/PGID inspection is not consulted at all any more. It never could
// authorize confirmed-dead (descendants outlive leaders, PIDs are reused), and
// once the fence moved to the filesystem path it stopped being needed for the
// negative case either.
type ContainmentClass string

const (
	// ContainmentConfirmedDead means Authority exists to treat the execution as
	// non-runnable for recovery purposes. Authorized by:
	//   - durable terminal finalization already committed before the crash, or
	//   - a current-daemon owned processcontainment.Handle that has completed
	//     confirmed drain, or
	//   - the execution's worktree generation has been durably retired, so any
	//     surviving writer is confined to a directory no daemon reads, pushes
	//     from, or cleans.
	ContainmentConfirmedDead ContainmentClass = "confirmed_dead"

	// ContainmentCurrentDaemonOwned means this daemon holds a live supervisor
	// handle for the execution. It is the only class recovery must leave alone:
	// the work is genuinely in flight in this process.
	ContainmentCurrentDaemonOwned ContainmentClass = "still_owned_by_this_daemon"
)

// ContainmentClassification is one classified durable observation.
type ContainmentClassification struct {
	Class ContainmentClass
	// Reason is a stable machine-oriented explanation (event payloads / tests).
	Reason string
	// PID is the durable PID when present (evidence only, never Authority).
	PID int
}

// durableTerminalExecution reports whether SQLite already holds a terminal
// finalization for the row. Active statuses are never confirmed-dead by status.
func durableTerminalExecution(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "failed", "timeout", "killed", "success":
		return true
	default:
		return false
	}
}

// classifyFromDurableStatusAndHandle applies the two confirmed-dead Authority
// rules that need no knowledge of the worktree. currentDaemonHandle may be nil
// (always after a crash — pre-crash handles do not exist).
func classifyFromDurableStatusAndHandle(execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle) (ContainmentClassification, bool) {
	pid := executionPID(execution)
	if durableTerminalExecution(execution.Status) {
		return ContainmentClassification{
			Class:  ContainmentConfirmedDead,
			Reason: "durable_terminal_finalization",
			PID:    pid,
		}, true
	}
	if currentDaemonHandle != nil && currentDaemonHandle.ConfirmedDead() {
		return ContainmentClassification{
			Class:  ContainmentConfirmedDead,
			Reason: "current_daemon_confirmed_drain",
			PID:    pid,
		}, true
	}
	return ContainmentClassification{}, false
}

// classifyDurableExecution is the whole classifier. currentDaemonOwnsLiveHandle
// is the only live signal; it comes from this process's own handle registry,
// not from the operating system.
func classifyDurableExecution(execution storage.AgentExecutionRecord, currentDaemonHandle *processcontainment.Handle, currentDaemonOwnsLiveHandle bool) ContainmentClassification {
	if classification, ok := classifyFromDurableStatusAndHandle(execution, currentDaemonHandle); ok {
		return classification
	}
	if currentDaemonOwnsLiveHandle {
		return ContainmentClassification{
			Class:  ContainmentCurrentDaemonOwned,
			Reason: "current_daemon_supervisor_handle",
			PID:    executionPID(execution),
		}
	}
	// No handle in this daemon: whatever started this row belongs to a previous
	// generation. Retiring its worktree generation is what makes that safe, and
	// the caller does that before acting on this classification.
	return ContainmentClassification{
		Class:  ContainmentConfirmedDead,
		Reason: "stale_generation_retired",
		PID:    executionPID(execution),
	}
}

func executionPID(execution storage.AgentExecutionRecord) int {
	if execution.PID != nil && *execution.PID > 0 {
		return int(*execution.PID)
	}
	return 0
}

// classificationAllowsTerminalOrRequeue is true only for confirmed-dead. Work
// this daemon still owns must not be marked terminal, requeued, or overlapped.
func classificationAllowsTerminalOrRequeue(class ContainmentClass) bool {
	return class == ContainmentConfirmedDead
}
