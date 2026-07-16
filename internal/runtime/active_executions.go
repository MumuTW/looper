package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nexu-io/looper/internal/agent"
)

type activeExecution interface {
	Wait(context.Context) (agent.Result, error)
	Kill(string) error
}

type forceKillingExecution interface {
	// ForceKill returns nil only after the owned process group is confirmed no
	// longer signalable. Implementations must not treat signal delivery alone as
	// successful resolution.
	ForceKill() error
}

type activeExecutionEntry struct {
	execution activeExecution
	done      chan struct{}
	cleanup   func()

	mu      sync.Mutex
	waitErr error
}

func (e *activeExecutionEntry) finish(err error) {
	e.mu.Lock()
	e.waitErr = err
	e.mu.Unlock()
	close(e.done)
}

func (e *activeExecutionEntry) err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.waitErr
}

type activeRunEntry struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

type ActiveExecutionRegistry struct {
	mu             sync.Mutex
	executions     map[string]*activeExecutionEntry
	runs           map[string]*activeRunEntry
	pendingStarts  int
	pendingByLoop  map[string]int
	stoppingLoops  map[string]int
	closing        bool
	stopReason     string
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	stateChanged   chan struct{}
}

func NewActiveExecutionRegistry() *ActiveExecutionRegistry {
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &ActiveExecutionRegistry{
		executions:     make(map[string]*activeExecutionEntry),
		runs:           make(map[string]*activeRunEntry),
		pendingByLoop:  make(map[string]int),
		stoppingLoops:  make(map[string]int),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		stateChanged:   make(chan struct{}),
	}
}

// BeginLoopStop prevents new run reservations and agent starts for loopID. The
// caller must hold the returned lease through its durable pause/terminate/
// takeover transition so the scheduler cannot publish a replacement between
// the final empty registry snapshot and that state change.
func (r *ActiveExecutionRegistry) BeginLoopStop(loopID string) func() {
	if r == nil || loopID == "" {
		return func() {}
	}
	r.mu.Lock()
	r.stoppingLoops[loopID]++
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.stoppingLoops[loopID] <= 1 {
				delete(r.stoppingLoops, loopID)
			} else {
				r.stoppingLoops[loopID]--
			}
			r.notifyStateChangedLocked()
			r.mu.Unlock()
		})
	}
}

// BeginShutdown closes the spawn boundary before scheduler cancellation. Any
// registered-start lease already in progress remains visible as pending until
// it either fails or atomically publishes its live handle; later starts are
// rejected before invoking the subprocess starter.
func (r *ActiveExecutionRegistry) BeginShutdown(reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.closing = true
	r.stopReason = reason
	cancel := r.shutdownCancel
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// IsClosing reports whether BeginShutdown has closed the spawn boundary.
// Callers that race RegisterRun against shutdown use this to decide whether
// unstarted claimed work should be requeued for restart recovery.
func (r *ActiveExecutionRegistry) IsClosing() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closing
}

// StartAgentExecution acquires an ownership lease before invoking Start. The
// lease closes the spawn-to-register gap: shutdown cannot observe an empty
// registry while a starter may already have created a child process.
func (r *ActiveExecutionRegistry) StartAgentExecution(ctx context.Context, loopID, runID, executionID string, starter agentExecutionStarter, input agent.RunInput) (agent.Execution, error) {
	if starter == nil {
		return nil, fmt.Errorf("agent execution starter is not configured")
	}
	if r == nil {
		return starter.Start(ctx, input)
	}

	r.mu.Lock()
	if r.closing || r.stoppingLoops[loopID] > 0 {
		r.mu.Unlock()
		return nil, context.Canceled
	}
	r.pendingStarts++
	r.pendingByLoop[loopID]++
	shutdownCtx := r.shutdownCtx
	r.notifyStateChangedLocked()
	r.mu.Unlock()

	startCtx, cancelStart := context.WithCancel(ctx)
	stopShutdownCancel := context.AfterFunc(shutdownCtx, cancelStart)
	cleanupStartContext := func() {
		stopShutdownCancel()
		cancelStart()
	}

	execution, err := starter.Start(startCtx, input)
	if err != nil {
		cleanupStartContext()
		r.finishPendingStart(loopID)
		return nil, err
	}
	if execution == nil {
		cleanupStartContext()
		r.finishPendingStart(loopID)
		return nil, fmt.Errorf("agent execution starter returned a nil execution")
	}

	key := activeExecutionKey(loopID, runID, executionID)
	entry := &activeExecutionEntry{execution: execution, done: make(chan struct{}), cleanup: cleanupStartContext}
	r.mu.Lock()
	r.pendingStarts--
	r.decrementPendingLoopLocked(loopID)
	r.executions[key] = entry
	closing := r.closing
	reason := r.stopReason
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	r.waitForExecution(key, entry)
	if closing {
		_ = execution.Kill(reason)
	}
	return execution, nil
}

