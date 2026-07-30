package auditor

import (
	"encoding/json"
	"testing"

	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

func TestCandidatesFromMergeOutcomesUsesOnlySuccessfulGatekeeperEvents(t *testing.T) {
	payload, err := json.Marshal(gatekeeper.MergeOutcome{Version: 1, PRNumber: 42, HeadSHA: "abc", Merged: true})
	if err != nil {
		t.Fatal(err)
	}
	notMerged, err := json.Marshal(gatekeeper.MergeOutcome{Version: 1, PRNumber: 43, HeadSHA: "def", Merged: false})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := CandidatesFromMergeOutcomes([]storage.EventLogRecord{
		{ID: "other", EventType: "unrelated", PayloadJSON: "{}", CreatedAt: "2026-07-31T10:00:00.000Z"},
		{ID: "refused", EventType: gatekeeper.MergeOutcomeEventType, PayloadJSON: string(notMerged), CreatedAt: "2026-07-31T10:01:00.000Z"},
		{ID: "merged", EventType: gatekeeper.MergeOutcomeEventType, PayloadJSON: string(payload), CreatedAt: "2026-07-31T10:02:00.000Z"},
	})
	if err != nil || len(candidates) != 1 || candidates[0].PRNumber != 42 || candidates[0].HeadSHA != "abc" || candidates[0].MergedAt.Format("2006-01-02T15:04:05.000Z") != "2026-07-31T10:02:00.000Z" {
		t.Fatalf("CandidatesFromMergeOutcomes() = %#v, %v", candidates, err)
	}
}

func TestCandidatesFromMergeOutcomesRejectsMalformedSuccessfulEvidence(t *testing.T) {
	_, err := CandidatesFromMergeOutcomes([]storage.EventLogRecord{{ID: "broken", EventType: gatekeeper.MergeOutcomeEventType, PayloadJSON: `{`, CreatedAt: "2026-07-31T10:00:00.000Z"}})
	if err == nil {
		t.Fatal("CandidatesFromMergeOutcomes() error = nil, want malformed evidence failure")
	}
}
