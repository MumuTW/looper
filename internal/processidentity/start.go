// Package processidentity reads stable operating-system process birth identity.
package processidentity

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// StartTime returns an operating-system process birth token for pid.
func StartTime(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("process identity: invalid pid %d", pid)
	}
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return 0, err
		}
		// comm is parenthesized and may contain spaces. Fields after the closing
		// parenthesis begin at proc field 3; starttime is field 22.
		endComm := strings.LastIndexByte(string(stat), ')')
		if endComm < 0 {
			return 0, fmt.Errorf("process identity: unexpected /proc stat shape")
		}
		fields := strings.Fields(string(stat)[endComm+1:])
		if len(fields) < 20 {
			return 0, fmt.Errorf("process identity: unexpected /proc stat shape")
		}
		start, err := strconv.ParseInt(fields[19], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("process identity: parse /proc start time: %w", err)
		}
		return start, nil
	}

	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return 0, fmt.Errorf("process identity: empty process start")
	}
	parsed, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", value, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("process identity: parse process start: %w", err)
	}
	return parsed.UnixNano(), nil
}
