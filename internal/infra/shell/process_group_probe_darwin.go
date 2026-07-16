//go:build darwin

package shell

import "golang.org/x/sys/unix"

// szomb is SZOMB from Darwin/BSD sys/proc.h.
const szomb int8 = 5

func inspectProcessGroupRunnable(pgid int) (runnable bool, inspected bool) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, false
	}
	for _, kp := range kps {
		if kp.Proc.P_stat != szomb {
			return true, true
		}
	}
	return false, true
}

func inspectProcessRunnable(pid int) (runnable bool, inspected bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// Process disappeared between kill(0) and the sysctl read.
		return false, true
	}
	return kp.Proc.P_stat != szomb, true
}
