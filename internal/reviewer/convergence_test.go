package reviewer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

func TestConvergenceBoundTracksUnproductiveRounds(t *testing.T) {
	t.Parallel()

	r := &Runner{
		loopConfig: config.ReviewerLoopConfig{
			ConvergenceUnproductiveRounds: 3,
			AbsoluteRoundCeiling:          10,
			SeverityFloor:                 "non_blocking",
		},
		now: func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
	}

	meta := map[string]any{
		"loop": map[string]any{
			"iterationCount":        2,
			"lastStatus":            "success",
			"lastOutputFingerprint": "fp-round-2",
		},
	}
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)

	checkpoint := reviewerCheckpoint{
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123",
		},
		Snapshot:   &checkpointSnapshot{HeadSHA: "abc123"},
		SkipReason: "",
	}

	result, err := r.recordLoopSuccessMetadata(&metaStr, checkpoint, "different findings")
	if err != nil {
		t.Fatalf("recordLoopSuccessMetadata error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop := parsed["loop"].(map[string]any)

	if v := intFromAny(loop["consecutiveUnproductiveRounds"]); v != 0 {
		t.Fatalf("consecutiveUnproductiveRounds = %d, want 0", v)
	}
	if _, ok := loop["lastProductiveAt"]; !ok {
		t.Fatal("lastProductiveAt should be set after productive round")
	}
}

func TestConvergenceBoundEscalatesOnStall(t *testing.T) {
	t.Parallel()

	r := &Runner{
		loopConfig: config.ReviewerLoopConfig{
			ConvergenceUnproductiveRounds: 3,
			AbsoluteRoundCeiling:          10,
			SeverityFloor:                 "non_blocking",
		},
		now: func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
	}

	meta := map[string]any{
		"loop": map[string]any{
			"iterationCount":        5,
			"lastStatus":            "success",
			"lastOutputFingerprint": "",
		},
	}
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)

	checkpoint := reviewerCheckpoint{
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123",
		},
		Snapshot:   &checkpointSnapshot{HeadSHA: "abc123"},
		SkipReason: "",
	}

	summary := "repeated findings round"

	// Round 1: productive (different from empty)
	result, err := r.recordLoopSuccessMetadata(&metaStr, checkpoint, summary)
	if err != nil {
		t.Fatalf("round 1 error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop := parsed["loop"].(map[string]any)
	if v := intFromAny(loop["consecutiveUnproductiveRounds"]); v != 0 {
		t.Fatalf("after round 1: unproductive = %d, want 0", v)
	}

	// Round 2: same summary → unproductive (1)
	metaStr2 := string(mustMarshalMeta(parsed))
	result2, err := r.recordLoopSuccessMetadata(&metaStr2, checkpoint, summary)
	if err != nil {
		t.Fatalf("round 2 error: %v", err)
	}
	if err := json.Unmarshal([]byte(result2), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop = parsed["loop"].(map[string]any)
	if v := intFromAny(loop["consecutiveUnproductiveRounds"]); v != 1 {
		t.Fatalf("after round 2: unproductive = %d, want 1", v)
	}

	// Round 3: same summary → unproductive (2)
	metaStr3 := string(mustMarshalMeta(parsed))
	result3, err := r.recordLoopSuccessMetadata(&metaStr3, checkpoint, summary)
	if err != nil {
		t.Fatalf("round 3 error: %v", err)
	}
	if err := json.Unmarshal([]byte(result3), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop = parsed["loop"].(map[string]any)
	if v := intFromAny(loop["consecutiveUnproductiveRounds"]); v != 2 {
		t.Fatalf("after round 3: unproductive = %d, want 2", v)
	}

	// Round 4: same summary → unproductive (3) → escalation
	metaStr4 := string(mustMarshalMeta(parsed))
	result4, err := r.recordLoopSuccessMetadata(&metaStr4, checkpoint, summary)
	if err != nil {
		t.Fatalf("round 4 error: %v", err)
	}
	if err := json.Unmarshal([]byte(result4), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop = parsed["loop"].(map[string]any)

	if reason := loop["terminationReason"]; reason != "convergence_stall" {
		t.Fatalf("terminationReason = %v, want convergence_stall", reason)
	}
	if status := loop["status"]; status != "terminated" {
		t.Fatalf("status = %v, want terminated", status)
	}
}

func TestConvergenceBoundAbsoluteCeiling(t *testing.T) {
	t.Parallel()

	r := &Runner{
		loopConfig: config.ReviewerLoopConfig{
			ConvergenceUnproductiveRounds: 5,
			AbsoluteRoundCeiling:          10,
			SeverityFloor:                 "non_blocking",
		},
		now: func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
	}

	meta := map[string]any{
		"loop": map[string]any{
			"iterationCount":              9,
			"consecutiveUnproductiveRounds": 0,
			"lastStatus":                  "success",
			"lastOutputFingerprint":       "fp-old",
		},
	}
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)

	checkpoint := reviewerCheckpoint{
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123",
		},
		Snapshot:   &checkpointSnapshot{HeadSHA: "abc123"},
		SkipReason: "",
	}

	result, err := r.recordLoopSuccessMetadata(&metaStr, checkpoint, "new findings")
	if err != nil {
		t.Fatalf("recordLoopSuccessMetadata error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop := parsed["loop"].(map[string]any)

	if reason := loop["terminationReason"]; reason != "absolute_ceiling" {
		t.Fatalf("terminationReason = %v, want absolute_ceiling", reason)
	}
	if status := loop["status"]; status != "terminated" {
		t.Fatalf("status = %v, want terminated", status)
	}
}

func TestConvergenceBoundResetOnProductiveRound(t *testing.T) {
	t.Parallel()

	r := &Runner{
		loopConfig: config.ReviewerLoopConfig{
			ConvergenceUnproductiveRounds: 3,
			AbsoluteRoundCeiling:          40,
			SeverityFloor:                 "non_blocking",
		},
		now: func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
	}

	meta := map[string]any{
		"loop": map[string]any{
			"iterationCount":              7,
			"consecutiveUnproductiveRounds": 2,
			"lastStatus":                  "success",
			"lastOutputFingerprint":       "old-fp",
		},
	}
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)

	checkpoint := reviewerCheckpoint{
		PendingReview: &pendingReviewCheckpoint{
			HeadSHA: "abc123",
		},
		Snapshot:   &checkpointSnapshot{HeadSHA: "abc123"},
		SkipReason: "",
	}

	result, err := r.recordLoopSuccessMetadata(&metaStr, checkpoint, "new and different findings")
	if err != nil {
		t.Fatalf("recordLoopSuccessMetadata error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop := parsed["loop"].(map[string]any)

	if v := intFromAny(loop["consecutiveUnproductiveRounds"]); v != 0 {
		t.Fatalf("consecutiveUnproductiveRounds = %d, want 0 (reset)", v)
	}
	if status := loop["status"]; status == "terminated" {
		t.Fatal("status should not be terminated on productive round")
	}
}

func TestConvergenceBoundSkippedRoundDoesNotCount(t *testing.T) {
	t.Parallel()

	r := &Runner{
		loopConfig: config.ReviewerLoopConfig{
			ConvergenceUnproductiveRounds: 3,
			AbsoluteRoundCeiling:          10,
			SeverityFloor:                 "non_blocking",
		},
		now: func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) },
	}

	meta := map[string]any{
		"loop": map[string]any{
			"iterationCount":              4,
			"consecutiveUnproductiveRounds": 2,
			"lastStatus":                  "success",
			"lastOutputFingerprint":       "old-fp",
		},
	}
	metaJSON, _ := json.Marshal(meta)
	metaStr := string(metaJSON)

	checkpoint := reviewerCheckpoint{
		Snapshot:   &checkpointSnapshot{HeadSHA: "abc123"},
		SkipReason: "change_requested_by_human",
	}

	result, err := r.recordLoopSuccessMetadata(&metaStr, checkpoint, "skipped")
	if err != nil {
		t.Fatalf("recordLoopSuccessMetadata error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	loop := parsed["loop"].(map[string]any)

	if v := intFromAny(loop["consecutiveUnproductiveRounds"]); v != 2 {
		t.Fatalf("consecutiveUnproductiveRounds = %d, want 2 (unchanged)", v)
	}
}

func mustMarshalMeta(parsed map[string]any) []byte {
	b, err := json.Marshal(parsed)
	if err != nil {
		panic(err)
	}
	return b
}

func init() {
	_ = strings.TrimSpace
}
