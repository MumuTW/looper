package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
)

type recordedLogEntry struct {
	level   string
	message string
	context map[string]any
}

type recordingLogger struct {
	mu      sync.Mutex
	entries []recordedLogEntry
}

func (l *recordingLogger) Debug(string, map[string]any) {}
func (l *recordingLogger) Info(string, map[string]any)  {}
func (l *recordingLogger) Warn(string, map[string]any)  {}

func (l *recordingLogger) Error(message string, context map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, recordedLogEntry{level: "error", message: message, context: context})
}

func (l *recordingLogger) errorEntries() []recordedLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedLogEntry(nil), l.entries...)
}

func testServerConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = freeTCPPort(t)
	return cfg
}

func TestServerLogsUnexpectedServeError(t *testing.T) {
	logger := &recordingLogger{}
	server := NewServer(testServerConfig(t), http.NewServeMux(), logger)
	if err := server.Start(); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}

	// Closing the listener out from under Serve simulates an unexpected
	// accept-loop failure while the daemon keeps running.
	server.mu.Lock()
	listener := server.listener
	done := server.done
	server.mu.Unlock()
	if listener == nil || done == nil {
		t.Fatal("server did not record its listener after Start")
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("listener.Close() error = %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve goroutine did not exit after listener failure")
	}

	entries := logger.errorEntries()
	if len(entries) != 1 {
		t.Fatalf("logged error entries = %d, want 1: %+v", len(entries), entries)
	}
	entry := entries[0]
	if !strings.Contains(entry.message, "api server stopped unexpectedly") {
		t.Fatalf("log message = %q, want it to mention the unexpected stop", entry.message)
	}
	if entry.context["error"] == nil || entry.context["error"] == "" {
		t.Fatalf("log context missing error detail: %+v", entry.context)
	}
}

func TestServerStopDoesNotLogServeError(t *testing.T) {
	logger := &recordingLogger{}
	server := NewServer(testServerConfig(t), http.NewServeMux(), logger)
	if err := server.Start(); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatalf("Server.Stop() error = %v", err)
	}

	if entries := logger.errorEntries(); len(entries) != 0 {
		t.Fatalf("logged error entries after graceful stop = %+v, want none", entries)
	}
}
