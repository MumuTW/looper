package cliapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestFeedbackCancellationStopsAgentDescendants(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	scriptPath := filepath.Join(dir, "fake-agent.sh")
	script := strings.Join([]string{
		"#!/bin/sh",
		`/bin/sh -c 'trap "" TERM; echo $$ > "$1"; while :; do sleep 1; done' sh "` + pidPath + `" </dev/null >/dev/null 2>&1 &`,
		"wait",
	}, "\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agent script: %v", err)
	}
	configPath := writeCLIConfigWithAgent(t, "http://127.0.0.1:1", string(config.AgentVendorOpenCode), map[string]any{"command": scriptPath})

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan int, 1)
	go func() {
		exitCode, _, _ := runAppWithContext(t, ctx, "feedback", "test cancellation", "--config", configPath)
		resultCh <- exitCode
	}()

	childPID := waitForCLIProcessPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	cancel()

	select {
	case exitCode := <-resultCh:
		if exitCode == 0 {
			t.Fatal("feedback exit code = 0 after cancellation, want non-zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("feedback did not return after cancellation")
	}
	waitForCLIProcessExit(t, childPID)
}

func TestFeedbackBoundsInheritedOutputPipeCleanup(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	scriptPath := filepath.Join(dir, "fake-agent.sh")
	script := strings.Join([]string{
		"#!/bin/sh",
		`/bin/sh -c 'echo $$ > "$1"; sleep 10' sh "` + pidPath + `" &`,
		"printf 'https://github.com/nexu-io/looper/issues/321\\n'",
	}, "\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agent script: %v", err)
	}
	configPath := writeCLIConfigWithAgent(t, "http://127.0.0.1:1", string(config.AgentVendorOpenCode), map[string]any{"command": scriptPath})

	type appResult struct {
		exitCode int
		stdout   string
	}
	resultCh := make(chan appResult, 1)
	go func() {
		exitCode, stdout, _ := runApp(t, "feedback", "test pipe cleanup", "--config", configPath)
		resultCh <- appResult{exitCode: exitCode, stdout: stdout}
	}()

	childPID := waitForCLIProcessPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	select {
	case result := <-resultCh:
		if result.exitCode != 0 {
			t.Fatalf("feedback exit code = %d, want 0", result.exitCode)
		}
		if result.stdout != "https://github.com/nexu-io/looper/issues/321\n" {
			t.Fatalf("feedback stdout = %q, want issue URL", result.stdout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("feedback waited indefinitely for an inherited output pipe")
	}
	waitForCLIProcessExit(t, childPID)
}

func TestFeedbackRunAndCleanupErrorPreservesBothFailuresAndSummary(t *testing.T) {
	runErr := context.Canceled
	cleanupErr := errors.New("process group remained live")
	stdout := `__LOOPER_RESULT__={"summary":"issue creation was interrupted"}`

	err := feedbackRunAndCleanupError(runErr, cleanupErr, stdout, "")
	if !errors.Is(err, runErr) {
		t.Fatalf("error = %v, want run error", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want cleanup barrier error", err)
	}
	if !strings.Contains(err.Error(), "clean up feedback agent process group") {
		t.Fatalf("error = %q, want cleanup context", err)
	}
	if !strings.Contains(err.Error(), "issue creation was interrupted") {
		t.Fatalf("error = %q, want agent summary", err)
	}
}

func waitForCLIProcessPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatalf("parse process PID %q: %v", data, parseErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read process PID file: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process PID file %q was not created", path)
	return 0
}

func waitForCLIProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}
