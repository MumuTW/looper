package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/e2e/harness"
)

// TestSmokeLooperdExitsWhenSupervisionPipeCloses proves the orphan protection:
// the harness holds the write end of the daemon's stdin, so when the test
// binary dies without running cleanups (SIGKILL, go-test timeout panic), the
// kernel closes the pipe and the daemon must shut itself down.
func TestSmokeLooperdExitsWhenSupervisionPipeCloses(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}

	proc.CloseSupervision()

	select {
	case <-proc.Exited():
	case <-time.After(15 * time.Second):
		t.Fatal("looperd still running 15s after its supervision pipe closed; orphan protection is broken")
	}
}

// TestSmokeLooperdExitsWhenSupervisionPipeClosesDuringStartup closes the pipe
// immediately after spawn, before waiting for readiness. Bootstrap's shutdown
// listener registers only after the runtime is up, so this exercises the
// window where the shutdown request must be buffered by the early signal
// registration instead of hitting the default SIGTERM action or being lost.
func TestSmokeLooperdExitsWhenSupervisionPipeClosesDuringStartup(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	port := harness.MustFreePort(t)
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{Port: port})
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, nil, cfg.Server.Host, cfg.Server.Port)

	proc.CloseSupervision()

	select {
	case <-proc.Exited():
	case <-time.After(45 * time.Second):
		t.Fatal("looperd still running 45s after its supervision pipe closed during startup")
	}
}
