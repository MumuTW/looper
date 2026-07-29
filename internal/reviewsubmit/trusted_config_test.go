package reviewsubmit

import (
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

func TestTrustedReviewConfigIgnoresChildPrecedenceLayers(t *testing.T) {
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
	t.Setenv("LOOPER_TRUSTED_REVIEW_PROXY_CHILD", "1")
	t.Setenv("LOOPER_GH_PATH", filepath.Join(t.TempDir(), "agent-env-gh"))
	t.Setenv("FORGEJO_TOKEN", "provider-credential")
	installTrustedReviewConfigFD(t, raw)
	// The child's own CLI args must lose to the snapshot: the daemon already
	// resolved precedence, and re-applying it here is how an agent-controlled
	// environment would get a say in what its review is judged against.
	opts := Options{ConfigArgs: []string{"--gh-path", filepath.Join(t.TempDir(), "child-cli-gh")}}
	loaded, err := loadConfig(opts)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if loaded.Config.Tools.GHPath == nil || *loaded.Config.Tools.GHPath != capturedGHPath {
		t.Fatalf("loadConfig().Tools.GHPath = %v, want captured daemon CLI winner %q", loaded.Config.Tools.GHPath, capturedGHPath)
	}
	if got := os.Getenv("FORGEJO_TOKEN"); got != "provider-credential" {
		t.Fatalf("FORGEJO_TOKEN after loadConfig() = %q, want provider credential preserved", got)
	}
}

func TestTrustedReviewConfigClearsFDAndFailsLoudOnSecondLoad(t *testing.T) {
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
	first, err := loadConfig(Options{})
	if err != nil {
		t.Fatalf("first loadConfig() error = %v", err)
	}
	if first.Config.Tools.GHPath == nil || *first.Config.Tools.GHPath != capturedGHPath {
		t.Fatalf("first loadConfig().Tools.GHPath = %v, want %q", first.Config.Tools.GHPath, capturedGHPath)
	}
	if os.Getenv(forge.TrustedReviewConfigFDEnv) != "" {
		t.Fatalf("TrustedReviewConfigFDEnv still set after first load; want cleared")
	}
	_, secondErr := loadConfig(Options{})
	if secondErr == nil {
		t.Fatal("second loadConfig() error = nil, want fail-loud after one-shot FD consumed")
	}
	if !strings.Contains(strings.ToLower(secondErr.Error()), "trusted review config descriptor") {
		t.Fatalf("second loadConfig() = %v, want missing-descriptor failure", secondErr)
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
