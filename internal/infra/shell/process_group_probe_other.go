//go:build !linux && !darwin

package shell

// Without a portable process-state inspection API, leave inspection to the
// kill(2) signalability probe in ProcessGroupRunnable / ProcessRunnable.
func inspectProcessGroupRunnable(int) (runnable bool, inspected bool) {
	return false, false
}

func inspectProcessRunnable(int) (runnable bool, inspected bool) {
	return false, false
}
