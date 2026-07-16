//go:build !darwin && !linux

package cliapp

import "syscall"

func takeoverProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func takeoverExitWasInterrupt(error) bool { return false }

func systemTakeoverTerminalControl() takeoverTerminalControl {
	return takeoverTerminalControl{
		foregroundProcessGroup:    func(int) (int, bool, error) { return 0, false, nil },
		setForegroundProcessGroup: func(int, int) error { return nil },
	}
}