func (r *ActiveExecutionRegistry) finishPendingStart(loopID string) {
	r.mu.Lock()
	if r.pendingStarts > 0 {
		r.pendingStarts--
	}
	r.decrementPendingLoopLocked(loopID)
	r.notifyStateChangedLocked()
	r.mu.Unlock()
}

func (r *ActiveExecutionRegistry) decrementPendingLoopLocked(loopID string) {
	if r.pendingByLoop[loopID] <= 1 {
		delete(r.pendingByLoop, loopID)
		return
	}
	r.pendingByLoop[loopID]--
}

func (r *ActiveExecutionRegistry) waitForExecution(key string, entry *activeExecutionEntry) {
	go func() {
		_, err := entry.execution.Wait(context.Background())
		if entry.cleanup != nil {
			entry.cleanup()
		}
		entry.finish(err)
		r.mu.Lock()
		// A Wait error means the live handle did not confirm process-group
		// reaping. Keep that authority registered so bounded shutdown can invoke
		// its forced-resolution contract instead of losing the only safe handle.
		if err == nil && r.executions[key] == entry {
			delete(r.executions, key)
		}
		r.notifyStateChangedLocked()
		r.mu.Unlock()
	}()
}

// Register makes the live executor handle authoritative until its Wait method
// confirms that the process has been reaped. The registry owns one background
// waiter so callers can safely stop or shut down even after a role runner's
// context has been cancelled. The returned release function is retained for
// caller compatibility, but cannot revoke ownership before Wait resolves.
func (r *ActiveExecutionRegistry) Register(loopID, runID, executionID string, execution activeExecution) func() {
	if r == nil || execution == nil {
		return func() {}
	}
	key := activeExecutionKey(loopID, runID, executionID)
	entry := &activeExecutionEntry{execution: execution, done: make(chan struct{})}

	r.mu.Lock()
	if r.closing || r.stoppingLoops[loopID] > 0 {
		reason := r.stopReason
		r.mu.Unlock()
		// The process was spawned across the shutdown boundary. Resolve that
		// ownership synchronously so a registration rejected after the registry
		// became empty cannot escape as an unowned child process.
		_ = execution.Kill(reason)
		if force, ok := execution.(forceKillingExecution); ok {
			_ = force.ForceKill()
		}
		_, _ = execution.Wait(context.Background())
		return func() {}
	}
	r.executions[key] = entry
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	r.waitForExecution(key, entry)

	return func() {}
}

// RegisterRun owns the cancellable context for the whole scheduled role run,
// including work performed after the coding-agent process exits (for example,
// validation commands). A loop stop therefore cannot lose ownership in the gap
// between agent completion and runner completion. As with Register, the returned
// release function cannot revoke ownership before done closes.
func (r *ActiveExecutionRegistry) RegisterRun(loopID, ownerID string, cancel context.CancelFunc, done <-chan struct{}) (func(), bool) {
	if r == nil || cancel == nil || done == nil {
		return func() {}, false
	}
	key := activeRunKey(loopID, ownerID)
	entry := &activeRunEntry{cancel: cancel, done: done}
	r.mu.Lock()
	if r.closing || r.stoppingLoops[loopID] > 0 {
		r.mu.Unlock()
		// RegisterRun is called before a scheduled role starts. Rejecting the
		// ownership reservation by cancelling its context prevents a new process
		// from being spawned after shutdown has crossed the empty boundary.
		cancel()
		return func() {}, false
	}
	r.runs[key] = entry
	r.notifyStateChangedLocked()
	r.mu.Unlock()
	go func() {
		<-done
		r.mu.Lock()
		if r.runs[key] == entry {
			delete(r.runs, key)
		}
		r.notifyStateChangedLocked()
		r.mu.Unlock()
	}()
	return func() {}, true
}

