package forge

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
)

func TestValidateTrustedReviewProxyArgv(t *testing.T) {
	t.Parallel()
	const allowed = "acme/looper#1"
	tests := []struct {
		name    string
		argv    []string
		allowed string
		wantErr bool
	}{
		{name: "submit", argv: []string{"review", "submit", allowed, "--event", "COMMENT"}, allowed: allowed},
		{name: "case insensitive", argv: []string{"review", "submit", "Acme/Looper#1"}, allowed: allowed},
		{name: "flags before target", argv: []string{"review", "submit", "--event", "COMMENT", allowed}, allowed: allowed},
		{name: "other pr", argv: []string{"review", "submit", "acme/looper#99"}, allowed: allowed, wantErr: true},
		{name: "other repo", argv: []string{"review", "submit", "evil/other#1"}, allowed: allowed, wantErr: true},
		{name: "missing target", argv: []string{"review", "submit", "--event", "COMMENT"}, allowed: allowed, wantErr: true},
		{name: "config override", argv: []string{"review", "submit", allowed, "--config", "/tmp/evil"}, allowed: allowed, wantErr: true},
		{name: "policy override", argv: []string{"--roles-reviewer-behavior-review-events-clean", "APPROVE", "review", "submit", allowed}, allowed: allowed, wantErr: true},
		{name: "other command", argv: []string{"status"}, allowed: allowed, wantErr: true},
		{name: "empty allowed", argv: []string{"review", "submit", allowed}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTrustedReviewProxyArgv(test.argv, test.allowed)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateTrustedReviewProxyArgv(%v, %q) error = %v, wantErr %t", test.argv, test.allowed, err, test.wantErr)
			}
		})
	}
}

func TestTrustedReviewProxyBindsSubmissionAuthority(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.DefaultConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Env = map[string]string{"AGENT_SECRET": "hidden"}
	cfg.Daemon.Environment = map[string]string{"DAEMON_SECRET": "hidden"}
	policy := TrustedReviewProxyPolicy{Clean: "APPROVE", Blocking: "REQUEST_CHANGES", ExpectedCommitID: "bound-head", ReviewerManual: true, ReviewerRunID: "run_bound"}
	var got TrustedReviewSubmission
	submitter := func(_ context.Context, input TrustedReviewSubmission, stdout, _ io.Writer) error {
		got = input
		_, _ = io.WriteString(stdout, "submitted")
		return nil
	}
	sock, cleanup, err := StartTrustedReviewProxy(submitter, map[string]string{"GH_TOKEN": "secret"}, "acme/looper#1", root, cfg, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv(TrustedReviewSockEnv, sock)
	if err := ProxyReviewSubmit([]string{
		"review", "submit", "acme/looper#1", "--event", "COMMENT",
		"--clean-review-event", "COMMENT", "--commit-id", "agent-head", "--reviewer-run-id", "run_agent",
	}, []byte(`{"body":"review"}`), filepath.Join(root, "agent-cwd")); err != nil {
		t.Fatal(err)
	}
	if got.PRRef != "acme/looper#1" || got.Event != "COMMENT" || got.CommitID != "bound-head" || got.CleanReviewEvent != "APPROVE" || got.BlockingReviewEvent != "REQUEST_CHANGES" || !got.ReviewerManual || got.ReviewerRunID != "run_bound" {
		t.Fatalf("bound submission = %#v", got)
	}
	if got.CWD != root || string(got.Stdin) != `{"body":"review"}` || got.CredentialEnv["GH_TOKEN"] != "secret" {
		t.Fatalf("bound data = %#v", got)
	}
	if len(got.Config.Agent.Env) != 0 || len(got.Config.Daemon.Environment) != 0 {
		t.Fatalf("bound config retained process secrets: %#v", got.Config)
	}
	if cfg.Agent.Env["AGENT_SECRET"] != "hidden" || cfg.Daemon.Environment["DAEMON_SECRET"] != "hidden" {
		t.Fatalf("proxy sanitization mutated source config: %#v", cfg)
	}
}

func TestTrustedReviewProxyRejectsBeforeSubmitter(t *testing.T) {
	called := false
	submitter := func(context.Context, TrustedReviewSubmission, io.Writer, io.Writer) error { called = true; return nil }
	sock, cleanup, err := StartTrustedReviewProxy(submitter, nil, "acme/looper#1", t.TempDir(), config.Config{}, testTrustedReviewPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv(TrustedReviewSockEnv, sock)
	err = ProxyReviewSubmit([]string{"review", "submit", "acme/looper#2", "--event", "COMMENT"}, nil, "/ignored")
	if err == nil || called {
		t.Fatalf("unbound request error = %v, submitter called = %t", err, called)
	}
}

func TestStartTrustedReviewProxyValidatesAuthority(t *testing.T) {
	submitter := TrustedReviewSubmitter(func(context.Context, TrustedReviewSubmission, io.Writer, io.Writer) error { return nil })
	dir := t.TempDir()
	policy := testTrustedReviewPolicy()
	tests := []struct {
		name      string
		submitter TrustedReviewSubmitter
		pr        string
		cwd       string
		policy    TrustedReviewProxyPolicy
	}{
		{name: "missing submitter", pr: "acme/looper#1", cwd: dir, policy: policy},
		{name: "missing pr", submitter: submitter, cwd: dir, policy: policy},
		{name: "invalid pr", submitter: submitter, pr: "bad", cwd: dir, policy: policy},
		{name: "missing cwd", submitter: submitter, pr: "acme/looper#1", policy: policy},
		{name: "relative cwd", submitter: submitter, pr: "acme/looper#1", cwd: "relative", policy: policy},
		{name: "invalid policy", submitter: submitter, pr: "acme/looper#1", cwd: dir},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := StartTrustedReviewProxy(test.submitter, nil, test.pr, test.cwd, config.Config{}, test.policy); err == nil {
				t.Fatal("StartTrustedReviewProxy() error = nil")
			}
		})
	}
}

