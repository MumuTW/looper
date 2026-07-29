package runtime

import (
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

func TestReviewPublishReadinessForCapableBinary(t *testing.T) {
	t.Parallel()

	looperPath := writeFakeLooper(t, trustedReviewCapableScript)
	got := ReviewPublishReadinessFor(trustedLooperConfig(looperPath))
	if !got.Capable || got.PublishingDisabled {
		t.Fatalf("got = %#v, want capable publishing", got)
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

func TestReviewPublishReadinessForIncapableBinary(t *testing.T) {
	t.Parallel()

	looperPath := writeFakeLooper(t, trustedReviewIncapableScript)
	got := ReviewPublishReadinessFor(trustedLooperConfig(looperPath))
	if got.Capable || !got.PublishingDisabled {
		t.Fatalf("got = %#v, want publishing disabled", got)
	}
	if got.LooperPath != looperPath {
		t.Fatalf("LooperPath = %q, want %q", got.LooperPath, looperPath)
	}
	if got.Capability != "" {
		t.Fatalf("Capability = %q, want empty", got.Capability)
	}
	if got.Reason == "" {
		t.Fatal("Reason empty, want probe failure detail")
	}
}

func TestReviewPublishReadinessForMissingPath(t *testing.T) {
	t.Parallel()

	got := ReviewPublishReadinessFor(config.Config{})
	if got.Capable || !got.PublishingDisabled {
		t.Fatalf("got = %#v, want publishing disabled", got)
	}
	if got.LooperPath != "" {
		t.Fatalf("LooperPath = %q, want empty", got.LooperPath)
	}
	if got.Reason == "" {
		t.Fatal("Reason empty, want missing-path message")
	}
}

func TestReviewPublishReadinessForMissingBinaryOnDisk(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing-looper")
	got := ReviewPublishReadinessFor(trustedLooperConfig(missing))
	if got.Capable || !got.PublishingDisabled {
		t.Fatalf("got = %#v, want publishing disabled", got)
	}
	if got.Reason == "" {
		t.Fatal("Reason empty, want resolve failure")
	}
}
