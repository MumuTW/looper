package shell

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ConfigureProcessGroup gives one command an isolated Unix process group and
// bounds Wait when descendants retain inherited output descriptors.
func ConfigureProcessGroup(cmd *exec.Cmd, descendantDrainGrace time.Duration) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if descendantDrainGrace > 0 {
		cmd.WaitDelay = descendantDrainGrace
	}
}

func SignalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

func KillProcessGroup(cmd *exec.Cmd) error {
	return SignalProcessGroup(cmd, syscall.SIGKILL)
}

// WaitProcessGroup observes the group leader's exit without reaping it, kills
// every remaining member while the zombie leader still anchors the numeric
// PGID, and only then calls Cmd.Wait. This removes the probe/kill reuse window
// created by reaping the leader first.
func WaitProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return os.ErrProcessDone
	}
	if err := waitLeaderExitWithoutReap(cmd.Process.Pid); err != nil {
		return err
	}
	killErr := KillProcessGroup(cmd)
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	} else if errors.Is(killErr, syscall.EPERM) {
		hasLiveDescendants, inspectErr := processGroupHasLiveDescendants(cmd.Process.Pid)
		if inspectErr != nil {
			killErr = errors.Join(killErr, inspectErr)
		} else if !hasLiveDescendants {
			killErr = nil
		}
	}
	waitErr := cmd.Wait()
	return errors.Join(waitErr, killErr)
}
