package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
)

func TestActiveExecutionRegistryStopAndWaitRequiresExecutionAndRunCompletion(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newBlockingRegistryExecution()
	registry.Register("loop_1", "run_1", "exec_1", execution)
	runDone := make(chan struct{})
	runCancelled := make(chan struct{})
	var cancelOnce sync.Once
	_, accepted := registry.RegisterRun("loop_1", "queue_1", func() {
		cancelOnce.Do(func() { close(runCancelled) })
	}, runDone)
	if !accepted {
		t.Fatal("RegisterRun rejected before shutdown")
	}

	returned := make(chan error, 1)
	go func() {
		_, err := registry.StopAndWait(context.Background(), "loop_1", "run_1", "exec_1", "stop test")
		returned <- err
	}()

	select {
	case <-execution.killed:
	case <-time.After(time.Second):
		t.Fatal("execution was not killed")
	}
	select {
	case <-runCancelled:
	case <-time.After(time.Second):
		t.Fatal("run context was not cancelled")
	}
	execution.finish()
	select {
	case err := <-returned:
		t.Fatalf("StopAndWait returned before run completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(runDone)
	if err := <-returned; err != nil {
		t.Fatalf("StopAndWait() error = %v", err)
	}
}

func TestActiveExecutionRegistryShutdownKillsLateRegistrationAndWaitsForReap(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	first := newBlockingRegistryExecution()
	registry.Register("loop_1", "run_1", "exec_1", first)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- registry.ShutdownAndWait(ctx, "shutdown") }()
	select {
	case <-first.killed:
	case <-time.After(time.Second):
		t.Fatal("first execution was not killed")
	}

	late := newBlockingRegistryExecution()
	registry.Register("loop_2", "run_2", "exec_2", late)
	select {
	case <-late.killed:
	case <-time.After(time.Second):
		t.Fatal("late execution escaped registry shutdown")
	}
	first.finish()
	late.finish()
	if err := <-returned; err != nil {
		t.Fatalf("ShutdownAndWait() error = %v", err)
	}
}

func TestActiveExecutionRegistryForceKillWaitsForReap(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newBlockingRegistryExecution()
	registry.Register("loop_1", "run_1", "exec_1", execution)
	if err := registry.ForceKillAndWait(context.Background()); err != nil {
		t.Fatalf("ForceKillAndWait() error = %v", err)
	}
	select {
	case <-execution.forceKilled:
	default:
		t.Fatal("ForceKill was not invoked")
	}
}

func TestActiveExecutionRegistryForceKillPreservesTerminalWaitError(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newTerminalPersistFailureRegistryExecution()
	registry.Register("loop_persist", "run_persist", "exec_persist", execution)
	<-execution.waited

	// Graceful shutdown keeps the live handle while durable terminal status
	// remains unwritten.
	if _, err := registry.StopAndWait(context.Background(), "loop_persist", "", "", "stop"); !errors.Is(err, errRegistryTerminalPersist) {
		t.Fatalf("StopAndWait() error = %v, want terminal persist failure", err)
	}

	// ForceKill confirms the process group is gone and retires the handle, but
	// must still surface the terminal Wait error so shutdown cannot treat the
	// durable agent_executions row as cleanly closed.
	if err := registry.ForceKillAndWait(context.Background()); !errors.Is(err, errRegistryTerminalPersist) {
		t.Fatalf("ForceKillAndWait() error = %v, want terminal persist failure preserved", err)
	}
	select {
	case <-execution.forceKilled:
	default:
		t.Fatal("ForceKill was not invoked")
	}
	if err := registry.ForceKillAndWait(context.Background()); err != nil {
		t.Fatalf("second ForceKillAndWait() error = %v after ownership retired", err)
	}
}

func TestActiveExecutionRegistryRejectsExecutionRegisteredAfterShutdownBoundary(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	if err := registry.ShutdownAndWait(context.Background(), "shutdown"); err != nil {
		t.Fatalf("ShutdownAndWait() error = %v", err)
	}

	execution := newBlockingRegistryExecution()
	registry.Register("loop_late", "run_late", "exec_late", execution)
	select {
	case <-execution.done:
	default:
		t.Fatal("late Register returned before the rejected execution was reaped")
	}
	select {
	case <-execution.killed:
	default:
		t.Fatal("late execution did not receive graceful cancellation")
	}
	select {
	case <-execution.forceKilled:
	default:
		t.Fatal("late execution did not receive forced process-group termination")
	}
}

func TestActiveExecutionRegistryShutdownWaitsForPendingStartLease(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	starter := &gatedRegistryStarter{started: make(chan struct{}), release: make(chan struct{})}
	startReturned := make(chan error, 1)
	go func() {
		_, err := registry.StartAgentExecution(context.Background(), "loop_pending", "run_pending", "exec_pending", starter, agent.RunInput{ExecutionID: "exec_pending"})
		startReturned <- err
	}()
	<-starter.started

	shutdownReturned := make(chan error, 1)
	go func() { shutdownReturned <- registry.ShutdownAndWait(context.Background(), "shutdown") }()
	select {
	case err := <-shutdownReturned:
		t.Fatalf("ShutdownAndWait returned across a pending start lease: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(starter.release)
	if err := <-startReturned; !errors.Is(err, context.Canceled) {
		t.Fatalf("StartAgentExecution() error = %v, want context canceled", err)
	}
	if err := <-shutdownReturned; err != nil {
		t.Fatalf("ShutdownAndWait() error = %v", err)
	}
}

func TestActiveExecutionRegistryRejectsRunAfterShutdownBoundary(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	registry.BeginShutdown("shutdown")
	cancelled := make(chan struct{})
	var once sync.Once
	_, accepted := registry.RegisterRun("loop_late", "queue_late", func() {
		once.Do(func() { close(cancelled) })
	}, make(chan struct{}))
	if accepted {
		t.Fatal("RegisterRun accepted work after shutdown began")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("RegisterRun did not cancel rejected work")
	}
}

func TestActiveExecutionRegistryLoopStopLeaseRejectsReplacementUntilDurableTransition(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newBlockingRegistryExecution()
	registry.Register("loop_stop", "run_old", "exec_old", execution)
	releaseLoopStop := registry.BeginLoopStop("loop_stop")
	returned := make(chan error, 1)
	go func() {
		_, err := registry.StopAndWait(context.Background(), "loop_stop", "run_old", "exec_old", "stop")
		returned <- err
	}()
	select {
	case <-execution.killed:
	case <-time.After(time.Second):
		t.Fatal("existing execution was not killed")
	}

	assertRunRejected := func(owner string) {
		t.Helper()
		cancelled := make(chan struct{})
		var once sync.Once
		_, accepted := registry.RegisterRun("loop_stop", owner, func() { once.Do(func() { close(cancelled) }) }, make(chan struct{}))
		if accepted {
			t.Fatalf("RegisterRun(%q) accepted while loop stop lease was held", owner)
		}
		select {
		case <-cancelled:
		default:
			t.Fatalf("RegisterRun(%q) did not cancel rejected run", owner)
		}
	}
	assertRunRejected("queue_during_reap")
	starter := &countingRegistryStarter{}
	if _, err := registry.StartAgentExecution(context.Background(), "loop_stop", "run_new", "exec_new", starter, agent.RunInput{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("StartAgentExecution() error = %v, want context canceled", err)
	}
	if starter.calls != 0 {
		t.Fatalf("underlying starter calls = %d, want 0 while loop stop lease held", starter.calls)
	}

	execution.finish()
	if err := <-returned; err != nil {
		t.Fatalf("StopAndWait() error = %v", err)
	}
	// StopAndWait has reached an empty snapshot, but the caller has not yet made
	// its durable transition; the lease must still close that final race.
	assertRunRejected("queue_before_transition")
	releaseLoopStop()

	done := make(chan struct{})
	close(done)
	unregister, accepted := registry.RegisterRun("loop_stop", "queue_after_release", func() {}, done)
	if !accepted {
		t.Fatal("RegisterRun rejected after loop stop lease was released")
	}
	unregister()
}

type blockingRegistryExecution struct {
	done        chan struct{}
	killed      chan struct{}
	forceKilled chan struct{}
	finishOnce  sync.Once
	killOnce    sync.Once
	forceOnce   sync.Once
}

var errRegistryTerminalPersist = errors.New("persist terminal agent execution status")

// terminalPersistFailureRegistryExecution models Wait after process reaping when
// the durable agent_executions terminal Upsert still failed.
type terminalPersistFailureRegistryExecution struct {
	waited      chan struct{}
	forceKilled chan struct{}
	waitOnce    sync.Once
	forceOnce   sync.Once
}

func newTerminalPersistFailureRegistryExecution() *terminalPersistFailureRegistryExecution {
	return &terminalPersistFailureRegistryExecution{
		waited:      make(chan struct{}),
		forceKilled: make(chan struct{}),
	}
}

func (e *terminalPersistFailureRegistryExecution) Wait(context.Context) (agent.Result, error) {
	e.waitOnce.Do(func() { close(e.waited) })
	return agent.Result{Status: "killed"}, errRegistryTerminalPersist
}

func (e *terminalPersistFailureRegistryExecution) Kill(string) error { return nil }

func (e *terminalPersistFailureRegistryExecution) ForceKill() error {
	e.forceOnce.Do(func() { close(e.forceKilled) })
	return nil
}

type gatedRegistryStarter struct {
	started chan struct{}
	release chan struct{}
}

type countingRegistryStarter struct{ calls int }

func (s *countingRegistryStarter) Start(context.Context, agent.RunInput) (agent.Execution, error) {
	s.calls++
	return newBlockingRegistryExecution(), nil
}

func (s *gatedRegistryStarter) Start(ctx context.Context, _ agent.RunInput) (agent.Execution, error) {
	close(s.started)
	<-s.release
	return nil, ctx.Err()
}

func newBlockingRegistryExecution() *blockingRegistryExecution {
	return &blockingRegistryExecution{done: make(chan struct{}), killed: make(chan struct{}), forceKilled: make(chan struct{})}
}

func (e *blockingRegistryExecution) Wait(ctx context.Context) (agent.Result, error) {
	select {
	case <-e.done:
		return agent.Result{Status: "killed"}, nil
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
}

func (e *blockingRegistryExecution) Kill(string) error {
	e.killOnce.Do(func() { close(e.killed) })
	return nil
}

func (e *blockingRegistryExecution) ForceKill() error {
	e.forceOnce.Do(func() { close(e.forceKilled) })
	e.finish()
	return nil
}

func (e *blockingRegistryExecution) finish() {
	e.finishOnce.Do(func() { close(e.done) })
}
