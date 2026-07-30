package runtime

import (
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
)

// daemon.worktreeCleanup.enabled is hot-editable, so the loop runs regardless of
// the startup value and gates each pass on the current one. Before this, a
// daemon that started with cleanup disabled had no loop at all, and a daemon
// that started with it enabled captured the interval for the process lifetime.
func TestWorktreeCleanupLoopGatesEachPassOnLiveEnabledFlag(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	rt := fixture.runtime
	rt.services = Services{Repositories: fixture.repos}
	setWorktreeCleanupEnabled(rt, false)

	rt.startWorktreeCleanupLoop()
	t.Cleanup(rt.stopWorktreeCleanupLoop)

	// The fixture's negative initial delay fires the first wake immediately, so
	// a pass would already have landed here had the loop ignored the flag.
	time.Sleep(250 * time.Millisecond)
	if status := rt.WorktreeCleanupStatus(); status.LastStatus != "idle" || status.Enabled {
		t.Fatalf("status while disabled = %#v, want idle and disabled", status)
	}

	setWorktreeCleanupEnabled(rt, true)
	rt.TriggerWorktreeCleanup()

	waitForCondition(t, 5*time.Second, func() bool {
		return rt.WorktreeCleanupStatus().LastStatus == "completed"
	})
	if status := rt.WorktreeCleanupStatus(); !status.Enabled {
		t.Fatalf("status after enabling = %#v, want enabled", status)
	}
}

// TriggerWorktreeCleanup is called from the config reload path, which can run
// before the loop is started and after it is stopped.
func TestTriggerWorktreeCleanupIsSafeWithoutRunningLoop(t *testing.T) {
	t.Parallel()

	fixture := newWorktreeCleanupFixture(t)
	fixture.runtime.TriggerWorktreeCleanup()

	fixture.runtime.startWorktreeCleanupLoop()
	fixture.runtime.stopWorktreeCleanupLoop()
	fixture.runtime.TriggerWorktreeCleanup()
}

func TestWorktreeCleanupIntervalFallsBackOnUnusableValue(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Daemon.WorktreeCleanup.Interval = "30m"
	if got := worktreeCleanupInterval(cfg); got != 30*time.Minute {
		t.Fatalf("worktreeCleanupInterval(30m) = %v, want 30m", got)
	}
	for _, value := range []string{"", "nonsense", "0s", "-5m"} {
		cfg.Daemon.WorktreeCleanup.Interval = value
		if got := worktreeCleanupInterval(cfg); got != time.Hour {
			t.Fatalf("worktreeCleanupInterval(%q) = %v, want 1h", value, got)
		}
	}
}

// setWorktreeCleanupEnabled publishes through the project catalog, the same
// boundary a config reload uses (applyLoadedConfigBoundaryLocked).
func setWorktreeCleanupEnabled(r *Runtime, enabled bool) {
	cfg := r.Config()
	cfg.Daemon.WorktreeCleanup.Enabled = enabled
	r.projectCatalog.PublishGlobals(cfg)
}
