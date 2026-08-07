package auditor

import (
	"encoding/json"
	"testing"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

func TestCandidatesFromMergeOutcomesUsesOnlySuccessfulGatekeeperEvents(t *testing.T) {
	payload, err := json.Marshal(gatekeeper.MergeOutcome{Version: 1, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, HeadSHA: "abc", Merged: true, TouchedFilesAvailable: true})
	if err != nil {
		t.Fatal(err)
	}
	notMerged, err := json.Marshal(gatekeeper.MergeOutcome{Version: 1, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 43, HeadSHA: "def", Merged: false})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := CandidatesFromMergeOutcomes([]storage.EventLogRecord{
		{ID: "other", EventType: "unrelated", PayloadJSON: "{}", CreatedAt: "2026-07-31T10:00:00.000Z"},
		{ID: "refused", EventType: gatekeeper.MergeOutcomeEventType, PayloadJSON: string(notMerged), CreatedAt: "2026-07-31T10:01:00.000Z"},
		{ID: "merged", EventType: gatekeeper.MergeOutcomeEventType, PayloadJSON: string(payload), CreatedAt: "2026-07-31T10:02:00.000Z"},
	})
	if err != nil || len(candidates) != 1 || candidates[0].ProjectID != "project_1" || candidates[0].Repo != "acme/looper" || candidates[0].PRNumber != 42 || candidates[0].HeadSHA != "abc" || candidates[0].MergedAt.Format("2006-01-02T15:04:05.000Z") != "2026-07-31T10:02:00.000Z" {
		t.Fatalf("CandidatesFromMergeOutcomes() = %#v, %v", candidates, err)
	}
}

func TestCandidatesFromMergeOutcomesUsesCoordinatorMergeWatchTimestamp(t *testing.T) {
	payload, err := json.Marshal(eventlog.CoordinatorPullRequestMerged{
		Version: 1, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, HeadSHA: "abc", MergedAt: "2026-07-31T09:58:00.000Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := CandidatesFromMergeOutcomes([]storage.EventLogRecord{{
		ID: "coordinator-merge", EventType: eventlog.CoordinatorPullRequestMergedEventType, PayloadJSON: string(payload), CreatedAt: "2026-07-31T10:02:00.000Z",
	}})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("CandidatesFromMergeOutcomes() = %#v, %v", candidates, err)
	}
	if got := candidates[0].MergedAt.Format("2006-01-02T15:04:05.000Z"); got != "2026-07-31T09:58:00.000Z" {
		t.Fatalf("candidate merge time = %q, want forge MergedAt", got)
	}
}

func TestCandidatesFromMergeOutcomesFallsBackToEventProjectForLegacyCoordinatorPayload(t *testing.T) {
	payload := `{"repo":"acme/looper","prNumber":42,"headSha":"abc","mergedAt":"2026-07-31T09:58:00Z"}`
	projectID := " project with padding "
	candidates, err := CandidatesFromMergeOutcomes([]storage.EventLogRecord{{
		ID: "legacy-coordinator", EventType: eventlog.CoordinatorPullRequestMergedEventType,
		ProjectID: &projectID, PayloadJSON: payload, CreatedAt: "2026-07-31T10:02:00.000Z",
	}})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("CandidatesFromMergeOutcomes() = %#v, %v", candidates, err)
	}
	if candidates[0].ProjectID != projectID {
		t.Fatalf("candidate project ID = %q, want exact event project %q", candidates[0].ProjectID, projectID)
	}
}

func TestCandidatesFromMergeOutcomesFallsBackToEventCreatedAtForMalformedCoordinatorTimestamp(t *testing.T) {
	payload := `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"headSha":"abc","mergedAt":"not-a-timestamp"}`
	candidates, err := CandidatesFromMergeOutcomes([]storage.EventLogRecord{{
		ID: "malformed-coordinator-time", EventType: eventlog.CoordinatorPullRequestMergedEventType,
		PayloadJSON: payload, CreatedAt: "2026-07-31T10:02:00.000Z",
	}})
	if err != nil || len(candidates) != 1 || candidates[0].MergedAt.Format("2006-01-02T15:04:05.000Z") != "2026-07-31T10:02:00.000Z" {
		t.Fatalf("CandidatesFromMergeOutcomes() = %#v, %v, want event CreatedAt fallback", candidates, err)
	}
}

func TestCandidatesFromMergeOutcomesRejectsMalformedSuccessfulEvidence(t *testing.T) {
	_, err := CandidatesFromMergeOutcomes([]storage.EventLogRecord{{ID: "broken", EventType: gatekeeper.MergeOutcomeEventType, PayloadJSON: `{`, CreatedAt: "2026-07-31T10:00:00.000Z"}})
	if err == nil {
		t.Fatal("CandidatesFromMergeOutcomes() error = nil, want malformed evidence failure")
	}
}
