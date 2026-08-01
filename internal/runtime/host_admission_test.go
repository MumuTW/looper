package runtime

import (
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/hostresources"
)

func freeBytes(value uint64) *uint64 { return &value }

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

func TestThresholdsFromConfigConvertsGigabytes(t *testing.T) {
	t.Parallel()

	thresholds := thresholdsFromConfig(config.ResourceGuardConfig{MinDiskFreeGB: 2.5})
	if want := uint64(2.5 * float64(1<<30)); thresholds.MinDiskFreeBytes != want {
		t.Fatalf("MinDiskFreeBytes = %d, want %d", thresholds.MinDiskFreeBytes, want)
	}
}
