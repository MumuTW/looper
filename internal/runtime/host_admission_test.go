package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/hostresources"
)

func freeBytes(value uint64) *uint64 { return &value }
func loadPtr(value float64) *float64 { return &value }

func guardConfig() config.ResourceGuardConfig {
	return config.ResourceGuardConfig{Enabled: true, MinDiskFreePercent: 5, MinDiskFreeGB: 10, MaxLoadPerCPU: 2}
}

func gateReading(t *testing.T, snapshot hostresources.Snapshot, now func() time.Time) (*hostAdmissionGate, *int) {
	t.Helper()
	reads := 0
	gate := newHostAdmissionGate("/state", now)
	gate.read = func(string) hostresources.Snapshot {
		reads++
		return snapshot
	}
	return gate, &reads
}

func TestHostAdmissionGateRefusesOnLowDisk(t *testing.T) {
	t.Parallel()

	const gib = uint64(1) << 30
	gate, _ := gateReading(t, hostresources.Snapshot{
		DiskFreeBytes:  freeBytes(2 * gib),
		DiskTotalBytes: freeBytes(500 * gib),
		NumCPU:         10,
	}, func() time.Time { return time.Unix(0, 0) })

	decision := gate.Decide(guardConfig())
	if decision == nil || decision.Admit {
		t.Fatalf("Decide() = %#v, want a refusal", decision)
	}
	if decision.Summary() == "" {
		t.Fatal("a refusal must carry operator-facing detail")
	}
}

// A disabled guard must return no opinion at all, not an admitting decision:
// the scheduler treats nil as ungated and must not pay for a host read.
func TestHostAdmissionGateIsInertWhenDisabled(t *testing.T) {
	t.Parallel()

	gate, reads := gateReading(t, hostresources.Snapshot{}, func() time.Time { return time.Unix(0, 0) })
	cfg := guardConfig()
	cfg.Enabled = false

	if decision := gate.Decide(cfg); decision != nil {
		t.Fatalf("Decide() = %#v, want nil", decision)
	}
	if *reads != 0 {
		t.Fatalf("host reads = %d, want 0 while disabled", *reads)
	}
}

// The claim path runs on every tick; the host must not be re-read each time.
func TestHostAdmissionGateCachesWithinTheSampleInterval(t *testing.T) {
	t.Parallel()

	current := time.Unix(0, 0)
	gate, reads := gateReading(t, hostresources.Snapshot{NumCPU: 10}, func() time.Time { return current })

	gate.Decide(guardConfig())
	gate.Decide(guardConfig())
	if *reads != 1 {
		t.Fatalf("host reads = %d, want 1 inside the sample interval", *reads)
	}

	current = current.Add(hostAdmissionSampleInterval + time.Second)
	gate.Decide(guardConfig())
	if *reads != 2 {
		t.Fatalf("host reads = %d, want 2 after the sample expired", *reads)
	}
}

// Thresholds are re-applied to the cached sample, so an operator raising a
// threshold to unblock a stuck daemon takes effect on the next tick rather than
// after the sample expires.
func TestHostAdmissionGateReappliesThresholdsToACachedSample(t *testing.T) {
	t.Parallel()

	const gib = uint64(1) << 30
	current := time.Unix(0, 0)
	gate, reads := gateReading(t, hostresources.Snapshot{
		DiskFreeBytes:  freeBytes(6 * gib),
		DiskTotalBytes: freeBytes(500 * gib),
		NumCPU:         10,
	}, func() time.Time { return current })

	if decision := gate.Decide(guardConfig()); decision == nil || decision.Admit {
		t.Fatalf("Decide() = %#v, want a refusal at the default floor", decision)
	}

	relaxed := guardConfig()
	relaxed.MinDiskFreeGB = 1
	relaxed.MinDiskFreePercent = 1
	if decision := gate.Decide(relaxed); decision == nil || !decision.Admit {
		t.Fatalf("Decide() = %#v, want admission after the floor was lowered", decision)
	}
	if *reads != 1 {
		t.Fatalf("host reads = %d, want the cached sample reused", *reads)
	}
}

