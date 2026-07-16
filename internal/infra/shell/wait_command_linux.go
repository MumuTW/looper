//go:build linux

package shell

import (
	"errors"

	"golang.org/x/sys/unix"
)

func waitLeaderExitWithoutReap(pid int) error {
	for {
		err := unix.Waitid(unix.P_PID, pid, nil, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func processGroupHasLiveDescendants(int) (bool, error) {
	// Linux does not return EPERM when signaling an owned zombie-only group.
	// Preserve EPERM as a containment failure instead of weakening the contract.
	return true, nil
}
