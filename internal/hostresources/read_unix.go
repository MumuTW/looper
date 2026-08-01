//go:build darwin || linux

package hostresources

import (
	"golang.org/x/sys/unix"
)

// Read samples every signal this platform supports for the filesystem holding
// statePath. A failed signal is named in Snapshot.Errors and left absent rather
// than reported as a value, so Evaluate cannot mistake a failure for a healthy
// reading.
func Read(statePath string) Snapshot {
	snapshot := Snapshot{NumCPU: numCPU()}
	readDisk(&snapshot, statePath)
	readLoad(&snapshot)
	readSwap(&snapshot)
	return snapshot
}

func readDisk(snapshot *Snapshot, statePath string) {
	var stat unix.Statfs_t
	if err := unix.Statfs(statePath, &stat); err != nil {
		snapshot.Errors = append(snapshot.Errors, "disk: "+err.Error())
		return
	}
	// Bsize is uint32 on darwin and int64 on linux; both widen cleanly.
	blockSize := uint64(stat.Bsize)
	// Bavail, not Bfree: reserved blocks are not available to the daemon, and
	// counting them would let the gate stay open right up to a write failure.
	free := uint64(stat.Bavail) * blockSize
	total := uint64(stat.Blocks) * blockSize
	snapshot.DiskFreeBytes = &free
	snapshot.DiskTotalBytes = &total
}