// StopAndWait stops every live execution, pending start, and enclosing
// scheduled run for the loop. Callers performing a durable lifecycle
// transition must hold BeginLoopStop's lease across this call and the state
// change. A true result means live or pending ownership existed and resolved.
func (r *ActiveExecutionRegistry) StopAndWait(ctx context.Context, loopID, _ string, _ string, reason string) (bool, error) {
	if r == nil {
		return false, nil
	}
	found := false
	var errList []error
	seenExecutions := make(map[*activeExecutionEntry]struct{})
	seenRuns := make(map[*activeRunEntry]struct{})
	for {
		r.mu.Lock()
		executions := r.executionsForLoopLocked(loopID)
		runs := r.runsForLoopLocked(loopID)
		pendingStarts := r.pendingByLoop[loopID]
		stateChanged := r.stateChanged
		r.mu.Unlock()

		if len(executions) == 0 && len(runs) == 0 && pendingStarts == 0 {
			return found, errors.Join(errList...)
		}
		found = true
		for _, execution := range executions {
			if _, seen := seenExecutions[execution]; seen {
				continue
			}
			seenExecutions[execution] = struct{}{}
			if err := execution.execution.Kill(reason); err != nil {
				errList = append(errList, fmt.Errorf("kill active execution: %w", err))
			}
		}
		for _, run := range runs {
			if _, seen := seenRuns[run]; seen {
				continue
			}
			seenRuns[run] = struct{}{}
			run.cancel()
		}
		if len(errList) > 0 {
			return true, errors.Join(errList...)
		}
		for _, execution := range executions {
			if err := waitForActiveDone(ctx, execution.done); err != nil {
				errList = append(errList, fmt.Errorf("wait for active execution reap: %w", err))
				return true, errors.Join(errList...)
			}
			if err := execution.err(); err != nil {
				errList = append(errList, fmt.Errorf("wait for active execution reap: %w", err))
			}
		}
		for _, run := range runs {
			if err := waitForActiveDone(ctx, run.done); err != nil {
				errList = append(errList, fmt.Errorf("wait for active run completion: %w", err))
				return true, errors.Join(errList...)
			}
		}
		if len(executions) == 0 && len(runs) == 0 && pendingStarts > 0 {
			if err := waitForActiveDone(ctx, stateChanged); err != nil {
				errList = append(errList, fmt.Errorf("wait for pending loop execution start: %w", err))
				return true, errors.Join(errList...)
			}
		}
		if len(errList) > 0 {
			return true, errors.Join(errList...)
		}
	}
}

// Kill preserves the existing non-blocking registry surface for callers that
// only need to request cancellation. Lifecycle transitions should use
// StopAndWait so process reaping remains part of the contract.
func (r *ActiveExecutionRegistry) Kill(loopID, runID, executionID, reason string) (bool, error) {
	if r == nil {
		return false, nil
	}
	key := activeExecutionKey(loopID, runID, executionID)
	r.mu.Lock()
	entry := r.executions[key]
	r.mu.Unlock()
	if entry == nil {
		return false, nil
	}
	return true, entry.execution.Kill(reason)
}

// ShutdownAndWait prevents newly registered work from escaping shutdown,
// requests cancellation for all current ownership records, and waits until the
// registry is empty or ctx expires.
func (r *ActiveExecutionRegistry) ShutdownAndWait(ctx context.Context, reason string) error {
	if r == nil {
		return nil
	}
	r.BeginShutdown(reason)

	seenExecutions := make(map[*activeExecutionEntry]struct{})
	seenRuns := make(map[*activeRunEntry]struct{})
	var killErrs []error
	for {
		r.mu.Lock()
		executions := make([]*activeExecutionEntry, 0, len(r.executions))
		for _, entry := range r.executions {
			executions = append(executions, entry)
		}
		runs := make([]*activeRunEntry, 0, len(r.runs))
		for _, entry := range r.runs {
			runs = append(runs, entry)
		}
		pendingStarts := r.pendingStarts
		stateChanged := r.stateChanged
		r.mu.Unlock()

		if len(executions) == 0 && len(runs) == 0 && pendingStarts == 0 {
			return nil
		}
		for _, entry := range executions {
			if _, seen := seenExecutions[entry]; !seen {
				seenExecutions[entry] = struct{}{}
				if err := entry.execution.Kill(reason); err != nil {
					killErrs = append(killErrs, fmt.Errorf("kill active execution during shutdown: %w", err))
				}
			}
		}
		for _, entry := range runs {
			if _, seen := seenRuns[entry]; !seen {
				seenRuns[entry] = struct{}{}
				entry.cancel()
			}
		}
		for _, entry := range executions {
			if err := waitForActiveDone(ctx, entry.done); err != nil {
				return fmt.Errorf("wait for active execution during shutdown: %w", err)
			}
			if err := entry.err(); err != nil {
				return fmt.Errorf("wait for active execution during shutdown: %w", err)
			}
		}
		for _, entry := range runs {
			if err := waitForActiveDone(ctx, entry.done); err != nil {
				return fmt.Errorf("wait for active run during shutdown: %w", err)
			}
		}
		if len(executions) == 0 && len(runs) == 0 && pendingStarts > 0 {
			if err := waitForActiveDone(ctx, stateChanged); err != nil {
				return fmt.Errorf("wait for pending agent start during shutdown: %w", err)
			}
		}
		if len(killErrs) > 0 {
			return errors.Join(killErrs...)
		}
	}
}

