//go:build linux

package shell

import (
	"os"
	"strconv"
	"strings"
)

func inspectProcessGroupRunnable(pgid int) (runnable bool, inspected bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		state, memberPgid, ok := readLinuxProcState(pid)
		if !ok || memberPgid != pgid {
			continue
		}
		if !isLinuxZombieState(state) {
			return true, true
		}
	}
	// Full /proc scan completed: group is empty or zombie-only.
	return false, true
}

func inspectProcessRunnable(pid int) (runnable bool, inspected bool) {
	state, _, ok := readLinuxProcState(pid)
	if !ok {
		// Process disappeared between kill(0) and the stat read.
		return false, true
	}
	return !isLinuxZombieState(state), true
}

func isLinuxZombieState(state byte) bool {
	// Z = zombie, X/x = dead (Linux).
	return state == 'Z' || state == 'X' || state == 'x'
}

func readLinuxProcState(pid int) (state byte, pgid int, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	// /proc/<pid>/stat: pid (comm) state ppid pgrp ...
	// comm may contain spaces and parentheses, so split on the final ") ".
	stat := string(data)
	idx := strings.LastIndex(stat, ") ")
	if idx < 0 || idx+2 >= len(stat) {
		return 0, 0, false
	}
	rest := stat[idx+2:]
	fields := strings.Fields(rest)
	// fields[0]=state, fields[1]=ppid, fields[2]=pgrp
	if len(fields) < 3 || len(fields[0]) == 0 {
		return 0, 0, false
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], pgid, true
}
