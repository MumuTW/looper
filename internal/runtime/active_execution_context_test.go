package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
)

func TestStartAgentExecutionKeepsRunContextAliveUntilShutdown(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	starter := &contextObservingRegistryStarter{}
	handle, err := registry.StartAgentExecution(
		context.Background(),
		"loop_context",
		"run_context",
		"exec_context",
		starter,
		agent.RunInput{ExecutionID: "exec_context"},
	)
	if err != nil {
		t.Fatalf("StartAgentExecution() error = %v", err)
	}
	if handle == nil || starter.execution == nil {
		t.Fatal("StartAgentExecution() did not publish a live execution")
	}

	select {
	case <-starter.execution.ctx.Done():
		t.Fatalf("execution context was cancelled when the live handle was published: %v", starter.execution.ctx.Err())
	case <-time.After(20 * time.Millisecond):
	}

	if err := registry.ShutdownAndWait(context.Background(), "test shutdown"); err != nil {
		t.Fatalf("ShutdownAndWait() error = %v", err)
	}
	select {
	case <-starter.execution.ctx.Done():
		if err := starter.execution.ctx.Err(); err == nil {
			t.Fatal("execution context completed during shutdown without a cancellation error")
		}
	default:
		t.Fatal("shutdown returned before cancelling the published execution context")
	}
}

func TestRegisterReleaseCannotDropLiveExecutionOwnership(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newBlockingRegistryExecution()
	release := registry.Register("loop_release", "run_release", "exec_release", execution)
	release()

	stopped := make(chan struct {
		found bool
		err   error
	}, 1)
	go func() {
		found, err := registry.StopAndWait(context.Background(), "loop_release", "run_release", "exec_release", "test stop")
		stopped <- struct {
			found bool
			err   error
		}{found: found, err: err}
	}()

	select {
	case <-execution.killed:
	case result := <-stopped:
		t.Fatalf("StopAndWait() returned before stopping the released live execution: found=%v err=%v", result.found, result.err)
	case <-time.After(time.Second):
		t.Fatal("StopAndWait() did not retain authority over the released live execution")
	}
	execution.finish()
	result := <-stopped
	if result.err != nil || !result.found {
		t.Fatalf("StopAndWait() = (%v, %v), want (true, nil)", result.found, result.err)
	}
}

func TestRegisterRunReleaseCannotDropLiveRunOwnership(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	runDone := make(chan struct{})
	runCancelled := make(chan struct{})
	var cancelOnce sync.Once
	release, accepted := registry.RegisterRun("loop_run_release", "owner_release", func() {
		cancelOnce.Do(func() { close(runCancelled) })
	}, runDone)
	if !accepted {
		t.Fatal("RegisterRun() rejected live run before shutdown")
	}
	release()

	stopped := make(chan struct {
		found bool
		err   error
	}, 1)
	go func() {
		found, err := registry.StopAndWait(context.Background(), "loop_run_release", "", "", "test stop")
		stopped <- struct {
			found bool
			err   error
		}{found: found, err: err}
	}()

	select {
	case <-runCancelled:
	case result := <-stopped:
		t.Fatalf("StopAndWait() returned before cancelling the released live run: found=%v err=%v", result.found, result.err)
	case <-time.After(time.Second):
		t.Fatal("StopAndWait() did not retain authority over the released live run")
	}
	close(runDone)
	result := <-stopped
	if result.err != nil || !result.found {
		t.Fatalf("StopAndWait() = (%v, %v), want (true, nil)", result.found, result.err)
	}
}

func TestFailedReapRemainsOwnedUntilForcedResolution(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newFailedReapRegistryExecution()
	registry.Register("loop_failed_reap", "run_failed_reap", "exec_failed_reap", execution)
	<-execution.waited

	stopped := make(chan struct {
		found bool
		err   error
	}, 1)
	go func() {
		found, err := registry.StopAndWait(context.Background(), "loop_failed_reap", "", "", "test stop")
		stopped <- struct {
			found bool
			err   error
		}{found: found, err: err}
	}()
	select {
	case result := <-stopped:
		if !result.found || !errors.Is(result.err, errRegistryReapFailed) {
			t.Fatalf("StopAndWait() = (%v, %v), want (true, failed-reap error)", result.found, result.err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("StopAndWait() looped after the retained execution reported a failed reap")
	}
	if err := registry.ForceKillAndWait(context.Background()); err != nil {
		t.Fatalf("ForceKillAndWait() error = %v", err)
	}
	select {
	case <-execution.forceKilled:
	default:
		t.Fatal("failed reap was removed before forced process-group resolution")
	}
	if err := registry.ForceKillAndWait(context.Background()); err != nil {
		t.Fatalf("second ForceKillAndWait() error = %v", err)
	}
}

func TestRuntimeStopLogsGracefulReapFailureAfterSuccessfulForceResolution(t *testing.T) {
	t.Parallel()

	logger := &testLogger{}
	rt := New(Options{Logger: logger, ShutdownTimeout: time.Second})
	execution := newFailedReapRegistryExecution()
	rt.activeExecutions.Register("loop_reap_log", "run_reap_log", "exec_reap_log", execution)
	<-execution.waited

	rt.Stop("test shutdown")

	select {
	case <-execution.forceKilled:
	default:
		t.Fatal("Runtime.Stop() did not force-resolve failed reap ownership")
	}
	if !logger.containsMessage("looperd runtime required forced active-execution reap") {
		t.Fatal("Runtime.Stop() did not log the graceful reap failure")
	}
}

type contextObservingRegistryStarter struct {
	execution *contextObservingRegistryExecution
}

var errRegistryReapFailed = errors.New("process group remains signalable")

type failedReapRegistryExecution struct {
	waited      chan struct{}
	forceKilled chan struct{}
	waitOnce    sync.Once
	forceOnce   sync.Once
}

func newFailedReapRegistryExecution() *failedReapRegistryExecution {
	return &failedReapRegistryExecution{
		waited:      make(chan struct{}),
		forceKilled: make(chan struct{}),
	}
}

func (e *failedReapRegistryExecution) Wait(context.Context) (agent.Result, error) {
	e.waitOnce.Do(func() { close(e.waited) })
	return agent.Result{Status: "killed"}, errRegistryReapFailed
}

func (e *failedReapRegistryExecution) Kill(string) error { return nil }

// A nil ForceKill result is the registry contract that the live handle has
// resolved process-group ownership, not merely that it sent a signal.
func (e *failedReapRegistryExecution) ForceKill() error {
	e.forceOnce.Do(func() { close(e.forceKilled) })
	return nil
}

func (s *contextObservingRegistryStarter) Start(ctx context.Context, _ agent.RunInput) (agent.Execution, error) {
	s.execution = &contextObservingRegistryExecution{ctx: ctx, done: make(chan struct{})}
	return s.execution, nil
}

type contextObservingRegistryExecution struct {
	ctx      context.Context
	done     chan struct{}
	doneOnce sync.Once
}

func (e *contextObservingRegistryExecution) Wait(context.Context) (agent.Result, error) {
	<-e.ctx.Done()
	e.doneOnce.Do(func() { close(e.done) })
	return agent.Result{Status: "killed"}, nil
}

func (e *contextObservingRegistryExecution) Kill(string) error { return nil }
