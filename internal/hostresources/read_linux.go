//go:build linux

package hostresources

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

func numCPU() int {
	if n := hostCPUCount(); n > 0 {
		return n
	}
	return runtime.NumCPU()
}

// hostCPUCount returns the host-wide logical CPU count from /proc/cpuinfo.
//
// runtime.NumCPU() reports the CPUs in this process's scheduling affinity, which
// a cgroup cpu.cfs_quota or taskset can pin to a subset of the host. /proc/loadavg
// is always host-wide, so comparing it against a restricted NumCPU trips the load
// ceiling on a busy host even when looper's own allocation is idle — on a 64-core
// node with looper restricted to 2 CPUs, a node load above 4 exceeds the default
// 2.0 ceiling and withholds every claim. The load average and the CPU count must
// be the same scope; /proc/cpuinfo reports the host's processors regardless of
// this process's affinity, so it matches /proc/loadavg's scope. A read failure
// falls back to runtime.NumCPU() rather than disabling the signal.
func hostCPUCount() int {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	return parseCPUCount(data)
}

// parseCPUCount counts the "processor" entries in /proc/cpuinfo, one per logical
// CPU. Extracted from hostCPUCount so the count can be verified against a
// synthetic payload rather than depending on the host's CPU topology.
func parseCPUCount(data []byte) int {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	return count
}

func readLoad(snapshot *Snapshot) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		snapshot.Errors = append(snapshot.Errors, "load: "+err.Error())
		return
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		snapshot.Errors = append(snapshot.Errors, "load: empty /proc/loadavg")
		return
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		snapshot.Errors = append(snapshot.Errors, "load: "+err.Error())
		return
	}
	snapshot.Load1 = &load
}

// readSwap is advisory only; see Thresholds for why memory never gates.
func readSwap(snapshot *Snapshot) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		snapshot.Errors = append(snapshot.Errors, "swap: "+err.Error())
		return
	}
	var total, free uint64
	var haveTotal, haveFree bool
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch key {
		case "SwapTotal":
			total, haveTotal = parseMeminfoKB(value)
		case "SwapFree":
			free, haveFree = parseMeminfoKB(value)
		}
	}
	if !haveTotal || !haveFree || free > total {
		return
	}
	used := total - free
	snapshot.SwapTotalBytes = &total
	snapshot.SwapUsedBytes = &used
}

func parseMeminfoKB(value string) (uint64, bool) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, false
	}
	parsed, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed * 1024, true
}
