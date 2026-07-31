package main

import (
	"context"
	"io"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/forge"
)

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
	dir := t.TempDir()
	snapshot, err := config.DefaultConfig(dir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	policy := forge.TrustedReviewProxyPolicy{
		Clean:            "COMMENT",
		Blocking:         "COMMENT",
		ExpectedCommitID: "daemon-bound-head",
	}
	called := make(chan forge.TrustedReviewSubmission, 1)
	submitter := func(_ context.Context, submission forge.TrustedReviewSubmission, _, _ io.Writer) error {
		called <- submission
		return nil
	}
	sockPath, cleanup, err := forge.StartTrustedReviewProxy(submitter, nil, "acme/looper#42", dir, snapshot, policy)
	if err != nil {
		t.Fatalf("StartTrustedReviewProxy() error = %v", err)
	}
	t.Cleanup(cleanup)

	t.Setenv(forge.TrustedReviewSockEnv, sockPath)
	if err := forge.ProxyReviewSubmit(agentReviewSubmitArgv, []byte(`{"body":"x"}`), dir); err != nil {
		t.Fatalf("ProxyReviewSubmit() error = %v", err)
	}
	submission := <-called
	if submission.PRRef != "acme/looper#42" || submission.CommitID != "daemon-bound-head" {
		t.Fatalf("submission = %+v, want daemon-bound PR and head", submission)
	}
	if submission.CWD != dir {
		t.Fatalf("submission.CWD = %q, want %q", submission.CWD, dir)
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
