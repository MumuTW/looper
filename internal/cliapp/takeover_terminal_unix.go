//go:build darwin || linux

package cliapp

import (
	"errors"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"
)

var takeoverTerminalSignalMu sync.Mutex

func takeoverProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func takeoverExitWasInterrupt(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ((waitStatus.Signaled() && waitStatus.Signal() == syscall.SIGINT) || exitErr.ExitCode() == 128+int(syscall.SIGINT))
}

func systemTakeoverTerminalControl() takeoverTerminalControl {
	return takeoverTerminalControl{
		foregroundProcessGroup: func(fd int) (int, bool, error) {
			var pgrp int32
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp)))
			if errno != 0 {
				return 0, false, nil
			}
			return int(pgrp), true, nil
		},
		setForegroundProcessGroup: func(fd int, pgrp int) error {
			takeoverTerminalSignalMu.Lock()
			defer takeoverTerminalSignalMu.Unlock()
			// The parent is a background process group while the interactive child
			// owns the terminal. Ignore SIGTTOU only around the restoring ioctl.
			signal.Ignore(syscall.SIGTTOU)
			defer signal.Reset(syscall.SIGTTOU)
			value := int32(pgrp)
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCSPGRP), uintptr(unsafe.Pointer(&value)))
			if errno != 0 {
				return errno
			}
			return nil
		},
	}
}
