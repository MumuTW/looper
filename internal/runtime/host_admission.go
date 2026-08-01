package runtime

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/hostresources"
	"github.com/MumuTW/looper/internal/storage"
)

// HostAdmissionStatus is the operator-facing view of the last host reading.
type HostAdmissionStatus struct {
	Enabled  bool                    `json:"enabled"`
	Admit    bool                    `json:"admit"`
	Reasons  []string                `json:"reasons,omitempty"`
	Detail   string                  `json:"detail,omitempty"`
	SampleAt *string                 `json:"sampleAt,omitempty"`
	Snapshot *hostresources.Snapshot `json:"snapshot,omitempty"`
}

// hostAdmissionSampleInterval bounds how often the host is sampled. The claim
// path runs on every scheduler tick and statfs plus two sysctls per tick is
// pointless work; host pressure does not meaningfully change inside a second.
const hostAdmissionSampleInterval = 5 * time.Second

// hostAdmissionGate caches one reading and re-evaluates it against the live
// config, so a threshold edit takes effect on the next tick without waiting for
// the sample to expire.
type hostAdmissionGate struct {
	mu        sync.Mutex
	statePath string
	now       func() time.Time
	read      func(string) hostresources.Snapshot

	sampledAt time.Time
	snapshot  hostresources.Snapshot
	hasSample bool

	// Throttle state for hold logging. The claim path runs several times per
	// second and a sustained hold would otherwise emit the same warning on
	// every call. A hold is logged once per decision transition and once per
	// fresh sample, so a sustained low-disk condition surfaces at the sample
	// cadence rather than the tick cadence.
	lastLoggedHoldKey      string
	lastLoggedHoldSampleAt time.Time
}

func newHostAdmissionGate(statePath string, now func() time.Time) *hostAdmissionGate {
	if now == nil {
		now = time.Now
	}
	return &hostAdmissionGate{statePath: statePath, now: now, read: hostresources.Read}
}

// Decide returns nil when the guard is disabled or unconfigured, which the
// scheduler treats as "no opinion" and admits.
func (g *hostAdmissionGate) Decide(cfg config.ResourceGuardConfig) *hostresources.Decision {
	if g == nil || !cfg.Enabled {
		return nil
	}
	snapshot := g.sample()
	decision := hostresources.Evaluate(snapshot, thresholdsFromConfig(cfg))
	return &decision
}

// Status re-evaluates the cached sample for the status surface. It never takes
// a fresh reading: a status request must not be able to drive host sampling.
func (g *hostAdmissionGate) Status(cfg config.ResourceGuardConfig) HostAdmissionStatus {
	status := HostAdmissionStatus{Enabled: cfg.Enabled, Admit: true}
	if g == nil {
		return status
	}
	g.mu.Lock()
	snapshot, sampledAt, hasSample := g.snapshot, g.sampledAt, g.hasSample
	g.mu.Unlock()
	if !hasSample {
		return status
	}
	sampleAt := formatJavaScriptISOString(sampledAt.UTC())
	status.SampleAt = &sampleAt
	status.Snapshot = &snapshot
	if !cfg.Enabled {
		return status
	}
	decision := hostresources.Evaluate(snapshot, thresholdsFromConfig(cfg))
	status.Admit = decision.Admit
	status.Reasons = decision.Reasons
	status.Detail = decision.Summary()
	return status
}

func (g *hostAdmissionGate) sample() hostresources.Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.hasSample && now.Sub(g.sampledAt) < hostAdmissionSampleInterval {
		return g.snapshot
	}
	g.snapshot = g.read(g.statePath)
	g.sampledAt = now
	g.hasSample = true
	return g.snapshot
}

// ShouldLogHold reports whether a hold decision should be logged now, recording
// the decision so a repeated hold on the same sample is logged once. The claim
// pass invokes this on every tick; without it a sustained hold would emit the
// same warning several times per second (and on an empty queue, since admission
// is computed from running-count capacity alone), churning logs and consuming
// the disk the guard protects. A hold logs once per decision transition and
// once per fresh sample; an admitting or absent decision resets the memory so
// the next hold logs immediately.
func (g *hostAdmissionGate) ShouldLogHold(decision *hostresources.Decision) bool {
	if g == nil {
		return true
	}
	if decision == nil || decision.Admit {
		g.mu.Lock()
		g.lastLoggedHoldKey = ""
		g.lastLoggedHoldSampleAt = time.Time{}
		g.mu.Unlock()
		return false
	}
	key := holdLogKey(decision)
	g.mu.Lock()
	defer g.mu.Unlock()
	if key == g.lastLoggedHoldKey && g.sampledAt.Equal(g.lastLoggedHoldSampleAt) {
		return false
	}
	g.lastLoggedHoldKey = key
	g.lastLoggedHoldSampleAt = g.sampledAt
	return true
}

