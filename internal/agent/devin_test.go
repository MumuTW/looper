package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestResolveSpawnDevinUsesUnattendedPrintMode(t *testing.T) {
	t.Parallel()

	model := "swe-1-7"
	command, args := ResolveSpawn(ExecutorConfig{
		Vendor: config.AgentVendorDevinExperimental,
		Model:  &model,
	}, "/tmp/looper-worktree", "fix the issue")

	if command != "devin" {
		t.Fatalf("command = %q, want devin", command)
	}
	want := "--model swe-1-7 --permission-mode dangerous --respect-workspace-trust false --print fix the issue"
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestResolveSpawnDevinPreservesExplicitPermissionPolicy(t *testing.T) {
	t.Parallel()

	command, args := ResolveSpawn(ExecutorConfig{
		Vendor: config.AgentVendorDevinExperimental,
		Params: map[string]any{
			"args": []any{"--permission-mode", "accept-edits", "--respect-workspace-trust=true"},
		},
	}, "/tmp/looper-worktree", "inspect only")

	if command != "devin" {
		t.Fatalf("command = %q, want devin", command)
	}
	got := strings.Join(args, " ")
	if strings.Count(got, "--permission-mode") != 1 {
		t.Fatalf("args duplicate permission mode: %q", got)
	}
	if strings.Count(got, "--respect-workspace-trust") != 1 {
		t.Fatalf("args duplicate workspace trust: %q", got)
	}
	want := "--permission-mode accept-edits --respect-workspace-trust=true --print inspect only"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestDevinNativeResumeRemainsUnsupportedUntilSessionCaptureExists(t *testing.T) {
	t.Parallel()

	if nativeResumeSupported(config.AgentVendorDevinExperimental) {
		t.Fatal("devin native resume must remain disabled until Looper captures ATIF session identity")
	}
	if InteractiveTakeoverSupported(config.AgentVendorDevinExperimental) {
		t.Fatal("devin interactive takeover must remain disabled until resume continuity is verified")
	}

	_, args := ResolveSpawnWithNativeResume(
		ExecutorConfig{Vendor: config.AgentVendorDevinExperimental},
		"/tmp/looper-worktree",
		"continue",
		"session-123",
		true,
	)
	if got, want := strings.Join(args, " "), "--permission-mode dangerous --respect-workspace-trust false --print continue"; got != want {
		t.Fatalf("unsupported resume must use a fresh prompt: args = %q, want %q", got, want)
	}
}

func TestConfiguredExecutorRunsDevinFreshRetriesInTheSameWorktree(t *testing.T) {
	worktree := t.TempDir()
	capturePath := filepath.Join(t.TempDir(), "invocations")
	fakeDevin := filepath.Join(t.TempDir(), "devin")
	script := `#!/bin/sh
{
  printf 'cwd=<%s>' "$PWD"
  for arg in "$@"; do printf ' arg=<%s>' "$arg"; done
  printf '\n'
} >> "$CAPTURE_PATH"
printf '%s\n' '__LOOPER_RESULT__={"summary":"fake Devin completed"}'
`
	if err := os.WriteFile(fakeDevin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake devin: %v", err)
	}

	model := "glm-5-2"
	cfg := ExecutorConfig{
		Vendor: config.AgentVendorDevinExperimental,
		Model:  &model,
		Params: map[string]any{"command": fakeDevin},
		Env:    map[string]string{"CAPTURE_PATH": capturePath},
	}
	executor := New(withParamsOwner(cfg, config.AgentVendorDevinExperimental))

	for _, executionID := range []string{"devin-first", "devin-retry"} {
		run, err := executor.Start(context.Background(), RunInput{
			ExecutionID:      executionID,
			Prompt:           "finish the task",
			WorkingDirectory: worktree,
			Timeout:          5 * time.Second,
		})
		if err != nil {
			t.Fatalf("Start(%s) error = %v", executionID, err)
		}
		result, err := run.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait(%s) error = %v", executionID, err)
		}
		if result.Status != "completed" || result.ParseStatus != "parsed" || result.Summary != "fake Devin completed" {
			t.Fatalf("result(%s) = %#v, want parsed completion marker", executionID, result)
		}
	}

	raw, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read fake Devin invocations: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("invocations = %q, want two fresh runs", raw)
	}
	for _, line := range lines {
		for _, want := range []string{
			"cwd=<" + worktree + ">",
			"arg=<--model>",
			"arg=<glm-5-2>",
			"arg=<--permission-mode>",
			"arg=<dangerous>",
			"arg=<--respect-workspace-trust>",
			"arg=<false>",
			"arg=<--print>",
			"arg=<finish the task>",
		} {
			if !strings.Contains(line, want) {
				t.Fatalf("invocation = %q, missing %q", line, want)
			}
		}
		if strings.Contains(line, "--resume") || strings.Contains(line, "--continue") {
			t.Fatalf("fresh retry inherited ambient resume state: %q", line)
		}
	}
}

func TestConfiguredExecutorCancelsDevinProcessGroup(t *testing.T) {
	startedPath := filepath.Join(t.TempDir(), "started")
	fakeDevin := filepath.Join(t.TempDir(), "devin")
	// Publish the marker with a rename so os.Stat cannot observe a created but
	// empty file, and install the trap before announcing readiness so a Kill
	// racing the announcement is still handled.
	script := `#!/bin/sh
trap 'exit 0' TERM INT
printf 'started\n' > "$STARTED_PATH.tmp"
mv "$STARTED_PATH.tmp" "$STARTED_PATH"
while :; do sleep 1; done
`
	if err := os.WriteFile(fakeDevin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake devin: %v", err)
	}

	cfg := ExecutorConfig{
		Vendor: config.AgentVendorDevinExperimental,
		Params: map[string]any{"command": fakeDevin},
		Env:    map[string]string{"STARTED_PATH": startedPath},
	}
	run, err := New(withParamsOwner(cfg, config.AgentVendorDevinExperimental)).Start(context.Background(), RunInput{
		ExecutionID:      "devin-cancel",
		Prompt:           "long task",
		WorkingDirectory: t.TempDir(),
		Timeout:          10 * time.Second,
		GracefulShutdown: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Spawning a shell can take well over 2s on a loaded CI runner. The
	// deadline only has to bound a genuine hang, so give it the run's whole
	// budget rather than an arbitrarily tighter slice of it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake Devin did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := run.Kill("test cancellation"); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	result, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "killed" {
		t.Fatalf("result status = %q, want killed", result.Status)
	}
}
