package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExecutionForceKillAfterFailedBarrierOnlyReprobesOwnedGroup(t *testing.T) {
	markerPath := filepath.Join(t.TempDir(), "natural-exit")
	cmd := exec.Command("/bin/sh", "-c", `sleep 0.05; : > "$MARKER_PATH"`)
	cmd.Env = append(os.Environ(), "MARKER_PATH="+markerPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process group: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})

	execution := &execution{
		process:                 cmd,
		processGroupKillSent:    true,
		processGroupSignalsDone: true,
	}
	if err := execution.ForceKill(); err != nil {
		t.Fatalf("ForceKill() re-probe error = %v", err)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("ForceKill() resent SIGKILL instead of waiting for the owned group to disappear: %v", err)
	}
	execution.mu.Lock()
	resolved := execution.processGroupResolved
	execution.mu.Unlock()
	if !resolved {
		t.Fatal("ForceKill() returned nil without confirming process-group disappearance")
	}
}
