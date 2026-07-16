package runtime

import (
	"os"
	"os/exec"
	"syscall"
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
	// Stop budgets 2x shutdownTimeout (TERM grace + kill/reap), still bounded.
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop() elapsed = %s, want bounded webhook shutdown", elapsed)
	}
}

func TestWebhookStopWaitsForForwarderReapAfterKillEscalation(t *testing.T) {
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	startedCh := make(chan struct{})
	originalCommand := execCommand
	originalStartedHook := webhookForwarderStartedHook
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(testBin, "-test.run=TestWebhookRuntimeForwarderHelperProcess", "--")
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "LOOPER_HELPER_IGNORE_TERM=1")
		cmd.Args[0] = name
		return cmd
	}
	webhookForwarderStartedHook = func() {
		close(startedCh)
	}
	t.Cleanup(func() {
		execCommand = originalCommand
		webhookForwarderStartedHook = originalStartedHook
	})

	const shutdownMS = 40
	rt := &webhookRuntime{
		cfg: config.Config{Daemon: config.DaemonConfig{ShutdownTimeoutMS: shutdownMS}},
		status: WebhookStatus{
			Enabled:    true,
			Forwarders: []WebhookForwarderState{{Repo: "nexu-io/looper", Command: []string{"gh", "webhook", "forward"}}},
		},
		stopCh:          make(chan struct{}),
		forwarderStopCh: map[string]chan struct{}{"nexu-io/looper": make(chan struct{})},
		now:             time.Now,
	}
	rt.launchForwarder("nexu-io/looper")
	<-startedCh

	// Wait until the forwarder publishes a live PID so we can assert reaping.
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := rt.Status()
		if status.Forwarders[0].Running && status.Forwarders[0].PID != nil {
			pid = *status.Forwarders[0].PID
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("forwarder did not publish a running PID")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	startedAt := time.Now()
	rt.Stop()
	elapsed := time.Since(startedAt)
	// Must outlast the TERM grace window so Kill escalation can complete, but
	// stay within the 2x shutdown budget used by Stop.
	if elapsed < time.Duration(shutdownMS)*time.Millisecond {
		t.Fatalf("Stop() returned after %s, want at least one TERM grace interval for escalation", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Stop() elapsed = %s, want bounded 2x shutdown wait", elapsed)
	}

	if live, probeErr := (defaultProcessProbe{}).IsAlive(pid); probeErr != nil {
		t.Fatalf("probe forwarder pid %d: %v", pid, probeErr)
	} else if live {
		t.Fatalf("forwarder pid %d still alive after Stop(); want reaped after Kill escalation", pid)
	}
	status := rt.Status()
	if status.Forwarders[0].Running || status.Forwarders[0].PID != nil {
		t.Fatalf("forwarder state after Stop = %#v, want not running and nil PID", status.Forwarders[0])
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
