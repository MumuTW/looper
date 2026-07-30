//go:build darwin || linux

package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// askSentinelProbeKind reports what the no-follow probe found at the sentinel
// path. The daemon must never follow an agent-controlled ask.json symlink with
// its own privileges, so the probe resolves the path component-by-component
// without following symlinks and only hands back a regular file's descriptor.
type askSentinelProbeKind string

const (
	askSentinelMissing   askSentinelProbeKind = "missing"     // no sentinel file at all
	askSentinelRegular   askSentinelProbeKind = "regular"     // a regular file, opened without following links
	askSentinelSymlink   askSentinelProbeKind = "symlink"     // the sentinel itself is a symlink; target captured, never read
	askSentinelIrregular askSentinelProbeKind = "irregular"   // present but not a regular file or symlink (dir/socket/device)
	askSentinelProbeErr  askSentinelProbeKind = "probe-error" // an unexpected error (permissions, I/O)
)

type askSentinelProbe struct {
	kind          askSentinelProbeKind
	file          *os.File    // set for "regular"; caller owns Close
	info          os.FileInfo // fstat info for "regular"; Lstat info for "symlink"/"irregular"
	symlinkTarget string      // set for "symlink" (the link target string, NOT its contents)
	err           error       // set for "probe-error"
}

// probeAskSentinel walks the sentinel path beneath the worktree root without
// following symlinks at any component, so an agent cannot redirect the daemon's
// read at an arbitrary daemon-readable path. A symlink sentinel is reported as
// "symlink" with its target string (obtained via readlink, never by opening the
// target); a missing sentinel as "missing"; a regular file as "regular" with an
// open descriptor. The caller must close the returned file.
func probeAskSentinel(worktreePath, relPath string) askSentinelProbe {
	components := strings.Split(filepath.ToSlash(strings.TrimSpace(relPath)), "/")
	if len(components) == 0 {
		return askSentinelProbe{kind: askSentinelMissing}
	}
	rootFD, err := unix.Open(worktreePath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return askSentinelProbe{kind: askSentinelMissing}
		}
		return askSentinelProbe{kind: askSentinelProbeErr, err: fmt.Errorf("open worktree root: %w", err)}
	}
	defer unix.Close(rootFD)

	dirFD := rootFD
	for i, part := range components {
		final := i == len(components)-1
		if part == "" {
			continue
		}
		if final {
			return probeAskSentinelFinal(dirFD, part)
		}
		nextFD, err := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			return askSentinelProbe{kind: askSentinelMissing}
		}
		if err != nil {
			// A symlinked or non-directory intermediate component means there is
			// no real ask.json reachable without following a link; treat as missing.
			return askSentinelProbe{kind: askSentinelMissing}
		}
		if dirFD != rootFD {
			unix.Close(dirFD)
		}
		dirFD = nextFD
	}
	return askSentinelProbe{kind: askSentinelMissing}
}

func probeAskSentinelFinal(dirFD int, name string) askSentinelProbe {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return askSentinelProbe{kind: askSentinelMissing}
		}
		return askSentinelProbe{kind: askSentinelProbeErr, err: fmt.Errorf("stat sentinel: %w", err)}
	}
	mode := fileModeFromUnixStat(uint32(stat.Mode))
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		buf := make([]byte, 4096)
		n, err := unix.Readlinkat(dirFD, name, buf)
		if err != nil {
			return askSentinelProbe{kind: askSentinelProbeErr, err: fmt.Errorf("read sentinel symlink: %w", err)}
		}
		return askSentinelProbe{kind: askSentinelSymlink, symlinkTarget: string(buf[:n]), info: staticFileInfo{name: name, mode: mode, size: stat.Size}}
	case unix.S_IFREG:
		fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			// The file is regular but cannot be opened (e.g. mode 0). Return it as
			// a regular sentinel with no descriptor so the caller can rename it
			// whole (preserving content without reading) and fingerprint by size.
			return askSentinelProbe{kind: askSentinelRegular, info: staticFileInfo{name: name, mode: mode, size: stat.Size}, err: fmt.Errorf("open sentinel: %w", err)}
		}
		file := os.NewFile(uintptr(fd), name)
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return askSentinelProbe{kind: askSentinelProbeErr, err: fmt.Errorf("fstat sentinel: %w", err)}
		}
		return askSentinelProbe{kind: askSentinelRegular, file: file, info: info}
	default:
		return askSentinelProbe{kind: askSentinelIrregular, info: staticFileInfo{name: name, mode: mode, size: stat.Size}}
	}
}

// fileModeFromUnixStat maps a unix stat mode into an os.FileMode, preserving the
// type bits (symlink/dir) that a plain os.FileMode type conversion would drop.
func fileModeFromUnixStat(raw uint32) os.FileMode {
	mode := os.FileMode(raw).Perm()
	switch raw & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= os.ModeDir
	case unix.S_IFLNK:
		mode |= os.ModeSymlink
	}
	return mode
}

// staticFileInfo is a minimal os.FileInfo for non-opened probe results (symlink
// / irregular / unreadable regular), where no fd is held.
type staticFileInfo struct {
	name string
	mode os.FileMode
	size int64
}

func (s staticFileInfo) Name() string       { return s.name }
func (s staticFileInfo) Size() int64        { return s.size }
func (s staticFileInfo) Mode() os.FileMode  { return s.mode }
func (s staticFileInfo) ModTime() time.Time { return time.Time{} }
func (s staticFileInfo) IsDir() bool        { return s.mode.IsDir() }
func (s staticFileInfo) Sys() any           { return nil }
