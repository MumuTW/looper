//go:build darwin

package hostresources

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestParseLoadavg verifies the darwin vm.loadavg decoder against a synthetic
// payload, so the struct offset and fixed-point scale are checked
// deterministically rather than depending on the host being busy enough to
// report a nonzero load.
//
// The first version of this reader read fscale from offset 12 — the four bytes
// of padding before the real field at 16 — divided by a garbage denominator,
// and yielded exactly 0.00. A live reading of 0.00 is legitimate on an idle or
// recently booted host, so a plausibility bound that accepts zero cannot tell
// that bug from a quiet machine, and a bound that rejects zero makes the test
// suite environment-dependent. This test is what catches that bug instead.
func TestParseLoadavg(t *testing.T) {
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }
	// ldavg is three uint32 in [0,12); four bytes of padding sit at 12; fscale
	// is a uint64 at 16. Verified against uptime: 6957/2048 = 3.40.
	payload := make([]byte, 24)
	binary.LittleEndian.PutUint32(payload[0:4], 6957)   // ldavg[0]
	binary.LittleEndian.PutUint64(payload[16:24], 2048) // fscale at offset 16

	load, parseErr := parseLoadavg(payload)
	if parseErr != "" {
		t.Fatalf("parseLoadavg error = %q, want none", parseErr)
	}
	if math.Abs(load-3.40) > 0.01 {
		t.Fatalf("load = %.4f, want ~3.40", load)
	}

	// The four padding bytes at [12,16) must be ignored. Filling them with a
	// value that would distort the result if fscale were read at 12 confirms
	// the parser reads fscale from offset 16, not the padding before it.
	payload[12], payload[13], payload[14], payload[15] = 0xFF, 0xFF, 0xFF, 0xFF
	load, parseErr = parseLoadavg(payload)
	if parseErr != "" {
		t.Fatalf("parseLoadavg error with padding = %q, want none", parseErr)
	}
	if math.Abs(load-3.40) > 0.01 {
		t.Fatalf("load with padding = %.4f, want ~3.40 (fscale must be read at offset 16, not 12)", load)
	}

	// A zero load is a legitimate reading on an idle host, not a parse failure.
	zeroPayload := make([]byte, 24)
	binary.LittleEndian.PutUint64(zeroPayload[16:24], 2048)
	load, parseErr = parseLoadavg(zeroPayload)
	if parseErr != "" {
		t.Fatalf("parseLoadavg zero-load error = %q, want none", parseErr)
	}
	if load != 0 {
		t.Fatalf("zero load = %v, want 0", load)
	}

	// An implausible scale is rejected rather than publishing a load wrong by
	// orders of magnitude — the guard against a future layout change.
	badScale := make([]byte, 24)
	binary.LittleEndian.PutUint32(badScale[0:4], 6957)
	binary.LittleEndian.PutUint64(badScale[16:24], 0)
	if _, parseErr = parseLoadavg(badScale); parseErr == "" {
		t.Fatal("parseLoadavg accepted a zero scale")
	}

	// A short payload is rejected.
	if _, parseErr = parseLoadavg(make([]byte, 16)); parseErr == "" {
		t.Fatal("parseLoadavg accepted a short payload")
	}
}
