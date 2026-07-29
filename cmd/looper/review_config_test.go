package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

func TestLoadReviewSubmitConfigIgnoresChildPrecedenceLayers(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	capturedGHPath := filepath.Join(t.TempDir(), "daemon-cli-gh")
	cfg.Tools.GHPath = &capturedGHPath
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(config snapshot) error = %v", err)
	}

	// In a real proxy child the conflicting LOOPER_GH_PATH may originate in
	// daemon ambient env or captured Agent.Env; neither may re-apply over the
	// daemon's already materialized CLI winner. Provider credentials remain
	// ordinary child env because the selected transport still needs them.
	// The CLI layer needs no coverage here: review submit accepts no
	// config-selecting flags and forwards no argv to the loader, so there is no
	// argument that could reach this precedence chain in the first place.
	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	t.Setenv("LOOPER_GH_PATH", filepath.Join(t.TempDir(), "agent-env-gh"))
	t.Setenv("FORGEJO_TOKEN", "provider-credential")
	installTrustedReviewConfigFD(t, raw)

	loaded, err := loadReviewSubmitConfig()
	if err != nil {
		t.Fatalf("loadReviewSubmitConfig() error = %v", err)
	}
	if loaded.Tools.GHPath == nil || *loaded.Tools.GHPath != capturedGHPath {
		t.Fatalf("loadReviewSubmitConfig().Tools.GHPath = %v, want captured daemon CLI winner %q", loaded.Tools.GHPath, capturedGHPath)
	}
	if got := os.Getenv("FORGEJO_TOKEN"); got != "provider-credential" {
		t.Fatalf("FORGEJO_TOKEN after loadReviewSubmitConfig() = %q, want provider credential preserved", got)
	}
}

func TestLoadReviewSubmitConfigClearsFDAndFailsLoudOnSecondLoad(t *testing.T) {
	// Skip-only fix: no memoization. First load consumes the one-shot FD;
	// a second load must fail loud so double consumers cannot mask as success.
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	capturedGHPath := filepath.Join(t.TempDir(), "daemon-cli-gh")
	cfg.Tools.GHPath = &capturedGHPath
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(config snapshot) error = %v", err)
	}

	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	installTrustedReviewConfigFD(t, raw)
	first, err := loadReviewSubmitConfig()
	if err != nil {
		t.Fatalf("first loadReviewSubmitConfig() error = %v", err)
	}
	if first.Tools.GHPath == nil || *first.Tools.GHPath != capturedGHPath {
		t.Fatalf("first loadReviewSubmitConfig().Tools.GHPath = %v, want %q", first.Tools.GHPath, capturedGHPath)
	}
	if os.Getenv(forge.TrustedReviewConfigFDEnv) != "" {
		t.Fatalf("TrustedReviewConfigFDEnv still set after first load; want cleared")
	}
	_, secondErr := loadReviewSubmitConfig()
	if secondErr == nil {
		t.Fatal("second loadReviewSubmitConfig() error = nil, want fail-loud after one-shot FD consumed")
	}
	if !strings.Contains(strings.ToLower(secondErr.Error()), "trusted review config descriptor") {
		t.Fatalf("second loadReviewSubmitConfig() = %v, want missing-descriptor failure", secondErr)
	}
}

