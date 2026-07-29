package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitForReadyHonorsCallerDeadlineDuringProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	proc := &DaemonProcess{
		baseURL: server.URL,
		doneCh:  make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := proc.WaitForReady(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForReady error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("WaitForReady exceeded caller budget by too much: %s", elapsed)
	}
}