// Status must never drive sampling: a status poll is not a reason to stat the
// filesystem, and a caller could otherwise poll the host arbitrarily fast.
func TestHostAdmissionStatusDoesNotSample(t *testing.T) {
	t.Parallel()

	gate, reads := gateReading(t, hostresources.Snapshot{NumCPU: 10}, func() time.Time { return time.Unix(0, 0) })

	status := gate.Status(guardConfig())
	if *reads != 0 {
		t.Fatalf("host reads = %d, want 0 before any decision", *reads)
	}
	if !status.Admit || status.SampleAt != nil {
		t.Fatalf("Status() = %#v, want an admitting status with no sample", status)
	}

	gate.Decide(guardConfig())
	if status = gate.Status(guardConfig()); status.SampleAt == nil || status.Snapshot == nil {
		t.Fatalf("Status() = %#v, want the sample published after a decision", status)
	}
	if *reads != 1 {
		t.Fatalf("host reads = %d, want the decision's sample reused", *reads)
	}
}

func TestHostAdmissionStatePathFollowsTheDatabase(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Storage.DBPath = "/var/looper/state/looper.sqlite"
	if got := hostAdmissionStatePath(cfg); got != "/var/looper/state" {
		t.Fatalf("hostAdmissionStatePath() = %q, want the database's directory", got)
	}
}

// A symlinked database file is followed to its target before the parent
// directory is selected. SQLite writes to the target, so the disk signal must
// measure the target's filesystem, not the one that merely holds the link;
// taking filepath.Dir before resolving would admit work based on the link's
// filesystem while the real database volume is full.
func TestHostAdmissionStatePathResolvesSymlinkedDatabase(t *testing.T) {
	t.Parallel()

	linkDir := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "looper.sqlite")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatalf("create target database: %v", err)
	}
	link := filepath.Join(linkDir, "looper.sqlite")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create database symlink: %v", err)
	}

	// TempDir may itself sit behind a symlink (e.g. /var -> /private/var on
	// macOS), so compare against the resolved target directory, not the raw one.
	want, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		t.Fatalf("resolve target directory: %v", err)
	}

	cfg := config.Config{}
	cfg.Storage.DBPath = link
	if got := hostAdmissionStatePath(cfg); got != want {
		t.Fatalf("hostAdmissionStatePath() = %q, want target directory %q (not the link's %q)", got, want, linkDir)
	}
}

// A SQLite file: URI must resolve to the real filesystem directory, not the URI
// prefix that filepath.Dir would produce. Without normalization the disk signal
// stats "file:/var/lib/looper", statfs fails, and Evaluate admits on the absent
// disk fields — silently disabling the guard for this configuration.
func TestHostAdmissionStatePathResolvesSQLiteFileURI(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Storage.DBPath = "file:/var/lib/looper/looper.sqlite?cache=shared"
	if got := hostAdmissionStatePath(cfg); got != "/var/lib/looper" {
		t.Fatalf("hostAdmissionStatePath() = %q, want /var/lib/looper", got)
	}
}

// An opaque file: URI (no authority) resolves to the relative path SQLite writes.
func TestHostAdmissionStatePathResolvesOpaqueFileURI(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Storage.DBPath = "file:looper.sqlite?_busy_timeout=5000"
	if got := hostAdmissionStatePath(cfg); got != "." {
		t.Fatalf("hostAdmissionStatePath() = %q, want .", got)
	}
}

// A memory database has no filesystem to measure. The path must fall through to
// the default home rather than a URI prefix, so the disk signal still reads a
// real volume instead of erroring on every sample.
func TestHostAdmissionStatePathFallsThroughForMemoryDatabase(t *testing.T) {
	t.Parallel()

	cfg := config.Config{}
	cfg.Storage.DBPath = ":memory:"
	home, err := config.DefaultLooperHome()
	if err != nil {
		t.Fatalf("DefaultLooperHome() error = %v", err)
	}
	if got := hostAdmissionStatePath(cfg); got != home {
		t.Fatalf("hostAdmissionStatePath() = %q, want default home %q for a memory database", got, home)
	}

	cfg.Storage.DBPath = "file:looper.sqlite?mode=memory"
	if got := hostAdmissionStatePath(cfg); got != home {
		t.Fatalf("hostAdmissionStatePath() = %q, want default home %q for a memory-mode URI", got, home)
	}
}

