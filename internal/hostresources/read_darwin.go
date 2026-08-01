//go:build darwin

package hostresources

import (
	"encoding/binary"
	"runtime"

	"golang.org/x/sys/unix"
)

func numCPU() int { return runtime.NumCPU() }

// readLoad reads vm.loadavg, which returns a struct loadavg: three fixed-point
// load samples plus the scale they are expressed in. The scale must be read
// from the same struct rather than assumed — it is not a constant across
// platforms, and hardcoding 2048 would silently misreport load everywhere it
// differs.
func readLoad(snapshot *Snapshot) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		snapshot.Errors = append(snapshot.Errors, "load: "+err.Error())
		return
	}
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }
	//
	// fixpt_t is uint32, so ldavg occupies [0,12). fscale is a long, which
	// needs 8-byte alignment on every darwin target, so the compiler inserts
	// four bytes of padding and fscale lands at 16 — not at 12. Reading it at
	// 12 yields a garbage scale and a load that rounds to zero, which looks
	// exactly like a quiet machine and would leave the load gate permanently
	// open. Verified against `uptime` on arm64: 6957/2048 = 3.40.
	const (
		loadavgSize  = 24
		fscaleOffset = 16
		minPlausible = 1
		maxPlausible = 1 << 24
	)
	if len(raw) < loadavgSize {
		snapshot.Errors = append(snapshot.Errors, "load: short vm.loadavg payload")
		return
	}
	scale := binary.LittleEndian.Uint64(raw[fscaleOffset : fscaleOffset+8])
	// Range-check rather than trust the offset: if a future darwin changes the
	// layout, report the signal as unreadable instead of publishing a load that
	// is wrong by orders of magnitude.
	if scale < minPlausible || scale > maxPlausible {
		snapshot.Errors = append(snapshot.Errors, "load: implausible vm.loadavg scale")
		return
	}
	load := float64(binary.LittleEndian.Uint32(raw[0:4])) / float64(scale)
	snapshot.Load1 = &load
}

// readSwap is advisory only; see Thresholds for why memory never gates.
func readSwap(snapshot *Snapshot) {
	raw, err := unix.SysctlRaw("vm.swapusage")
	if err != nil {
		snapshot.Errors = append(snapshot.Errors, "swap: "+err.Error())
		return
	}
	// struct xsw_usage { uint64 xsu_total, xsu_avail, xsu_used; ... }
	if len(raw) < 24 {
		snapshot.Errors = append(snapshot.Errors, "swap: short vm.swapusage payload")
		return
	}
	total := binary.LittleEndian.Uint64(raw[0:8])
	used := binary.LittleEndian.Uint64(raw[16:24])
	snapshot.SwapTotalBytes = &total
	snapshot.SwapUsedBytes = &used
}
