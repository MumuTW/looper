package fixer

import (
	"testing"

	"github.com/MumuTW/looper/internal/reviewitem"
)

func TestNormalizeFixItemsReadsValidatedReviewItemSeverity(t *testing.T) {
	items := normalizeFixItems([]map[string]any{{
		"id":       "comment-1",
		"threadId": "thread-1",
		"state":    "UNRESOLVED",
		"source":   "review_thread",
		"body":     "Fix the nil dereference.\n\n<!-- looper:review-item severity=blocking -->",
	}}, nil, false)
	if len(items) != 1 || items[0].Severity != reviewitem.SeverityBlocking {
		t.Fatalf("normalizeFixItems() = %#v, want one blocking review item", items)
	}
}

func TestNormalizeFixItemsDoesNotInferMissingSeverity(t *testing.T) {
	items := normalizeFixItems([]map[string]any{{
		"id":       "comment-1",
		"threadId": "thread-1",
		"state":    "UNRESOLVED",
		"source":   "review_thread",
		"body":     "This prose says the issue is blocking.",
	}}, nil, false)
	if len(items) != 1 || items[0].Severity != "" {
		t.Fatalf("normalizeFixItems() = %#v, want no inferred severity", items)
	}
}
