package runtime

import (
	"sync"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/daemonbinary"
)

// daemonBinaryWatcher holds the identity of the executable this daemon started
// from and re-checks it on demand.
//
// The check is cheap (one stat) until the file actually moves, so the scheduler
// can run it every tick. Logging is edge-triggered: each distinct on-disk state
// is reported once, so a swap is loud in the log without repeating forever.
type daemonBinaryWatcher struct {
	logger bootstrap.Logger

	mu           sync.Mutex
	recorded     daemonbinary.Identity
	lastReported string
}

func newDaemonBinaryWatcher(logger bootstrap.Logger) *daemonBinaryWatcher {
	return &daemonBinaryWatcher{logger: logger}
}

// record captures the running executable and logs it, so the build an operator
// is actually running is in the log before anything can replace it.
func (w *daemonBinaryWatcher) record() {
	identity, err := daemonbinary.Self()
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("daemon executable identity unavailable; a binary swap cannot be detected", map[string]any{
				"error": err.Error(),
			})
		}
		return
	}

	w.mu.Lock()
	w.recorded = identity
	w.lastReported = identity.SHA256
	w.mu.Unlock()

	if w.logger != nil {
		w.logger.Info("daemon executable recorded", map[string]any{
			"path":   identity.Path,
			"sha256": identity.SHA256,
			"size":   identity.Size,
		})
	}
}

// Status compares the on-disk executable with the running image and logs the
// first observation of each new state.
func (w *daemonBinaryWatcher) Status() daemonbinary.Status {
	w.mu.Lock()
	recorded := w.recorded
	w.mu.Unlock()

	status := daemonbinary.Verify(recorded)

	w.mu.Lock()
	marker := statusMarker(status)
	report := marker != w.lastReported
	if report {
		w.lastReported = marker
	}
	w.mu.Unlock()

	if report && w.logger != nil && status.Swapped {
		w.logger.Warn("daemon executable changed underneath the running daemon", map[string]any{
			"path":          status.Path,
			"runningSha256": status.RunningSHA256,
			"onDiskSha256":  status.OnDiskSHA256,
			"reason":        status.Reason,
		})
	}

	return status
}

// statusMarker collapses a status into the value that decides whether this
// observation is new. A swap with no readable digest still has to be
// distinguishable from the recorded digest, so the reason stands in.
func statusMarker(status daemonbinary.Status) string {
	if !status.Known {
		return "unknown"
	}
	if status.OnDiskSHA256 != "" {
		return status.OnDiskSHA256
	}
	return status.Reason
}

// DaemonBinaryStatus reports whether the daemon's own executable file still
// holds the image this process is running.
func (r *Runtime) DaemonBinaryStatus() daemonbinary.Status {
	if r == nil || r.daemonBinary == nil {
		return daemonbinary.Verify(daemonbinary.Identity{})
	}

	return r.daemonBinary.Status()
}