// holdLogKey is the stable signature of a hold decision: the tripped reasons
// and their operator-facing detail. A change in either is a transition worth
// logging; identical key on the same sample is the repeated warning to suppress.
func holdLogKey(decision *hostresources.Decision) string {
	return strings.Join(decision.Reasons, ",") + "|" + decision.Summary()
}

func thresholdsFromConfig(cfg config.ResourceGuardConfig) hostresources.Thresholds {
	thresholds := hostresources.Thresholds{
		MinDiskFreePercent: cfg.MinDiskFreePercent,
		MaxLoadPerCPU:      cfg.MaxLoadPerCPU,
	}
	if cfg.MinDiskFreeGB > 0 {
		thresholds.MinDiskFreeBytes = uint64(cfg.MinDiskFreeGB * float64(1<<30))
	}
	return thresholds
}

// hostAdmissionStatePath is the directory whose filesystem the disk signal
// measures: the one holding the SQLite database, because that is the write that
// turns a full disk into a corrupt daemon.
//
// The configured dbPath may be a SQLite file: URI (e.g.
// "file:/var/lib/looper/looper.sqlite?cache=shared"), which filepath.Dir cannot
// interpret — it would yield "file:/var/lib/looper", a path statfs cannot
// resolve, so Read records a disk error and Evaluate admits on the absent disk
// fields, silently disabling the guard for that configuration. Normalize through
// storage.SQLiteFilesystemPath first so the URI form resolves to the real
// filesystem directory. A memory database (":memory:" or "file:...?mode=memory")
// has no filesystem to measure; SQLiteFilesystemPath reports that as !isFile, and
// the path falls through to the default home so the disk signal still reads a
// real volume rather than a URI prefix.
//
// The filesystem path is resolved through filepath.EvalSymlinks before its
// parent is selected. SQLite follows a symlinked database file to its target and
// writes there, so taking filepath.Dir of the link would measure the filesystem
// that holds the link while the database lives on the target's filesystem — the
// guard could admit work based on ample space beside the symlink even when the
// real database volume is full. A database that does not yet exist falls back to
// the unresolved directory; statfs follows any symlink in that directory itself,
// so only the file-level symlink needs resolving here.
func hostAdmissionStatePath(cfg config.Config) string {
	if path := strings.TrimSpace(cfg.Storage.DBPath); path != "" {
		if fsPath, isFile, err := storage.SQLiteFilesystemPath(path); err == nil && isFile {
			return databaseDirForAdmission(fsPath)
		}
	}
	if home, err := config.DefaultLooperHome(); err == nil {
		return home
	}
	return "."
}

// databaseDirForAdmission resolves symlinks in the database path before
// selecting its parent directory, so the disk signal measures the filesystem
// SQLite writes to rather than the one that merely holds the link. See
// hostAdmissionStatePath for why the file-level symlink must be resolved before
// filepath.Dir is taken.
func databaseDirForAdmission(dbPath string) string {
	if resolved, err := filepath.EvalSymlinks(dbPath); err == nil {
		return filepath.Dir(resolved)
	}
	// A database that has not been created yet cannot be resolved at the file
	// level. Fall back to its directory as-is: statfs follows directory symlinks
	// itself, so a symlinked parent directory still measures the target
	// filesystem. Any other resolution error is treated as an absent signal,
	// which Evaluate admits on, matching the guard's fail-open posture.
	return filepath.Dir(dbPath)
}

// HostAdmissionStatus publishes the last host reading for the status surface.
func (r *Runtime) HostAdmissionStatus() HostAdmissionStatus {
	cfg := r.Config().Daemon.ResourceGuard
	r.mu.RLock()
	gate := r.hostAdmission
	r.mu.RUnlock()
	return gate.Status(cfg)
}
