package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRuntimeStopWaitsForAdmittedHTTPMutation(t *testing.T) {
	admission := NewAdmission()
	if err := admission.MarkReady("test"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	rt := &Runtime{
		admission:       admission,
		shutdownCh:      make(chan struct{}),
		shutdownTimeout: 500 * time.Millisecond,
	}
	release, err := rt.BeginAdmittedMutationIfAllowed()
	if err != nil {
		t.Fatalf("BeginAdmittedMutationIfAllowed() error = %v", err)
	}

	stopped := make(chan struct{})
	go func() {
		rt.Stop("test admitted HTTP mutation")
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Runtime.Stop returned while admitted HTTP mutation was still running")
	case <-time.After(50 * time.Millisecond):
	}
	release()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Runtime.Stop did not finish after admitted HTTP mutation released")
	}
	if err := rt.ShutdownDrainError(); err != nil {
		t.Fatalf("ShutdownDrainError() = %v, want nil after the lease released", err)
	}
}

func TestRuntimeStopRetainsStorageWhenAdmittedHTTPMutationTimesOut(t *testing.T) {
	admission := NewAdmission()
	if err := admission.MarkReady("test"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	rt := &Runtime{
		admission:       admission,
		shutdownCh:      make(chan struct{}),
		shutdownTimeout: 20 * time.Millisecond,
	}
	release, err := rt.BeginAdmittedMutationIfAllowed()
	if err != nil {
		t.Fatalf("BeginAdmittedMutationIfAllowed() error = %v", err)
	}
	rt.Stop("test admitted HTTP timeout")
	release()

	if !rt.StorageRetained() {
		t.Fatal("StorageRetained() = false, want true after admitted HTTP timeout")
	}
	if err := rt.ShutdownDrainError(); err == nil || !strings.Contains(err.Error(), "admitted HTTP mutations") {
		t.Fatalf("ShutdownDrainError() = %v, want admitted HTTP mutation timeout", err)
	}
}

func TestBeginDrainStopsAndJoinsWebhookForwarderMonitors(t *testing.T) {
	admission := NewAdmission()
	if err := admission.MarkReady("test"); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	webhook := &webhookRuntime{
		stopCh:          make(chan struct{}),
		drainCh:         make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{},
	}
	monitorStopped := make(chan struct{})
	webhook.wg.Add(1)
	go func() {
		defer webhook.wg.Done()
		<-webhook.stopCh
		close(monitorStopped)
	}()

	rt := &Runtime{
		admission:       admission,
		shutdownTimeout: time.Second,
		webhook:         webhook,
	}
	if err := rt.BeginDrain("test webhook monitor drain"); err != nil {
		t.Fatalf("BeginDrain() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := rt.WaitForDrain(ctx, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForDrain() error = %v, snapshot=%+v", err, snapshot)
	}
	if !webhook.isStopped() {
		t.Fatal("webhook monitor runtime was not stopped during drain")
	}
	select {
	case <-monitorStopped:
	default:
		t.Fatal("webhook monitor was not joined before drained=true")
	}
}
