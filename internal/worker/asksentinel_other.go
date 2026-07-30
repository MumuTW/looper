//go:build !darwin && !linux

package worker

import (
	"fmt"
	"os"
	"path/filepath"
)

func probeAskSentinel(worktreePath, relPath string) askSentinelProbe {
	// Secure no-follow sentinel reads are unsupported on this platform; fall
	// back to a plain Lstat so the daemon still fails closed on a symlink
	// sentinel rather than following it.
	path := filepath.Join(worktreePath, relPath)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return askSentinelProbe{kind: askSentinelMissing}
		}
		return askSentinelProbe{kind: askSentinelProbeErr, err: fmt.Errorf("lstat sentinel: %w", err)}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return askSentinelProbe{kind: askSentinelProbeErr, err: fmt.Errorf("read sentinel symlink: %w", err)}
		}
		return askSentinelProbe{kind: askSentinelSymlink, symlinkTarget: target, info: info}
	}
	if !info.Mode().IsRegular() {
		return askSentinelProbe{kind: askSentinelIrregular, info: info}
	}
	file, err := os.Open(path)
	if err != nil {
		return askSentinelProbe{kind: askSentinelRegular, info: info, err: fmt.Errorf("open sentinel: %w", err)}
	}
	return askSentinelProbe{kind: askSentinelRegular, file: file, info: info}
}
