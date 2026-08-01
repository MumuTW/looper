package runtime

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/hostresources"
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
func hostAdmissionStatePath(cfg config.Config) string {
	if path := strings.TrimSpace(cfg.Storage.DBPath); path != "" {
		return filepath.Dir(path)
	}
	if home, err := config.DefaultLooperHome(); err == nil {
		return home
	}
	return "."
}

// HostAdmissionStatus publishes the last host reading for the status surface.
func (r *Runtime) HostAdmissionStatus() HostAdmissionStatus {
	cfg := r.Config().Daemon.ResourceGuard
	r.mu.RLock()
	gate := r.hostAdmission
	r.mu.RUnlock()
	return gate.Status(cfg)
}
