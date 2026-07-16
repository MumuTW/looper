package shell

import (
	"errors"
	"syscall"
)

// ProcessGroupRunnable reports whether process group pgid still has any
// non-zombie member. kill(-pgid, 0) can succeed for zombie-only groups on
// Linux until init reaps them; those groups are not runnable and must not be
// treated as live ownership barriers after SIGKILL.
func ProcessGroupRunnable(pgid int) (bool, error) {
	if pgid <= 0 {
		return false, nil
	}
	err := syscall.Kill(-pgid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		// Signalable: may still be zombie-only.
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
	runnable, inspected := inspectProcessGroupRunnable(pgid)
	if !inspected {
		// Fall back to signalability when member state cannot be inspected.
		return true, nil
	}
	return runnable, nil
}

// ProcessRunnable reports whether pid still refers to a non-zombie process.
// Zombies remain signalable via kill(pid, 0) until reaped, but are not live
// work that ownership barriers should wait on.
func ProcessRunnable(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
	runnable, inspected := inspectProcessRunnable(pid)
	if !inspected {
		return true, nil
	}
	return runnable, nil
}
