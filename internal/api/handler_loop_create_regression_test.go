package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestForcedManualWorkerMetadataInitializesNestedOverride(t *testing.T) {
	tests := []struct {
		name string
		in   *string
	}{
		{name: "nil", in: nil},
		{name: "empty object", in: stringPtr("{}")},
		{name: "missing worker", in: stringPtr(`{"source":"manual"}`)},
		{name: "non-object worker", in: stringPtr(`{"worker":"legacy","source":"manual"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forcedManualWorkerMetadataJSONCompat(tt.in)
			if got == nil {
				t.Fatal("forcedManualWorkerMetadataJSONCompat() returned nil")
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(*got), &metadata); err != nil {
				t.Fatalf("metadata JSON invalid: %v", err)
			}
			worker, ok := metadata["worker"].(map[string]any)
			if !ok || worker["issueClaimOverride"] != true {
				t.Fatalf("worker metadata = %#v, want issueClaimOverride=true", metadata["worker"])
			}
			if _, ok := worker["autoDiscovered"]; ok {
				t.Fatalf("worker metadata = %#v, want autoDiscovered removed", worker)
			}
		})
	}
}

func TestDeriveWorkerTitleTruncatesAtRuneBoundary(t *testing.T) {
	prompt := strings.Repeat("界", 81)
	got := deriveWorkerTitle(&prompt, nil, nil, nil, nil)
	if got != strings.Repeat("界", 80) {
		t.Fatalf("deriveWorkerTitle() = %q, want 80 complete runes", got)
	}
}
