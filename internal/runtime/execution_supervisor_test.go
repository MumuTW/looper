package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExecutionSupervisorShutdownOwnsAdmittedReservation(t *testing.T) {
	supervisor := NewActiveExecutionRegistry()
	reservation, err := supervisor.Reserve("loop-1")
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	supervisor.BeginShutdown("daemon shutting down")

	if _, err := supervisor.Reserve("loop-2"); !errors.Is(err, ErrExecutionAdmissionClosed) {
		t.Fatalf("Reserve() error = %v, want ErrExecutionAdmissionClosed", err)
	}
	select {
	case <-reservation.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("reservation context was not cancelled by shutdown")
	}

	reservation.Release()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := supervisor.Wait(waitCtx); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
}

func TestExecutionSupervisorLoopStopCancelsAndRejectsOnlyTargetLoop(t *testing.T) {
	supervisor := NewActiveExecutionRegistry()
	target, err := supervisor.Reserve("loop-target")
	if err != nil {
		t.Fatalf("Reserve(target) error = %v", err)
	}

	releaseStop := supervisor.BeginLoopStop("loop-target", "operator stopped loop")
	defer releaseStop()

	if _, err := supervisor.Reserve("loop-target"); !errors.Is(err, ErrExecutionLoopStopping) {
		t.Fatalf("Reserve(target while stopping) error = %v, want ErrExecutionLoopStopping", err)
	}
	other, err := supervisor.Reserve("loop-other")
	if err != nil {
		t.Fatalf("Reserve(other) error = %v", err)
	}
	other.Release()
	select {
	case <-target.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("target reservation context was not cancelled by loop stop")
	}
	target.Release()

	releaseStop()
	resumed, err := supervisor.Reserve("loop-target")
	if err != nil {
		t.Fatalf("Reserve(target after stop) error = %v", err)
	}
	resumed.Release()
}

func TestExecutionSupervisorBindsUnknownClaimIntoConcurrentLoopStop(t *testing.T) {
	supervisor := NewActiveExecutionRegistry()
	reservation, err := supervisor.Reserve("")
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	defer reservation.Release()

	releaseStop := supervisor.BeginLoopStop("loop-target", "operator stopped loop")
	defer releaseStop()
	reservation.BindLoop("loop-target")

	select {
	case <-reservation.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("reservation bound during loop stop was not cancelled")
	}
}

func TestExecutionSupervisorRejectsLateExecutionRegistration(t *testing.T) {
	supervisor := NewActiveExecutionRegistry()
	supervisor.BeginShutdown("daemon shutting down")
	execution := &recordingActiveExecution{killReasons: make(chan string, 1)}

	supervisor.Register("loop-1", "run-1", "execution-1", execution)

	select {
	case reason := <-execution.killReasons:
		if reason != "daemon shutting down" {
			t.Fatalf("kill reason = %q, want daemon shutting down", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("late execution registration was not stopped")
	}
}

func TestExecutionSupervisorInfrastructureFailureClosesAdmission(t *testing.T) {
	supervisor := NewExecutionSupervisor()
	reservation, err := supervisor.Reserve("loop-1")
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	supervisor.MarkDegraded(errors.New("terminal persistence unavailable"))

	if _, err := supervisor.Reserve("loop-2"); !errors.Is(err, ErrExecutionSupervisorDegraded) {
		t.Fatalf("Reserve() error = %v, want ErrExecutionSupervisorDegraded", err)
	}
	select {
	case <-reservation.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("degraded Supervisor did not cancel admitted work")
	}
	reservation.Release()
}

type recordingActiveExecution struct{ killReasons chan string }

func (e *recordingActiveExecution) Kill(reason string) error {
	e.killReasons <- reason
	return nil
}