// ForceKillAndWait is the bounded-shutdown escalation path. A nil ForceKill
// result is the live handle's confirmation that process-group ownership has
// resolved, not merely that a signal was sent. This lets the registry safely
// retire a handle whose earlier Wait returned a failed-reap diagnostic.
func (r *ActiveExecutionRegistry) ForceKillAndWait(ctx context.Context) error {
	if r == nil {
		return nil
	}
	type keyedExecution struct {
		key      string
		entry    *activeExecutionEntry
		forceErr error
	}
	type keyedRun struct {
		key   string
		entry *activeRunEntry
	}
	for {
		r.mu.Lock()
		executions := make([]keyedExecution, 0, len(r.executions))
		for key, entry := range r.executions {
			executions = append(executions, keyedExecution{key: key, entry: entry})
		}
		runs := make([]keyedRun, 0, len(r.runs))
		for key, entry := range r.runs {
			runs = append(runs, keyedRun{key: key, entry: entry})
		}
		pendingStarts := r.pendingStarts
		stateChanged := r.stateChanged
		r.mu.Unlock()
		if len(executions) == 0 && len(runs) == 0 && pendingStarts == 0 {
			return nil
		}
		for index := range executions {
			force, ok := executions[index].entry.execution.(forceKillingExecution)
			if !ok {
				executions[index].forceErr = fmt.Errorf("active execution does not support forced process-group termination")
				continue
			}
			if err := force.ForceKill(); err != nil {
				executions[index].forceErr = fmt.Errorf("force kill active execution: %w", err)
			}
		}
		for _, run := range runs {
			run.entry.cancel()
		}
		var errList []error
		for _, execution := range executions {
			if err := waitForActiveDone(ctx, execution.entry.done); err != nil {
				errList = append(errList, fmt.Errorf("wait for force-killed execution reap: %w", err))
				continue
			}
			waitErr := execution.entry.err()
			if execution.forceErr != nil {
				// ForceKill did not confirm process-group resolution. Keep the
				// live handle and surface both failures.
				if waitErr != nil {
					errList = append(errList, execution.forceErr, fmt.Errorf("wait for force-killed execution reap: %w", waitErr))
				} else {
					errList = append(errList, execution.forceErr)
				}
				continue
			}
			// ForceKill confirmed the process group is gone, so process ownership
			// can be retired. Preserve a non-nil Wait error (for example a
			// terminal persistFinal failure) so shutdown cannot treat durable
			// status as cleanly closed while the agent_executions row remains
			// running/cancelling.
			r.mu.Lock()
			if r.executions[execution.key] == execution.entry {
				delete(r.executions, execution.key)
				r.notifyStateChangedLocked()
			}
			r.mu.Unlock()
			if waitErr != nil {
				errList = append(errList, fmt.Errorf("wait for force-killed execution reap: %w", waitErr))
			}
		}
		for _, run := range runs {
			if err := waitForActiveDone(ctx, run.entry.done); err != nil {
				errList = append(errList, fmt.Errorf("wait for cancelled active run: %w", err))
				continue
			}
			r.mu.Lock()
			if r.runs[run.key] == run.entry {
				delete(r.runs, run.key)
				r.notifyStateChangedLocked()
			}
			r.mu.Unlock()
		}
		if len(executions) == 0 && len(runs) == 0 && pendingStarts > 0 {
			if err := waitForActiveDone(ctx, stateChanged); err != nil {
				errList = append(errList, fmt.Errorf("wait for pending agent start before force reap: %w", err))
			}
		}
		if len(errList) > 0 {
			return errors.Join(errList...)
		}
	}
}

func (r *ActiveExecutionRegistry) notifyStateChangedLocked() {
	if r.stateChanged == nil {
		r.stateChanged = make(chan struct{})
		return
	}
	close(r.stateChanged)
	r.stateChanged = make(chan struct{})
}

func (r *ActiveExecutionRegistry) runsForLoopLocked(loopID string) []*activeRunEntry {
	prefix := loopID + "\x00"
	runs := make([]*activeRunEntry, 0)
	for key, entry := range r.runs {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			runs = append(runs, entry)
		}
	}
	return runs
}

func (r *ActiveExecutionRegistry) executionsForLoopLocked(loopID string) []*activeExecutionEntry {
	prefix := loopID + "\x00"
	executions := make([]*activeExecutionEntry, 0)
	for key, entry := range r.executions {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			executions = append(executions, entry)
		}
	}
	return executions
}

func waitForActiveDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func activeExecutionKey(loopID, runID, executionID string) string {
	return loopID + "\x00" + runID + "\x00" + executionID
}

func activeRunKey(loopID, ownerID string) string {
	return loopID + "\x00" + ownerID
}
