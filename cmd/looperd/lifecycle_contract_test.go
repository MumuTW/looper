package main

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

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
)

func TestTakeoverWaitsForTermResistantLiveProcessGroupReap(t *testing.T) {
	ctx := context.Background()
	services, repos, now := newStopAllTestServices(t)
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{
		loopID: "loop_live_takeover", seq: 1, loopType: "worker", loopStatus: "running",
		runID: "run_live_takeover", runStatus: "running", executionID: "exec_live_takeover", executionStatus: "running",
	})

	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "agent.pid")
	script := filepath.Join(workDir, "term-resistant-agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\necho $$ > \"$PID_FILE\"\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatalf("write agent script: %v", err)
	}
	executor := agent.New(agent.ExecutorOptions{Config: agent.ExecutorConfig{
		Vendor: config.AgentVendor("lifecycle-test"),
		Params: map[string]any{"command": script},
	}})
	execution, err := executor.Start(ctx, agent.RunInput{
		ExecutionID: "exec_live_takeover", LoopID: "loop_live_takeover", RunID: "run_live_takeover",
		Prompt: "hold worktree", WorkingDirectory: workDir, Timeout: 5 * time.Second,
		GracefulShutdown: 120 * time.Millisecond, Env: map[string]string{"PID_FILE": pidFile},
	})
	if err != nil {
		t.Fatalf("executor.Start() error = %v", err)
	}
	registry := looperdruntime.NewActiveExecutionRegistry()
	registry.Register("loop_live_takeover", "run_live_takeover", "exec_live_takeover", execution)
	services.ActiveExecutions = registry

	pid := waitForProcessPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	returned := make(chan error, 1)
	go func() {
		_, err := takeoverLoop(ctx, services, "loop_live_takeover", "live takeover", time.Now, syscall.Kill, nil)
		returned <- err
	}()

	select {
	case err := <-returned:
		t.Fatalf("takeover returned before TERM grace/reap: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	loop, err := repos.Loops.GetByID(ctx, "loop_live_takeover")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "running" {
		t.Fatalf("loop during TERM grace = %#v, want running until process reap", loop)
	}

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("takeoverLoop() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("takeover did not finish after executor SIGKILL escalation")
	}
	if processGroupExists(pid) {
		t.Fatalf("process group %d still exists after takeover completed", pid)
	}
	loop, err = repos.Loops.GetByID(ctx, "loop_live_takeover")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "human_takeover" {
		t.Fatalf("loop after reap = %#v, want human_takeover", loop)
	}
}

func TestCloseWaitsForTermResistantLiveProcessGroupReap(t *testing.T) {
	ctx := context.Background()
	services, repos, now := newStopAllTestServices(t)
	insertStopAllTestLoop(t, ctx, repos, now, stopAllLoopFixture{
		loopID: "loop_live_close", seq: 2, loopType: "worker", loopStatus: "running",
		runID: "run_live_close", runStatus: "running", executionID: "exec_live_close", executionStatus: "running",
	})

	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "agent.pid")
	script := filepath.Join(workDir, "term-resistant-agent.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\necho $$ > \"$PID_FILE\"\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatalf("write agent script: %v", err)
	}
	executor := agent.New(agent.ExecutorOptions{Config: agent.ExecutorConfig{
		Vendor: config.AgentVendor("lifecycle-test"),
		Params: map[string]any{"command": script},
	}})
	execution, err := executor.Start(ctx, agent.RunInput{
		ExecutionID: "exec_live_close", LoopID: "loop_live_close", RunID: "run_live_close",
		Prompt: "hold worktree", WorkingDirectory: workDir, Timeout: 5 * time.Second,
		GracefulShutdown: 120 * time.Millisecond, Env: map[string]string{"PID_FILE": pidFile},
	})
	if err != nil {
		t.Fatalf("executor.Start() error = %v", err)
	}
	registry := looperdruntime.NewActiveExecutionRegistry()
	registry.Register("loop_live_close", "run_live_close", "exec_live_close", execution)
	services.ActiveExecutions = registry

	pid := waitForProcessPIDFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	returned := make(chan error, 1)
	go func() {
		_, err := closeLoop(ctx, services, "loop_live_close", "live close", time.Now, nil, nil)
		returned <- err
	}()

	select {
	case err := <-returned:
		t.Fatalf("close returned before TERM grace/reap: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	loop, err := repos.Loops.GetByID(ctx, "loop_live_close")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "running" {
		t.Fatalf("loop during TERM grace = %#v, want running until process reap", loop)
	}

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("closeLoop() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after executor SIGKILL escalation")
	}
	if processGroupExists(pid) {
		t.Fatalf("process group %d still exists after close completed", pid)
	}
	loop, err = repos.Loops.GetByID(ctx, "loop_live_close")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "terminated" {
		t.Fatalf("loop after reap = %#v, want terminated", loop)
	}
}

func waitForProcessPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process pid file %s was not written", path)
	return 0
}

func processGroupExists(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