func TestThresholdsFromConfigConvertsGigabytes(t *testing.T) {
	t.Parallel()

	thresholds := thresholdsFromConfig(config.ResourceGuardConfig{MinDiskFreeGB: 2.5})
	if want := uint64(2.5 * float64(1<<30)); thresholds.MinDiskFreeBytes != want {
		t.Fatalf("MinDiskFreeBytes = %d, want %d", thresholds.MinDiskFreeBytes, want)
	}
}

// The claim pass runs several times per second. A sustained hold must log once
// per decision transition and once per fresh sample, not on every call, or it
// churns the log and consumes the disk the guard protects.
func TestHostAdmissionGateThrottlesRepeatedHoldWarnings(t *testing.T) {
	t.Parallel()

	const gib = uint64(1) << 30
	current := time.Unix(0, 0)
	// Both signals trip under the default config: disk is below both floors and
	// load exceeds 2/CPU. This lets a threshold edit transition the decision on
	// the cached sample without waiting for a fresh read.
	pressure := hostresources.Snapshot{DiskFreeBytes: freeBytes(2 * gib), DiskTotalBytes: freeBytes(500 * gib), Load1: loadPtr(40), NumCPU: 10}
	gate := newHostAdmissionGate("/state", func() time.Time { return current })
	gate.read = func(string) hostresources.Snapshot { return pressure }

	hold := gate.Decide(guardConfig())
	if hold == nil || hold.Admit {
		t.Fatalf("Decide() = %#v, want a hold", hold)
	}
	if !gate.ShouldLogHold(hold) {
		t.Fatal("ShouldLogHold() = false on the first hold, want true")
	}
	if gate.ShouldLogHold(hold) {
		t.Fatal("ShouldLogHold() = true on a repeated hold on the same sample, want false")
	}

	// A decision transition on the cached sample logs immediately: an operator
	// relaxing the disk floor sees the load-only hold at once, not after the
	// sample expires. This is the hot-edit path the gate exists to support.
	relaxDisk := guardConfig()
	relaxDisk.MinDiskFreeGB = 0
	relaxDisk.MinDiskFreePercent = 0
	transition := gate.Decide(relaxDisk)
	if transition == nil || transition.Admit {
		t.Fatalf("Decide() = %#v, want a load-only hold", transition)
	}
	if !gate.ShouldLogHold(transition) {
		t.Fatal("ShouldLogHold() = false on a decision transition, want true")
	}
	if gate.ShouldLogHold(transition) {
		t.Fatal("ShouldLogHold() = true on a repeated transition, want false")
	}

	// A fresh sample re-logs the same hold: the operator still sees pressure
	// at the sample cadence rather than once on the first sample only.
	current = current.Add(hostAdmissionSampleInterval + time.Second)
	hold = gate.Decide(relaxDisk)
	if !gate.ShouldLogHold(hold) {
		t.Fatal("ShouldLogHold() = false on a fresh sample, want true")
	}

	// An admitting decision resets the memory, so the next hold logs at once.
	admit := gate.Decide(config.ResourceGuardConfig{Enabled: true, MinDiskFreePercent: 1, MinDiskFreeGB: 1, MaxLoadPerCPU: 0})
	if admit == nil || !admit.Admit {
		t.Fatalf("Decide() = %#v, want admission", admit)
	}
	if gate.ShouldLogHold(admit) {
		t.Fatal("ShouldLogHold() = true on an admitting decision, want false")
	}
	current = current.Add(time.Second)
	nextHold := gate.Decide(guardConfig())
	if !gate.ShouldLogHold(nextHold) {
		t.Fatal("ShouldLogHold() = false on the first hold after an admit, want true")
	}
}
