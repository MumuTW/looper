package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/daemonbinary"
)

func recordedWatcher(t *testing.T, logger *testLogger, content string) (*daemonBinaryWatcher, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "looperd")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	identity, err := daemonbinary.Capture(path)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	watcher := newDaemonBinaryWatcher(logger)
	watcher.recorded = identity
	watcher.lastReported = identity.SHA256
	return watcher, path
}

func replaceExecutable(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("rewrite %s: %v", path, err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
}

func TestDaemonBinaryWatcherWarnsOncePerSwap(t *testing.T) {
	logger := &testLogger{}
	watcher, path := recordedWatcher(t, logger, "running image")

	if status := watcher.Status(); status.Swapped {
		t.Fatalf("Status() = %#v, want no swap before the file changes", status)
	}

	replaceExecutable(t, path, "agent-built replacement")

	status := watcher.Status()
	if !status.Swapped {
		t.Fatalf("Status() = %#v, want Swapped after the executable is replaced", status)
	}

	// Repeat checks run every scheduler tick; the warning is edge-triggered so
	// the log records the swap without burying everything after it.
	watcher.Status()
	watcher.Status()

	warnings := 0
	for _, message := range logger.messages() {
		if strings.Contains(message, "daemon executable changed") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("swap warnings = %d, want exactly 1 (messages: %v)", warnings, logger.messages())
	}

	// A second, different replacement is a new fact and warns again.
	replaceExecutable(t, path, "a third build entirely")
	watcher.Status()

	warnings = 0
	for _, message := range logger.messages() {
		if strings.Contains(message, "daemon executable changed") {
			warnings++
		}
	}
	if warnings != 2 {
		t.Fatalf("swap warnings after a second replacement = %d, want 2", warnings)
	}
}

// A Runtime that never recorded an identity must report "unknown" rather than
// the reassuring "unchanged".
func TestDaemonBinaryStatusWithoutRecordIsUnknown(t *testing.T) {
	watcher := newDaemonBinaryWatcher(&testLogger{})

	status := watcher.Status()
	if status.Known || status.Swapped {
		t.Fatalf("Status() = %#v, want an unknown, unswapped status", status)
	}
	if status.Reason == "" {
		t.Fatal("Reason is empty, want an explanation that detection is unavailable")
	}
}

// The daemon records its own executable at Start, so a live daemon can always
// answer the question.
func TestRuntimeStartRecordsDaemonBinary(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	backupDir := t.TempDir()
	cfg.Storage.DBPath = filepath.Join(workingDir, "runtime.sqlite")
	cfg.Storage.BackupDir = &backupDir

	rt := New(Options{
		Config:           cfg,
		Logger:           &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
	})
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		rt.Stop("test cleanup")
		rt.WaitForShutdown()
	})

	status := rt.DaemonBinaryStatus()
	if !status.Known {
		t.Fatalf("DaemonBinaryStatus() = %#v, want a known identity after Start", status)
	}
	if status.Path == "" || status.RunningSHA256 == "" {
		t.Fatalf("DaemonBinaryStatus() = %#v, want the executable path and running digest", status)
	}
}
