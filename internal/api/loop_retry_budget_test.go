package api

import (
	"encoding/json"
	"testing"

	"github.com/MumuTW/looper/internal/loops"
)

func TestResetFixerLoopRetryMetadataClearsRoundBudget(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(map[string]any{
		"fixerFailureStreak": map[string]any{"count": 3},
		"pauseReason":        "fixer_non_converging",
		"fixerRoundBudget":   map[string]any{"rounds": 9, "lastHeadSha": "head-9", "firstRoundAt": "t0", "recordedAt": "t1"},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	current := string(encoded)
	updated, err := resetFixerLoopRetryMetadata(&current)
	if err != nil {
		t.Fatalf("resetFixerLoopRetryMetadata() error = %v", err)
	}
	if updated == nil {
		t.Fatal("updated = nil, want metadata")
	}
	meta, err := loops.DecodeMetadataObjectForWrite(updated)
	if err != nil {
		t.Fatalf("DecodeMetadataObjectForWrite() error = %v", err)
	}
	if _, ok := meta["fixerFailureStreak"]; ok {
		t.Fatalf("fixerFailureStreak survived reset: %v", meta)
	}
	if _, ok := meta["fixerRoundBudget"]; ok {
		t.Fatalf("fixerRoundBudget survived reset: %v", meta)
	}
	if reason, _ := meta["pauseReason"].(string); reason != "" {
		t.Fatalf("pauseReason = %q, want cleared", reason)
	}
}

// TestResetFixerLoopRetryMetadataPreservesForeignPause confirms the retry path
// clears only the failure-streak and round-budget gates it owns; another gate's
// pause reason is not lifted by an operator retrying the fixer loop.
func TestResetFixerLoopRetryMetadataPreservesForeignPause(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(map[string]any{
		"fixerRoundBudget":   map[string]any{"rounds": 9, "lastHeadSha": "head-9", "recordedAt": "t1"},
		"pauseReason":        "fixer_label_mismatch",
		"fixerFailureStreak": map[string]any{"count": 2},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	current := string(encoded)
	updated, err := resetFixerLoopRetryMetadata(&current)
	if err != nil {
		t.Fatalf("resetFixerLoopRetryMetadata() error = %v", err)
	}
	meta, err := loops.DecodeMetadataObjectForWrite(updated)
	if err != nil {
		t.Fatalf("DecodeMetadataObjectForWrite() error = %v", err)
	}
	if _, ok := meta["fixerRoundBudget"]; ok {
		t.Fatalf("fixerRoundBudget survived reset: %v", meta)
	}
	if _, ok := meta["fixerFailureStreak"]; ok {
		t.Fatalf("fixerFailureStreak survived reset: %v", meta)
	}
	if reason, _ := meta["pauseReason"].(string); reason != "fixer_label_mismatch" {
		t.Fatalf("pauseReason = %q, want fixer_label_mismatch preserved", reason)
	}
}
