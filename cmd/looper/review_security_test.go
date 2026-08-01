package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/forge"
)

// The trusted socket is the only agent-visible authority for review publish.
// Removing it must not turn the same machine-only command into a direct gh
// client using agent-visible config or ambient credentials (#184).
func TestReviewSubmitWithoutTrustedProxyFailsBeforeGitHub(t *testing.T) {
	assertReviewSubmitWithoutTrustedProxyFailsBeforeGitHub(t, false)
}

func TestReviewSubmitRejectsForgedLegacyChildEnvironment(t *testing.T) {
	assertReviewSubmitWithoutTrustedProxyFailsBeforeGitHub(t, true)
}

func assertReviewSubmitWithoutTrustedProxyFailsBeforeGitHub(t *testing.T, forgeLegacyChild bool) {
	t.Helper()
	t.Setenv(forge.TrustedReviewSockEnv, "")
	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "")
	t.Setenv("LOOPER_TRUSTED_REVIEW_CONFIG_FD", "")
	if forgeLegacyChild {
		t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
		t.Setenv("LOOPER_TRUSTED_REVIEW_CONFIG_FD", "3")
	}

	root := t.TempDir()
	invoked := filepath.Join(root, "gh-invoked")
	ghPath := filepath.Join(root, "gh")
	if err := os.WriteFile(ghPath, []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\nexit 1\n", invoked)), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	configPath := filepath.Join(root, "config.json")
	configJSON := fmt.Sprintf(`{
  "tools": {"ghPath": %q, "gitPath": "git"},
  "storage": {"dbPath": %q},
  "roles": {"reviewer": {"behavior": {"reviewEvents": {"clean": "COMMENT", "blocking": "COMMENT"}}}}
}`, ghPath, filepath.Join(root, "looper.sqlite"))
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := runReview(context.Background(), []string{
		"review", "submit", "acme/looper#42",
		"--event", "COMMENT",
		"--commit-id", "abc123",
		"--config", configPath,
	}, strings.NewReader(`{"body":"review"}`), stdout, stderr)

	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "trusted review proxy is required") {
		t.Fatalf("stderr = %q, want trusted-proxy refusal", stderr.String())
	}
	if _, err := os.Stat(invoked); !os.IsNotExist(err) {
		t.Fatalf("agent-visible gh was invoked without proxy authority: %v", err)
	}
}
