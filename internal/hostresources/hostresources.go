// Package hostresources reads the host pressure signals the scheduler needs to
// decide whether starting more work is safe, and turns them into an admission
// decision.
//
// Scope is deliberately narrow. maxConcurrentRuns counts runs, but a run
// expands into an agent process plus whatever that agent spawns — compilers,
// test binaries, language servers — and the fan-out is not something looper
// controls or can predict. Counting runs therefore says nothing about what the
// host is actually carrying. These signals measure the host directly.
//
// Thresholds are relative on purpose. A free-space floor in gigabytes or a load
// ceiling as a bare number encodes one machine; the same config has to behave
// on a laptop and on a 64-core runner. Disk uses a percentage and an absolute
// floor together (whichever is stricter), and load is a multiple of
// runtime.NumCPU().
package hostresources

import (
	"fmt"
	"runtime"
	"strings"
)

// Snapshot is one reading of host pressure. Every field is optional: a signal
// this platform cannot read stays absent rather than defaulting to a value that
// would be indistinguishable from a healthy reading.
type Snapshot struct {
	// DiskFreeBytes and DiskTotalBytes describe the filesystem holding the
	// daemon's state directory — the one that matters, because that is where
	// SQLite writes.
	DiskFreeBytes  *uint64 `json:"diskFreeBytes,omitempty"`
	DiskTotalBytes *uint64 `json:"diskTotalBytes,omitempty"`
	// Load1 is the 1-minute load average.
	Load1 *float64 `json:"load1,omitempty"`
	// NumCPU is what Load1 is measured against.
	NumCPU int `json:"numCpu"`
	// SwapUsedBytes and SwapTotalBytes are advisory and never gate admission.
	// See Thresholds for why memory is reported but not enforced.
	SwapUsedBytes  *uint64 `json:"swapUsedBytes,omitempty"`
	SwapTotalBytes *uint64 `json:"swapTotalBytes,omitempty"`
	// Unsupported names the signals this platform could not read at all.
	Unsupported []string `json:"unsupported,omitempty"`
	// Errors names signals whose read failed on a platform that supports them.
	Errors []string `json:"errors,omitempty"`
}

// Thresholds configures Evaluate.
//
// There is deliberately no memory threshold. On macOS a free-page count is not
// a usable proxy for memory pressure — compressed memory and swap mean a host
// can report very little "free" memory while running perfectly well, and a gate
// on that number would refuse work on a healthy machine. Sustained swap use is
// reported in the snapshot so an operator can see it, but nothing admits or
// refuses on it. A signal that would fire wrongly is worse than no signal:
// wrongly refusing work is invisible until someone notices the queue is idle.
type Thresholds struct {
	// MinDiskFreePercent refuses new work below this share of the filesystem.
	MinDiskFreePercent float64
	// MinDiskFreeBytes refuses new work below this absolute floor. Applied
	// together with the percentage; whichever is stricter wins. A percentage
	// alone is useless on a 4 TB disk (1% is 40 GB of slack nobody needs) and
	// an absolute floor alone is useless on a small one.
	MinDiskFreeBytes uint64
	// MaxLoadPerCPU refuses new work above NumCPU * this. Zero disables the
	// load signal.
	MaxLoadPerCPU float64
}

// Decision is Evaluate's verdict.
type Decision struct {
	// Admit reports whether new work may start. In-flight work is never the
	// subject of this decision — see the package's callers.
	Admit bool `json:"admit"`
	// Reasons names every tripped signal, stable and machine-readable.
	Reasons []string `json:"reasons,omitempty"`
	// Detail is one operator-facing sentence per tripped signal.
	Detail []string `json:"detail,omitempty"`
}

// Reason codes. Stable strings: they reach status payloads and notifications.
const (
	ReasonDiskLow  = "host_disk_low"
	ReasonLoadHigh = "host_load_high"
)

// Evaluate decides whether the host can carry more work.
//
// An absent signal admits. That is the safe default in the direction that
// matters: a platform whose load average looper cannot read, or a statfs that
// failed, must not silently halt the scheduler — an operator would see an idle
// queue with no cause. Refusing work is the exceptional outcome and needs a
// reading that positively says so.
func Evaluate(snapshot Snapshot, thresholds Thresholds) Decision {
	decision := Decision{Admit: true}

	if snapshot.DiskFreeBytes != nil {
		free := *snapshot.DiskFreeBytes
		if thresholds.MinDiskFreeBytes > 0 && free < thresholds.MinDiskFreeBytes {
			decision.refuse(ReasonDiskLow, fmt.Sprintf(
				"%s free on the state filesystem is below the %s floor",
				formatBytes(free), formatBytes(thresholds.MinDiskFreeBytes)))
		} else if thresholds.MinDiskFreePercent > 0 && snapshot.DiskTotalBytes != nil && *snapshot.DiskTotalBytes > 0 {
			percent := 100 * float64(free) / float64(*snapshot.DiskTotalBytes)
			if percent < thresholds.MinDiskFreePercent {
				decision.refuse(ReasonDiskLow, fmt.Sprintf(
					"%.1f%% free on the state filesystem is below the %.1f%% floor",
					percent, thresholds.MinDiskFreePercent))
			}
		}
	}

	if snapshot.Load1 != nil && thresholds.MaxLoadPerCPU > 0 {
		cpus := snapshot.NumCPU
		if cpus <= 0 {
			cpus = runtime.NumCPU()
		}
		ceiling := thresholds.MaxLoadPerCPU * float64(cpus)
		if ceiling > 0 && *snapshot.Load1 > ceiling {
			decision.refuse(ReasonLoadHigh, fmt.Sprintf(
				"1-minute load %.2f exceeds %.2f (%d CPUs x %.2f)",
				*snapshot.Load1, ceiling, cpus, thresholds.MaxLoadPerCPU))
		}
	}

	return decision
}

func (d *Decision) refuse(reason, detail string) {
	d.Admit = false
	d.Reasons = append(d.Reasons, reason)
	d.Detail = append(d.Detail, detail)
}

// Summary renders a decision's detail as one line for logs and notifications.
func (d Decision) Summary() string {
	return strings.Join(d.Detail, "; ")
}

func formatBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := uint64(unit), 0
	for n := value / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTP"[exp])
}
