package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestServerReportsUnexpectedPostStartServeFailure(t *testing.T) {
	server := NewServer(config.Config{Server: config.ServerConfig{Host: "127.0.0.1", Port: freeTCPPort(t)}}, http.NotFoundHandler())
	if err := server.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	server.mu.Lock()
	listener := server.listener
	server.mu.Unlock()
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}

	select {
	case err := <-server.Errors():
		if err == nil || !strings.Contains(err.Error(), "serve API") {
			t.Fatalf("Errors() = %v, want post-start serve failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Errors() did not report post-start serve failure")
	}
	_ = server.Stop(context.Background())
}

func TestServerStopBoundsDoneWaitByContext(t *testing.T) {
	server := &Server{server: &http.Server{}, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = server.Stop(ctx)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop() elapsed = %s, want context-bounded done wait", elapsed)
	}
}
