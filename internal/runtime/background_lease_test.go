package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackgroundOperationIsOwnedByDrainAndCanceledOnShutdown(t *testing.T) {
	registry := NewActiveExecutionRegistry()
	operation, err := registry.AdmitBackground(context.Background(), BackgroundOperationMeta{Name: "backfill"})
	if err != nil {
		t.Fatalf("AdmitBackground() error = %v", err)
	}
	if snapshot := registry.DrainSnapshot(); snapshot.BackgroundOperations != 1 || snapshot.Drained() {
		t.Fatalf("snapshot = %#v, want one live background operation", snapshot)
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- registry.BeginShutdown("test shutdown") }()
	select {
	case <-operation.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the background operation")
	}
	operation.Release()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("BeginShutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("BeginShutdown did not observe the released operation")
	}
	if snapshot := registry.DrainSnapshot(); !snapshot.Drained() {
		t.Fatalf("final snapshot = %#v, want drained", snapshot)
	}
}

func TestBackgroundOperationAdmissionClosesWithDrain(t *testing.T) {
	registry := NewActiveExecutionRegistry()
	operation, err := registry.AdmitBackground(context.Background(), BackgroundOperationMeta{Name: "backfill"})
	if err != nil {
		t.Fatalf("AdmitBackground() error = %v", err)
	}
	operation.Release()
	if err := registry.BeginShutdown("test shutdown"); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	if _, err := registry.AdmitBackground(context.Background(), BackgroundOperationMeta{Name: "late"}); !errors.Is(err, ErrOperationAdmissionClosed) {
		t.Fatalf("late AdmitBackground() error = %v, want admission closed", err)
	}
}
