//go:build linux

package hostresources

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

func numCPU() int { return runtime.NumCPU() }

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