// A trusted child gets exactly one config descriptor, so review submit must be
// the only thing in the process that loads config. Anything else on the path —
// a second load, an ambient startup step — closes the FD and turns the run into
// an EBADF that looks like a forge failure.
func TestRunTrustedReviewChildReadsSnapshotWithoutEBADF(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	sentinelGH := filepath.Join(t.TempDir(), "snapshot-authority-gh")
	cfg.Tools.GHPath = &sentinelGH
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal(config snapshot) error = %v", err)
	}

	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	// Ambient override must not win over the snapshot.
	t.Setenv("LOOPER_GH_PATH", filepath.Join(t.TempDir(), "must-not-win"))
	installTrustedReviewConfigFD(t, raw)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"review", "submit", "acme/looper#42",
		"--event", "COMMENT",
		"--commit-id", "0000000000000000000000000000000000000000",
	}, bytes.NewBufferString(`{"body":"diagnostic","comments":[]}`), &stdout, &stderr)
	if code == 0 {
		t.Fatal("run() exit = 0, want non-zero after config load (no real gh/PR)")
	}
	combined := strings.ToLower(stdout.String() + "\n" + stderr.String())
	if strings.Contains(combined, "bad file descriptor") {
		t.Fatalf("run() output contains EBADF:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	// Snapshot must have been readable; failure should be downstream of config
	// (gateway/PR/auth), not trusted-review-config FD transport.
	if strings.Contains(combined, "trusted review config") && strings.Contains(combined, "descriptor") {
		t.Fatalf("run() hit trusted config descriptor failure:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
}

func TestRunReviewCapabilityAnswersTheDaemonProbe(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"review", "capability"}, bytes.NewBufferString(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(review capability) = %d, want 0; the daemon reads a non-zero exit as no review support", code)
	}
	if stdout.String() != forge.TrustedReviewCapabilityToken+"\n" {
		t.Fatalf("stdout = %q, want %q", stdout.String(), forge.TrustedReviewCapabilityToken+"\n")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestParseReviewSubmitFlagsAcceptsTrustedProxyArgv(t *testing.T) {
	// Exactly what applyTrustedReviewProxyPolicy appends after the prompt's own
	// flags, in both spellings the proxy and the prompt can produce.
	parsed, err := parseReviewSubmitFlags([]string{
		"acme/looper#42",
		"--event", "COMMENT",
		"--clean-review-event=COMMENT",
		"--blocking-review-event", "REQUEST_CHANGES",
		"--commit-id", "head-42",
		"--reviewer-manual",
		"--reviewer-run-id", "run-7",
	})
	if err != nil {
		t.Fatalf("parseReviewSubmitFlags() error = %v", err)
	}
	want := reviewSubmitFlags{
		ref: "acme/looper#42", event: "COMMENT", commitID: "head-42",
		cleanReviewEvent: "COMMENT", blockingReviewEvent: "REQUEST_CHANGES",
		reviewerRunID: "run-7", reviewerManual: true,
	}
	if parsed != want {
		t.Fatalf("parseReviewSubmitFlags() = %+v, want %+v", parsed, want)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "no target", args: []string{"--event", "COMMENT"}},
		{name: "two targets", args: []string{"acme/looper#42", "acme/looper#43"}},
		{name: "missing value", args: []string{"acme/looper#42", "--event"}},
		{name: "unknown flag", args: []string{"acme/looper#42", "--config", "/etc/looper.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseReviewSubmitFlags(tc.args); err == nil {
				t.Fatalf("parseReviewSubmitFlags(%q) error = nil, want rejection", tc.args)
			}
		})
	}
}

// installTrustedReviewConfigFD writes snapshot bytes into a pipe and exposes a
// duplicated read FD via TrustedReviewConfigFDEnv. The loader owns and closes
// that duplicate exclusively; the original pipe ends are closed by cleanup so
// finalizers cannot double-close a reused descriptor number.
func installTrustedReviewConfigFD(t *testing.T, snapshot []byte) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	writeDone := make(chan error, 1)
	go func() {
		var writeErr error
		if len(snapshot) > 0 {
			_, writeErr = writer.Write(snapshot)
		}
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeDone <- writeErr
	}()

	dupFD, err := syscall.Dup(int(reader.Fd()))
	if err != nil {
		_ = reader.Close()
		_ = writer.Close()
		t.Fatalf("Dup(config reader) error = %v", err)
	}
	if err := reader.Close(); err != nil {
		_ = syscall.Close(dupFD)
		t.Fatalf("Close(original reader) error = %v", err)
	}
	t.Setenv(forge.TrustedReviewConfigFDEnv, strconv.Itoa(dupFD))
	// Loader owns dupFD exclusively and closes it. Do not close here: after the
	// loader closes, the number may be reused and a second close would be unsafe.
	t.Cleanup(func() {
		if err := <-writeDone; err != nil {
			t.Errorf("write config snapshot error = %v", err)
		}
	})
}
