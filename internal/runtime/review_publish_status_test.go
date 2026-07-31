package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/forge"
)

func TestReviewPublishReadinessForUnknownBinaryDoesNotProbe(t *testing.T) {
	t.Parallel()

	probeLog := filepath.Join(t.TempDir(), "probe-log")
	looperPath := writeFakeLooper(t, "#!/bin/sh\necho probe > "+probeLog+"\necho "+forge.TrustedReviewCapabilityToken+"\n")
	got := ReviewPublishReadinessFor(trustedLooperConfig(looperPath))
	if got.Known || got.Capable || !got.PublishingDisabled {
		t.Fatalf("got = %#v, want unknown fail-closed readiness", got)
	}
	if got.LooperPath != looperPath {
		t.Fatalf("LooperPath = %q, want %q", got.LooperPath, looperPath)
	}
	if got.Reason != "capability has not been probed yet" {
		t.Fatalf("Reason = %q, want explicit unknown reason", got.Reason)
	}
	if _, err := os.Stat(probeLog); !os.IsNotExist(err) {
		t.Fatalf("status probed configured binary: Stat(%q) error = %v, want not exist", probeLog, err)
	}
}

func TestReviewPublishReadinessForCachedCapableBinary(t *testing.T) {
	t.Parallel()

	looperPath := writeFakeLooper(t, trustedReviewCapableScript)
	if got := resolveTrustedLooperCLIPath(trustedLooperConfig(looperPath), nil); got != looperPath {
		t.Fatalf("resolveTrustedLooperCLIPath() = %q, want %q", got, looperPath)
	}
	got := ReviewPublishReadinessFor(trustedLooperConfig(looperPath))
	if !got.Known || !got.Capable || got.PublishingDisabled {
		t.Fatalf("got = %#v, want cached capable publishing", got)
	}
	if got.LooperPath != looperPath {
		t.Fatalf("LooperPath = %q, want %q", got.LooperPath, looperPath)
	}
	if got.Capability != forge.TrustedReviewCapabilityToken {
		t.Fatalf("Capability = %q, want %q", got.Capability, forge.TrustedReviewCapabilityToken)
	}
	if got.Reason != "" {
		t.Fatalf("Reason = %q, want empty", got.Reason)
	}
}

func TestReviewPublishReadinessForMissingPath(t *testing.T) {
	t.Parallel()

	got := ReviewPublishReadinessFor(config.Config{})
	if !got.Known || got.Capable || !got.PublishingDisabled {
		t.Fatalf("got = %#v, want publishing disabled", got)
	}
	if got.LooperPath != "" {
		t.Fatalf("LooperPath = %q, want empty", got.LooperPath)
	}
	if got.Reason == "" {
		t.Fatal("Reason empty, want missing-path message")
	}
}

func TestReviewPublishReadinessForCachedFailureRedactsProbeOutput(t *testing.T) {
	t.Parallel()

	looperPath := writeFakeLooper(t, "#!/bin/sh\necho 'TOKEN=super-secret-status-value' >&2\nexit 1\n")
	if got := resolveTrustedLooperCLIPath(trustedLooperConfig(looperPath), nil); got != "" {
		t.Fatalf("resolveTrustedLooperCLIPath() = %q, want empty", got)
	}
	got := ReviewPublishReadinessFor(trustedLooperConfig(looperPath))
	if !got.Known || got.Capable || !got.PublishingDisabled {
		t.Fatalf("got = %#v, want publishing disabled", got)
	}
	if got.Reason != "configured looper binary capability probe failed" {
		t.Fatalf("Reason = %q, want classified failure", got.Reason)
	}
	if strings.Contains(got.Reason, "super-secret-status-value") || strings.Contains(got.Reason, "TOKEN=") {
		t.Fatalf("Reason leaked raw probe output: %q", got.Reason)
	}
}
