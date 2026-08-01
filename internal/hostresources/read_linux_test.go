//go:build linux

package hostresources

import "testing"

// TestParseCPUCount verifies the /proc/cpuinfo decoder against a synthetic
// payload, so the host-wide CPU count is checked deterministically rather than
// depending on the host's CPU topology. This is what catches a scope mismatch:
// runtime.NumCPU() reports the process's affinity-restricted count, while the
// load average is host-wide, so the parser must count every host processor.
func TestParseCPUCount(t *testing.T) {
	// A representative /proc/cpuinfo excerpt: two logical processors, each with
	// its own "processor" index line plus model/frequency lines that must not be
	// counted.
	data := []byte("processor\t: 0\nmodel name\t: Intel(R) Core(TM)\ncpu MHz\t: 3200.000\n\nprocessor\t: 1\nmodel name\t: Intel(R) Core(TM)\ncpu MHz\t: 3200.000\n")
	if got := parseCPUCount(data); got != 2 {
		t.Fatalf("parseCPUCount() = %d, want 2", got)
	}

	if got := parseCPUCount([]byte("")); got != 0 {
		t.Fatalf("parseCPUCount(empty) = %d, want 0", got)
	}

	// A 64-core host: the count must reach 64 so a host-wide load below the
	// ceiling admits when looper is restricted to a small cgroup slice.
	big := make([]byte, 0, 64*40)
	for i := 0; i < 64; i++ {
		big = append(big, []byte("processor\t: 0\ncpu MHz\t: 3200.000\n\n")...)
	}
	if got := parseCPUCount(big); got != 64 {
		t.Fatalf("parseCPUCount() = %d, want 64", got)
	}
}
