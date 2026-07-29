package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIClientGetContextHonorsPollingDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := newAPIClient(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := client.getContext(ctx, "/api/v1/runs", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("getContext error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("getContext exceeded polling budget by too much: %s", elapsed)
	}
}
