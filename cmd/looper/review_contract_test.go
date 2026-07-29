package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

// The reviewer runner tells agents to publish through a daemon-written wrapper
// that ends in `looper review submit`, and the daemon's trusted proxy rewrites
// that argv before spawning this binary. Nothing in this repository forced the
// two ends to agree, which is how the verb came to be deleted while the daemon
// kept emitting it.
//
// This runs the real binary behind the real proxy: the argv the runner prompts
// for goes in, and the assertion is that the child got far enough to need a
// forge. Anything earlier — an unknown command, an unknown flag, a rejected
// config snapshot — means the contract is broken again.

// agentReviewSubmitArgv is the argv the reviewer prompt tells agents to run.
// It is spelled out rather than derived so a change to the prompt has to be
// mirrored here deliberately. See internal/reviewer/runner.go.
var agentReviewSubmitArgv = []string{
	"review", "submit", "acme/looper#42",
	"--event", "COMMENT",
	"--commit-id", "agent-head",
	"--clean-review-event", "COMMENT",
	"--blocking-review-event", "COMMENT",
}

func TestProxyArgvReachesReviewSubmit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the looper binary")
	}

	dir := t.TempDir()
	binary := buildLooperBinary(t, dir)
	snapshot := trustedReviewSnapshotConfig(t, dir)

	policy := forge.TrustedReviewProxyPolicy{
		Clean:            "COMMENT",
		Blocking:         "COMMENT",
		ExpectedCommitID: "daemon-bound-head",
	}
	sockPath, cleanup, err := forge.StartTrustedReviewProxy(binary, nil, "acme/looper#42", dir, snapshot, policy, nil)
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)

	t.Setenv(forge.TrustedReviewSockEnv, sockPath)
	stderr := captureStderr(t, func() {
		err = forge.ProxyReviewSubmit(agentReviewSubmitArgv, []byte(`{"body":"x"}`), dir)
	})

	// The child cannot publish: the snapshot has no gh binary and no Forgejo
	// provider, so it stops at gateway construction. That it got there at all is
	// the contract — argv routed, flags parsed, snapshot loaded and validated,
	// and the daemon-bound review-event policy accepted.
	if err == nil {
		t.Fatal("review submit succeeded without a forge; the test fixture is wrong, not the CLI")
	}
	if !strings.Contains(stderr, "GitHub CLI (gh) not found") {
		t.Fatalf("proxy child stderr = %q, want it to fail at gateway construction (error was %v)", stderr, err)
	}
	for _, broken := range []string{"unknown command", "unknown flag", "review command is"} {
		if strings.Contains(stderr, broken) {
			t.Fatalf("proxy child rejected the daemon's own argv: %s", stderr)
		}
	}
}

// TestProxyArgvWithReviewerRunFlagsIsAccepted covers the manual-reviewer shape,
// where the proxy appends a bare --reviewer-manual followed by
// --reviewer-run-id. A parser that expected a value after the boolean would
// swallow the next flag and change what the daemon bound.
func TestProxyArgvWithReviewerRunFlagsIsAccepted(t *testing.T) {
	argv := append(append([]string(nil), agentReviewSubmitArgv...), "--reviewer-manual", "--reviewer-run-id", "run_agent")

	opts, err := parseReviewSubmit(argv)
	if err != nil {
		t.Fatalf("parseReviewSubmit(%v) error = %v", argv, err)
	}
	if !opts.ReviewerManual {
		t.Error("ReviewerManual = false, want true")
	}
	if opts.ReviewerRunID != "run_agent" {
		t.Errorf("ReviewerRunID = %q, want run_agent", opts.ReviewerRunID)
	}
	if opts.PRRef != "acme/looper#42" {
		t.Errorf("PRRef = %q, want acme/looper#42", opts.PRRef)
	}
	if opts.CommitID != "agent-head" {
		t.Errorf("CommitID = %q, want agent-head", opts.CommitID)
	}
}

// TestParseReviewSubmitRejectsUnknownFlags keeps the parser strict. The proxy's
// argv validator skips tokens it does not recognise, so a flag this parser
// silently ignored would be one that passed a security check and then did
// nothing.
func TestParseReviewSubmitRejectsUnknownFlags(t *testing.T) {
	tests := map[string][]string{
		"unknown flag":                {"review", "submit", "acme/looper#42", "--force"},
		"missing value":               {"review", "submit", "acme/looper#42", "--event"},
		"second positional":           {"review", "submit", "acme/looper#42", "other/repo#1"},
		"missing pull request target": {"review", "submit", "--event", "COMMENT"},
		"unsupported subcommand":      {"review", "repair", "acme/looper#42"},
		"boolean with a value":        {"review", "submit", "acme/looper#42", "--reviewer-manual=true"},
	}
	for name, argv := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseReviewSubmit(argv); err == nil {
				t.Fatalf("parseReviewSubmit(%v) = nil error, want a refusal", argv)
			}
		})
	}
}

// TestParseReviewSubmitAcceptsInlineValues covers the `--flag=value` spelling,
// which the proxy preserves for flags it does not rewrite.
func TestParseReviewSubmitAcceptsInlineValues(t *testing.T) {
	opts, err := parseReviewSubmit([]string{"review", "submit", "acme/looper#42", "--event=APPROVE", "--commit-id=abc123"})
	if err != nil {
		t.Fatalf("parseReviewSubmit() error = %v", err)
	}
	if opts.Event != "APPROVE" || opts.CommitID != "abc123" {
		t.Fatalf("opts = %+v, want event APPROVE and commit abc123", opts)
	}
}

func buildLooperBinary(t *testing.T, dir string) string {
	t.Helper()

	binary := filepath.Join(dir, "looper")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/looper: %v\n%s", err, out)
	}
	return binary
}

// trustedReviewSnapshotConfig is the config the daemon would materialize for a
// run, minus any way to reach a forge: no gh path and no provider, so the child
// stops at the first step that needs one.
func trustedReviewSnapshotConfig(t *testing.T, dir string) config.Config {
	t.Helper()

	cfg, err := config.DefaultConfig(dir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Tools.GHPath = nil
	cfg.Projects = nil
	cfg.Providers = nil
	cfg.Network = config.NetworkConfig{}
	cfg.Storage.DBPath = filepath.Join(dir, "state", "looper.sqlite")
	return cfg
}

// captureStderr collects what ProxyReviewSubmit relays from the child, which it
// writes to the process's own stderr rather than returning.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	original := os.Stderr
	os.Stderr = write

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(read)
		done <- buf.String()
	}()

	fn()

	os.Stderr = original
	_ = write.Close()
	captured := <-done
	_ = read.Close()
	return captured
}
