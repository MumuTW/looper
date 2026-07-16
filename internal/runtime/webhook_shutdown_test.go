package runtime

import (
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestWebhookStopDoesNotWaitPastShutdownDeadline(t *testing.T) {
	cfg := config.Config{Daemon: config.DaemonConfig{ShutdownTimeoutMS: 25}}
	runtime := newWebhookRuntime(cfg, nil, time.Now)
	runtime.wg.Add(1) // Model a stuck webhook goroutine.
	t.Cleanup(runtime.wg.Done)

	startedAt := time.Now()
	runtime.Stop()
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop() elapsed = %s, want bounded webhook shutdown", elapsed)
	}
}

func TestWebhookStopClosesTaskSpawnGate(t *testing.T) {
	runtime := newWebhookRuntime(config.Config{}, nil, time.Now)
	runtime.Stop()

	if runtime.launchForwarder("nexu-io/looper") {
		t.Fatal("launchForwarder accepted a task after Stop")
	}
	if runtime.adoptForwarder(storage.WebhookForwarderRecord{Repo: "nexu-io/looper", PID: 42}, []string{"gh"}) {
		t.Fatal("adoptForwarder accepted a task after Stop")
	}
	if runtime.scheduleReconcileRetry(&storage.Repositories{}) {
		t.Fatal("scheduleReconcileRetry accepted a task after Stop")
	}
	if got := len(runtime.Status().Forwarders); got != 0 {
		t.Fatalf("forwarders after rejected adoption = %d, want 0", got)
	}
}