func TestTrustedReviewSockConfigured(t *testing.T) {
	t.Setenv(TrustedReviewSockEnv, "")
	if TrustedReviewSockConfigured() {
		t.Fatal("TrustedReviewSockConfigured() = true when unset")
	}
	t.Setenv(TrustedReviewSockEnv, "/tmp/sock")
	if !TrustedReviewSockConfigured() {
		t.Fatal("TrustedReviewSockConfigured() = false when set")
	}
}

func TestTrustedReviewProxyLimitsAndCleanup(t *testing.T) {
	started := make(chan struct{})
	submitter := func(ctx context.Context, _ TrustedReviewSubmission, _, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	sock, cleanup, err := StartTrustedReviewProxy(submitter, nil, "acme/looper#1", t.TempDir(), config.Config{}, testTrustedReviewPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(TrustedReviewSockEnv, sock)
	if err := ProxyReviewSubmit([]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, make([]byte, maxTrustedReviewProxyStdinBytes+1), "/ignored"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized error = %v", err)
	}

	activeDone := make(chan error, 1)
	go func() {
		activeDone <- ProxyReviewSubmit([]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, nil, "/ignored")
	}()
	<-started
	partial, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = partial.Write([]byte("{"))
	done := make(chan struct{})
	go func() { cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup blocked on submitter or partial connection")
	}
	_ = partial.Close()
	<-activeDone
}

func TestTrustedReviewBoundedBufferAndTruncation(t *testing.T) {
	buffer := newTrustedReviewBoundedBuffer(4)
	written, err := buffer.Write([]byte("abcdef"))
	if err != nil || written != 6 || buffer.String() != "abcd" || !buffer.Truncated() {
		t.Fatalf("buffer = (%d, %v, %q, %t)", written, err, buffer.String(), buffer.Truncated())
	}
	existing := "submission failed"
	if got := mergeTrustedReviewTruncationError(existing); !strings.Contains(got, existing) || !strings.Contains(got, trustedReviewOutputTruncatedMsg) {
		t.Fatalf("merged truncation error = %q", got)
	}
}

func TestTrustedReviewBoundedBufferConcurrentWriteAndRead(t *testing.T) {
	buffer := newTrustedReviewBoundedBuffer(1 << 20)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = buffer.Write([]byte("chunk\n"))
				}
			}
		}()
	}
	deadline := time.Now().Add(25 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = buffer.String()
	}
	close(stop)
	wg.Wait()
}

func testTrustedReviewPolicy() TrustedReviewProxyPolicy {
	return TrustedReviewProxyPolicy{Clean: "COMMENT", Blocking: "COMMENT", ExpectedCommitID: "abc123"}
}

func TestTrustedReviewSubmitterErrorsAreReturned(t *testing.T) {
	want := errors.New("submit failed")
	submitter := func(context.Context, TrustedReviewSubmission, io.Writer, io.Writer) error { return want }
	sock, cleanup, err := StartTrustedReviewProxy(submitter, nil, "acme/looper#1", t.TempDir(), config.Config{}, testTrustedReviewPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	t.Setenv(TrustedReviewSockEnv, sock)
	if err := ProxyReviewSubmit([]string{"review", "submit", "acme/looper#1", "--event", "COMMENT"}, nil, "/ignored"); err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("ProxyReviewSubmit() error = %v", err)
	}
}
