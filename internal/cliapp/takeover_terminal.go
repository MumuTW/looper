package cliapp

import (
	"fmt"
	"os"
	"os/exec"
)

type takeoverTerminalControl struct {
	foregroundProcessGroup    func(fd int) (pgrp int, isTerminal bool, err error)
	setForegroundProcessGroup func(fd int, pgrp int) error
}

func prepareTakeoverTerminal(cmd *exec.Cmd, stdin *os.File, control takeoverTerminalControl) (func() error, error) {
	noRestore := func() error { return nil }
	if cmd == nil || stdin == nil || control.foregroundProcessGroup == nil || control.setForegroundProcessGroup == nil {
		return noRestore, nil
	}
	fd := int(stdin.Fd())
	parentPgrp, isTerminal, err := control.foregroundProcessGroup(fd)
	if err != nil {
		return nil, fmt.Errorf("inspect takeover terminal: %w", err)
	}
	if !isTerminal {
		return noRestore, nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = takeoverProcessAttributes()
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Foreground = true
	cmd.SysProcAttr.Ctty = fd
	return func() error {
		if err := control.setForegroundProcessGroup(fd, parentPgrp); err != nil {
			return fmt.Errorf("restore takeover terminal foreground process group: %w", err)
		}
		return nil
	}, nil
}
