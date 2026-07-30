package fixer

import (
	"errors"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
)

// Cross-component contract: every runner's loop-metadata merge decodes through
// the shared strict decoder, so a malformed stored value blocks the merge
// instead of being replaced with only the updates.
func TestMergeLoopMetadataJSONRejectsMalformedCurrentValue(t *testing.T) {
	t.Parallel()

	malformed := `{"issueUrl":`
	got, err := mergeLoopMetadataJSON(&malformed, map[string]any{"issueUrl": "https://example.com/issues/1"})
	if err == nil {
		t.Fatalf("mergeLoopMetadataJSON(malformed) = %q, want error", got)
	}
	if !errors.Is(err, loops.ErrMalformedLoopMetadata) {
		t.Fatalf("mergeLoopMetadataJSON(malformed) error = %v, want ErrMalformedLoopMetadata", err)
	}

	valid := `{"worktreeId":"wt-1"}`
	out, err := mergeLoopMetadataJSON(&valid, map[string]any{"issueUrl": "https://example.com/issues/1"})
	if err != nil {
		t.Fatalf("mergeLoopMetadataJSON(valid) error = %v", err)
	}
	if !strings.Contains(out, `"worktreeId"`) || !strings.Contains(out, `"issueUrl"`) {
		t.Fatalf("mergeLoopMetadataJSON(valid) = %q, want existing keys preserved and update applied", out)
	}
}
