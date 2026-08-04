package runtime

import (
	"context"
	"testing"
	"time"
)

func TestMarkDegradedCancelsAdmittedBackfillBackgroundOperation(t *testing.T) {
	registry := NewActiveExecutionRegistry()
	operation, err := registry.AdmitBackground(context.Background(), BackgroundOperationMeta{Name: "coordinator-backfill"})
	if err != nil {
		t.Fatalf("AdmitBackground() error = %v", err)
	}
	defer operation.Release()

	runtime := &Runtime{admission: NewAdmission(), activeExecutions: registry}
	if err := runtime.MarkDegraded("persistence failure"); err != nil {
		t.Fatalf("MarkDegraded() error = %v", err)
	}
	select {
	case <-operation.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("MarkDegraded did not cancel an admitted backfill operation")
	}
	if got := runtime.AdmissionState(); got != AdmissionDegraded {
		t.Fatalf("AdmissionState() = %q, want degraded", got)
	}
}
