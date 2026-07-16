package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	looperdruntime "github.com/nexu-io/looper/internal/runtime"
)

func TestHandlerRejectsMutationsUntilStartupRecoveryCompletes(t *testing.T) {
	startupReady := make(chan struct{})
	reconciliations := 0
	handler := NewHandler(Context{
		StartupReady: startupReady,
		ReconcileStaleRuns: func(context.Context) (looperdruntime.StaleRunReconcileSummary, error) {
			reconciliations++
			return looperdruntime.StaleRunReconcileSummary{}, nil
		},
	})

	mutation := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/runs/reconcile-stale", nil))
		return recorder
	}

	beforeReady := mutation()
	if beforeReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("mutation before startup ready status = %d, want %d; body=%s", beforeReady.Code, http.StatusServiceUnavailable, beforeReady.Body.String())
	}
	if reconciliations != 0 {
		t.Fatalf("reconciliations before startup ready = %d, want 0", reconciliations)
	}

	readOnly := httptest.NewRecorder()
	handler.ServeHTTP(readOnly, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	if readOnly.Code != http.StatusOK {
		t.Fatalf("read-only request before startup ready status = %d, want %d; body=%s", readOnly.Code, http.StatusOK, readOnly.Body.String())
	}

	close(startupReady)
	afterReady := mutation()
	if afterReady.Code != http.StatusOK {
		t.Fatalf("mutation after startup ready status = %d, want %d; body=%s", afterReady.Code, http.StatusOK, afterReady.Body.String())
	}
	if reconciliations != 1 {
		t.Fatalf("reconciliations after startup ready = %d, want 1", reconciliations)
	}
}
