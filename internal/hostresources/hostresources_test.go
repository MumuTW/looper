package hostresources

import (
	"runtime"
	"slices"
	"testing"
)

func bytesPtr(value uint64) *uint64   { return &value }
func floatPtr(value float64) *float64 { return &value }

func TestEvaluate(t *testing.T) {
	const gib = uint64(1) << 30

	tests := []struct {
		name        string
		snapshot    Snapshot
		thresholds  Thresholds
		wantAdmit   bool
		wantReasons []string
	}{
		{
			name:       "healthy host admits",
			snapshot:   Snapshot{DiskFreeBytes: bytesPtr(200 * gib), DiskTotalBytes: bytesPtr(1000 * gib), Load1: floatPtr(2), NumCPU: 8},
			thresholds: Thresholds{MinDiskFreePercent: 5, MinDiskFreeBytes: 10 * gib, MaxLoadPerCPU: 1.5},
			wantAdmit:  true,
		},
		{
			name:        "absolute disk floor refuses even at a healthy percentage",
			snapshot:    Snapshot{DiskFreeBytes: bytesPtr(3 * gib), DiskTotalBytes: bytesPtr(4000 * gib), NumCPU: 8},
			thresholds:  Thresholds{MinDiskFreePercent: 0.01, MinDiskFreeBytes: 10 * gib},
			wantAdmit:   false,
			wantReasons: []string{ReasonDiskLow},
		},
		{
			name:        "percentage floor refuses even above the absolute floor",
			snapshot:    Snapshot{DiskFreeBytes: bytesPtr(12 * gib), DiskTotalBytes: bytesPtr(1000 * gib), NumCPU: 8},
			thresholds:  Thresholds{MinDiskFreePercent: 5, MinDiskFreeBytes: 10 * gib},
			wantAdmit:   false,
			wantReasons: []string{ReasonDiskLow},
		},
		{
			name:        "load is measured against CPU count",
			snapshot:    Snapshot{Load1: floatPtr(11), NumCPU: 8},
			thresholds:  Thresholds{MaxLoadPerCPU: 1.25},
			wantAdmit:   false,
			wantReasons: []string{ReasonLoadHigh},
		},
		{
			name: "the same load admits on a bigger host",
			// The whole point of a relative threshold: load 11 is distress on
			// 8 cores and idle on 64.
			snapshot:   Snapshot{Load1: floatPtr(11), NumCPU: 64},
			thresholds: Thresholds{MaxLoadPerCPU: 1.25},
			wantAdmit:  true,
		},
		{
			name:        "both signals report independently",
			snapshot:    Snapshot{DiskFreeBytes: bytesPtr(1 * gib), DiskTotalBytes: bytesPtr(1000 * gib), Load1: floatPtr(40), NumCPU: 8},
			thresholds:  Thresholds{MinDiskFreePercent: 5, MinDiskFreeBytes: 10 * gib, MaxLoadPerCPU: 1.25},
			wantAdmit:   false,
			wantReasons: []string{ReasonDiskLow, ReasonLoadHigh},
		},
		{
			name: "an unreadable signal admits",
			// A failed statfs or an unsupported platform must not halt the
			// scheduler: an operator would see an idle queue with no cause.
			snapshot:   Snapshot{NumCPU: 8, Unsupported: []string{"disk", "load"}},
			thresholds: Thresholds{MinDiskFreePercent: 90, MinDiskFreeBytes: 900 * gib, MaxLoadPerCPU: 0.01},
			wantAdmit:  true,
		},
		{
			name:       "a zero load threshold disables the signal",
			snapshot:   Snapshot{Load1: floatPtr(500), NumCPU: 8},
			thresholds: Thresholds{MaxLoadPerCPU: 0},
			wantAdmit:  true,
		},
		{
			name: "swap pressure never refuses",
			// Memory is reported, not enforced. A host deep in swap still
			// admits, because no free-page proxy is reliable enough to gate on.
			snapshot:   Snapshot{SwapUsedBytes: bytesPtr(30 * gib), SwapTotalBytes: bytesPtr(32 * gib), NumCPU: 8},
			thresholds: Thresholds{MinDiskFreePercent: 5, MinDiskFreeBytes: 10 * gib, MaxLoadPerCPU: 1.25},
			wantAdmit:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := Evaluate(test.snapshot, test.thresholds)
			if decision.Admit != test.wantAdmit {
				t.Fatalf("Admit = %v, want %v (detail: %s)", decision.Admit, test.wantAdmit, decision.Summary())
			}
			if !slices.Equal(decision.Reasons, test.wantReasons) {
				t.Fatalf("Reasons = %v, want %v", decision.Reasons, test.wantReasons)
			}
			if !decision.Admit && decision.Summary() == "" {
				t.Fatal("a refusal must carry operator-facing detail")
			}
		})
	}
}

// Read must produce values a human would recognise. A silently wrong scale
// factor or struct offset would otherwise only surface as a gate that never
// fires, or one that fires constantly.
func TestReadReturnsPlausibleValues(t *testing.T) {
	snapshot := Read(t.TempDir())

	if snapshot.NumCPU != runtime.NumCPU() {
		t.Fatalf("NumCPU = %d, want %d", snapshot.NumCPU, runtime.NumCPU())
	}

	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		if len(snapshot.Unsupported) == 0 {
			t.Fatal("an unimplemented platform must report its signals as unsupported")
		}
		return
	}

	if len(snapshot.Errors) > 0 {
		t.Fatalf("Read() errors = %v, want none on %s", snapshot.Errors, runtime.GOOS)
	}
	if snapshot.DiskFreeBytes == nil || snapshot.DiskTotalBytes == nil {
		t.Fatal("disk signals are unset on a supported platform")
	}
	if *snapshot.DiskTotalBytes == 0 || *snapshot.DiskFreeBytes > *snapshot.DiskTotalBytes {
		t.Fatalf("implausible disk reading: free=%d total=%d", *snapshot.DiskFreeBytes, *snapshot.DiskTotalBytes)
	}
	if snapshot.Load1 == nil {
		t.Fatal("load is unset on a supported platform")
	}
	// Strictly greater than zero, not merely non-negative. A wrong fixed-point
	// scale — the first version of the darwin reader took fscale from the
	// padding before it — divides by a garbage denominator and yields exactly
	// 0.00, which is indistinguishable from an idle host under a >= 0 bound
	// and would leave the load gate permanently open. A machine running its own
	// test suite is never at exactly zero load.
	if *snapshot.Load1 <= 0 || *snapshot.Load1 > 1024 {
		t.Fatalf("implausible load reading: %v", *snapshot.Load1)
	}
	if snapshot.SwapTotalBytes != nil && snapshot.SwapUsedBytes != nil && *snapshot.SwapUsedBytes > *snapshot.SwapTotalBytes {
		t.Fatalf("implausible swap reading: used=%d total=%d", *snapshot.SwapUsedBytes, *snapshot.SwapTotalBytes)
	}
}
