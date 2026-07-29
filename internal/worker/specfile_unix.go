//go:build darwin || linux

package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openSpecFileBeneath walks from an already-open repository descriptor. Each
// component is opened without following symlinks, so a concurrent worktree
// replacement cannot redirect the final open outside the repository.
func openSpecFileBeneath(repoRoot, relativePath string) (*os.File, error) {
	rootFD, err := unix.Open(repoRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}

	currentFD := rootFD
	parts := strings.Split(relativePath, string(filepath.Separator))
	for index, part := range parts {
		final := index == len(parts)-1
		if final {
			var info unix.Stat_t
			if err := unix.Fstatat(currentFD, part, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				unix.Close(currentFD)
				return nil, err
			}
			if info.Mode&unix.S_IFMT != unix.S_IFREG {
				unix.Close(currentFD)
				return nil, fmt.Errorf("spec path is not a regular file")
			}
		}

		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if !final {
			flags |= unix.O_DIRECTORY
		}
		nextFD, err := unix.Openat(currentFD, part, flags, 0)
		unix.Close(currentFD)
		if err != nil {
			return nil, err
		}
		currentFD = nextFD
	}

	return os.NewFile(uintptr(currentFD), relativePath), nil
}
