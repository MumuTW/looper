package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

// The fakes answer with the shared contract token rather than a literal, so a
// change to the token cannot leave these scripts passing against a probe that
// now expects something else.
var trustedReviewCapableScript = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"review\" ] && [ \"$2\" = \"capability\" ]; then\n  echo %s\n  exit 0\nfi\nexit 1\n", forge.TrustedReviewCapabilityToken)

const trustedReviewIncapableScript = "#!/bin/sh\necho 'unknown command \"review\" for \"looper\"' >&2\nexit 1\n"

const trustedReviewWrongTokenScript = "#!/bin/sh\necho other-tool/9\nexit 0\n"

func writeFakeLooper(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "looper")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}

func trustedLooperConfig(path string) config.Config {
	value := path
	cfg := config.Config{}
	cfg.Tools.LooperPath = &value
	return cfg
}

func (l *capturingSchedulerLogger) countMessage(message string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, entry := range l.entries {
		if entry.message == message {
			count++
		}
	}
	return count
}

func (l *capturingSchedulerLogger) reasonFor(t *testing.T, message string) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.message == message {
			reason, _ := entry.context["reason"].(string)
			return reason
		}
	}
	t.Fatalf("logger entries = %#v, want %q", l.entries, message)
	return ""
}

func TestResolveTrustedLooperCLIPathAcceptsCapableBinary(t *testing.T) {
	looperPath := writeFakeLooper(t, trustedReviewCapableScript)
	logger := &capturingSchedulerLogger{}

	got := resolveTrustedLooperCLIPath(trustedLooperConfig(looperPath), logger)
	if got != looperPath {
		t.Fatalf("resolveTrustedLooperCLIPath() = %q, want %q", got, looperPath)
	}
	logger.requireMessage(t, "trusted looper review-submit wrapper verified")
}

func TestResolveTrustedLooperCLIPathRejectsBinaryWithoutReviewSubmit(t *testing.T) {
	looperPath := writeFakeLooper(t, trustedReviewIncapableScript)
	logger := &capturingSchedulerLogger{}

	if got := resolveTrustedLooperCLIPath(trustedLooperConfig(looperPath), logger); got != "" {
		t.Fatalf("resolveTrustedLooperCLIPath() = %q, want \"\"", got)
	}
	reason := logger.reasonFor(t, "reviewer publishing disabled: configured looper binary cannot serve `looper review submit`")
	if !strings.Contains(reason, "unknown command") {
		t.Fatalf("logged reason = %q, want it to carry the binary's own diagnostic", reason)
	}
}

func TestResolveTrustedLooperCLIPathRejectsUnexpectedCapabilityToken(t *testing.T) {
	looperPath := writeFakeLooper(t, trustedReviewWrongTokenScript)
	logger := &capturingSchedulerLogger{}

	if got := resolveTrustedLooperCLIPath(trustedLooperConfig(looperPath), logger); got != "" {
		t.Fatalf("resolveTrustedLooperCLIPath() = %q, want \"\"", got)
	}
	reason := logger.reasonFor(t, "reviewer publishing disabled: configured looper binary cannot serve `looper review submit`")
	if !strings.Contains(reason, "other-tool/9") || !strings.Contains(reason, forge.TrustedReviewCapabilityToken) {
		t.Fatalf("logged reason = %q, want the observed and expected tokens", reason)
	}
}

func TestResolveTrustedLooperCLIPathRejectsMissingBinary(t *testing.T) {
	looperPath := filepath.Join(t.TempDir(), "looper")
	logger := &capturingSchedulerLogger{}

	if got := resolveTrustedLooperCLIPath(trustedLooperConfig(looperPath), logger); got != "" {
		t.Fatalf("resolveTrustedLooperCLIPath() = %q, want \"\"", got)
	}
	logger.requireMessage(t, "reviewer publishing disabled: configured looper binary cannot serve `looper review submit`")
}

func TestResolveTrustedLooperCLIPathSkipsProbeWhenUnconfigured(t *testing.T) {
	logger := &capturingSchedulerLogger{}

	if got := resolveTrustedLooperCLIPath(config.Config{}, logger); got != "" {
		t.Fatalf("resolveTrustedLooperCLIPath() = %q, want \"\"", got)
	}
	if count := logger.countMessage("reviewer publishing disabled: configured looper binary cannot serve `looper review submit`"); count != 0 {
		t.Fatalf("verdict log count = %d, want 0 for an unconfigured looper path", count)
	}
}

func TestTrustedReviewCapabilityCachesVerdictUntilBinaryChanges(t *testing.T) {
	dir := t.TempDir()
	looperPath := filepath.Join(dir, "looper")
	probeLog := filepath.Join(dir, "probes")
	capable := fmt.Sprintf("#!/bin/sh\necho probe >> %s\nif [ \"$1\" = \"review\" ] && [ \"$2\" = \"capability\" ]; then\n  echo %s\n  exit 0\nfi\nexit 1\n", probeLog, forge.TrustedReviewCapabilityToken)
	if err := os.WriteFile(looperPath, []byte(capable), 0o755); err != nil {
		t.Fatalf("WriteFile(looperPath) error = %v", err)
	}
	logger := &capturingSchedulerLogger{}
	cfg := trustedLooperConfig(looperPath)

	for attempt := 0; attempt < 3; attempt++ {
		if got := resolveTrustedLooperCLIPath(cfg, logger); got != looperPath {
			t.Fatalf("resolveTrustedLooperCLIPath() attempt %d = %q, want %q", attempt, got, looperPath)
		}
	}
	if probes := countProbes(t, probeLog); probes != 1 {
		t.Fatalf("probe count = %d, want 1 for repeated resolves of an unchanged binary", probes)
	}
	if count := logger.countMessage("trusted looper review-submit wrapper verified"); count != 1 {
		t.Fatalf("verdict log count = %d, want 1 for an unchanged verdict", count)
	}

	downgraded := "#!/bin/sh\necho probe >> " + probeLog + "\necho 'unknown command \"review\" for \"looper\"' >&2\nexit 1\n"
	if err := os.WriteFile(looperPath, []byte(downgraded), 0o755); err != nil {
		t.Fatalf("WriteFile(downgraded looperPath) error = %v", err)
	}

	if got := resolveTrustedLooperCLIPath(cfg, logger); got != "" {
		t.Fatalf("resolveTrustedLooperCLIPath() after downgrade = %q, want \"\"", got)
	}
	if probes := countProbes(t, probeLog); probes != 2 {
		t.Fatalf("probe count after downgrade = %d, want 2", probes)
	}
	logger.requireMessage(t, "reviewer publishing disabled: configured looper binary cannot serve `looper review submit`")
}

func countProbes(t *testing.T, probeLog string) int {
	t.Helper()
	contents, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", probeLog, err)
	}
	return len(strings.Fields(string(contents)))
}
