//go:build !darwin && !linux

package hostresources

import "runtime"

// Read reports every signal as unsupported on platforms this package has no
// implementation for. Evaluate admits on absent signals, so an unsupported
// platform behaves exactly as it did before the gate existed rather than
// halting the scheduler on a reading looper never took.
func Read(string) Snapshot {
	return Snapshot{
		NumCPU:      runtime.NumCPU(),
		Unsupported: []string{"disk", "load", "swap"},
	}
}
