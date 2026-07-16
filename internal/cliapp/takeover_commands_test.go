package cliapp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestPrepareTakeoverTerminalMakesChildForegroundAndRestoresParent(t *testing.T) {
	cmd := exec.Command("sh", "-c", "true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin := os.NewFile(17, "fake-tty")
	restoredPgrp := 0
	control := takeoverTerminalControl{
		foregroundProcessGroup: func(fd int) (int, bool, error) {
			if fd != 17 {
				t.Fatalf("foreground fd = %d, want 17", fd)
			}
			return 42, true, nil
		},
		setForegroundProcessGroup: func(fd, pgrp int) error {
			if fd != 17 {
				t.Fatalf("restore fd = %d, want 17", fd)
			}
			restoredPgrp = pgrp
			return nil
		},
	}

	restore, err := prepareTakeoverTerminal(cmd, stdin, control)
	if err != nil {
		t.Fatalf("prepareTakeoverTerminal() error = %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || !cmd.SysProcAttr.Foreground || cmd.SysProcAttr.Ctty != 17 {
		t.Fatalf("SysProcAttr = %#v, want owned foreground process group on fd 17", cmd.SysProcAttr)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore() error = %v", err)
	}
	if restoredPgrp != 42 {
		t.Fatalf("restored process group = %d, want 42", restoredPgrp)
	}
}

func TestResumeLoopPropagatesNonzeroAgentExit(t *testing.T) {
	server := newResumeLoopServer(t, "exit 7")
	defer server.Close()
	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})

	exitCode := app.Run(context.Background(), []string{"--config", configPath, "resume", "12"})
	if exitCode == 0 {
		t.Fatalf("resume exit code = 0, want non-zero; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "exit status 7") {
		t.Fatalf("resume stderr = %q, want child exit status", stderr.String())
	}
	if strings.Contains(stdout.String(), "Session detached") {
		t.Fatalf("resume stdout = %q, want no success message after failed child", stdout.String())
	}
}

func TestResumeLoopTreatsAgentSIGINTAsNormalDetach(t *testing.T) {
	for name, resumeCommand := range map[string]string{
		"shell signaled directly": "kill -INT $$",
		"shell waits on child":    "sh -c 'kill -INT $$'",
	} {
		t.Run(name, func(t *testing.T) {
			server := newResumeLoopServer(t, resumeCommand)
			defer server.Close()
			configPath := writeCLIConfig(t, server.URL, "")
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})

			exitCode := app.Run(context.Background(), []string{"--config", configPath, "resume", "12"})
			if exitCode != 0 {
				t.Fatalf("resume exit code = %d, want normal SIGINT detach; stderr=%q", exitCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Session detached") {
				t.Fatalf("resume stdout = %q, want detach instructions", stdout.String())
			}
		})
	}
}

func TestResumeLoopPropagatesShellLaunchFailure(t *testing.T) {
	server := newResumeLoopServer(t, "true")
	defer server.Close()
	configPath := writeEditableCLIConfigWithPayload(t, map[string]any{
		"server":        map[string]any{"baseUrl": server.URL, "authMode": "none"},
		"notifications": map[string]any{"osascript": map[string]any{"enabled": false}},
	})
	t.Setenv("PATH", t.TempDir())
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})

	exitCode := app.Run(context.Background(), []string{"--config", configPath, "resume", "12"})
	if exitCode == 0 {
		t.Fatalf("resume exit code = 0, want shell launch failure; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `executable file not found`) {
		t.Fatalf("resume stderr = %q, want shell launch error", stderr.String())
	}
	if strings.Contains(stdout.String(), "Session detached") {
		t.Fatalf("resume stdout = %q, want no success message after launch failure", stdout.String())
	}
}

func TestResumeLoopCancellationStopsAgentDescendants(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	scriptPath := filepath.Join(dir, "resume-agent.sh")
	script := strings.Join([]string{
		"#!/bin/sh",
		`/bin/sh -c 'trap "" TERM; echo $$ > "$1"; while :; do sleep 1; done' sh "` + pidPath + `" </dev/null >/dev/null 2>&1 &`,
		"wait",
	}, "\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write resume agent script: %v", err)
	}
	server := newResumeLoopServer(t, `"`+scriptPath+`"`)
	defer server.Close()
	configPath := writeCLIConfig(t, server.URL, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	app := New(Deps{Stdout: stdout, Stderr: stderr, HTTPClient: server.Client()})

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan int, 1)
	go func() {
		resultCh <- app.Run(ctx, []string{"--config", configPath, "resume", "12"})
	}()
	childPID := waitForCLIProcessPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	cancel()

	select {
	case exitCode := <-resultCh:
		if exitCode == 0 {
			t.Fatalf("resume exit code = 0 after cancellation, want non-zero; stdout=%q", stdout.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resume did not return after cancellation")
	}
	waitForCLIProcessExit(t, childPID)
}

func newResumeLoopServer(t *testing.T, resumeCommand string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/loops/12/takeover" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeEnvelope(t, w, pkgapi.Success("req_takeover", map[string]any{
			"loopId":        "loop_12",
			"vendor":        "codex",
			"sessionId":     "session_12",
			"worktreePath":  t.TempDir(),
			"supported":     true,
			"resumeCommand": resumeCommand,
		}))
	}))
}
