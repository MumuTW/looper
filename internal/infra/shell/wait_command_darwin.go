//go:build darwin

package shell

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func waitLeaderExitWithoutReap(pid int) error {
	const (
		waitIDPID  = 1
		waitNoHang = 0x00000001
		waitExited = 0x00000004
		waitNoWait = 0x00000020
	)
	// Darwin exposes waitid(2), including WNOWAIT, but x/sys/unix does not
	// wrap it. WNOHANG avoids relying on the raw blocking syscall's interaction
	// with Go's runtime; si_pid is non-zero only once this child is waitable.
	started := time.Now()
	for {
		info := darwinSiginfo{}
		_, _, errno := unix.Syscall6(
			unix.SYS_WAITID,
			waitIDPID,
			uintptr(pid),
			uintptr(unsafe.Pointer(&info)),
			waitNoHang|waitExited|waitNoWait,
			0,
			0,
		)
		if errno == 0 && info.pid == int32(pid) {
			return nil
		}
		if errors.Is(errno, unix.EINTR) {
			continue
		}
		if errno != 0 {
			return fmt.Errorf("wait for child exit without reaping: %w", errno)
		}
		pollInterval := time.Millisecond
		if time.Since(started) >= time.Second {
			pollInterval = 50 * time.Millisecond
		}
		time.Sleep(pollInterval)
	}
}

// darwinSiginfo matches siginfo_t from <sys/signal.h>. Only pid is read.
type darwinSiginfo struct {
	signo  int32
	errno  int32
	code   int32
	pid    int32
	uid    uint32
	status int32
	addr   uintptr
	value  uintptr
	band   int64
	pad    [7]uint64
}

func processGroupHasLiveDescendants(leaderPID int) (bool, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", leaderPID)
	if err != nil {
		return false, fmt.Errorf("inspect process group after permission failure: %w", err)
	}
	const zombieProcessState = 5
	for _, process := range processes {
		if int(process.Proc.P_pid) != leaderPID && process.Proc.P_stat != zombieProcessState {
			return true, nil
		}
	}
	return false, nil
}
