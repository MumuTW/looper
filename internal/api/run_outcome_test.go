package api

import (
	"encoding/json"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

func TestSerializeRunDerivesHistoricalFixerOutcome(t *testing.T) {
	t.Parallel()

	checkpoint := `{"fixItems":[{"id":"c1","type":"comment","threadId":"t1"}],"reconcileCommits":{"newCommitShas":["commit-1"]},"push":{"pushed":true},"resolvedComments":{"items":[{"status":"resolved","replyState":"sent"}]}}`
	currentStep := "recheck"
	errorMessage := "recheck failed"
	response := serializeRun(storage.RunRecord{Status: "failed", CurrentStep: &currentStep, CheckpointJSON: &checkpoint, ErrorMessage: &errorMessage})

	if response.Outcome == nil || response.Outcome.PrimaryFailure == nil || response.Outcome.PrimaryFailure.Message != errorMessage || !response.Outcome.PartialSuccess {
		t.Fatalf("serializeRun().Outcome = %#v, want read-time historical outcome", response.Outcome)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	outcome, ok := wire["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("wire outcome = %#v, want object", wire["outcome"])
	}
	if _, ok := outcome["primaryFailure"].(map[string]any); !ok {
		t.Fatalf("wire primaryFailure = %#v, want object", outcome["primaryFailure"])
	}
	if progress, ok := outcome["progress"].(map[string]any); !ok || progress["pushed"] != true {
		t.Fatalf("wire progress = %#v, want pushed outcome", outcome["progress"])
	}
}

func TestDecorateLoopDiagnosticsSurfacesHistoricalFixerOutcome(t *testing.T) {
	t.Parallel()

	checkpoint := `{"fixItems":[{"id":"c1","type":"comment","threadId":"t1"}],"reconcileCommits":{"newCommitShas":["commit-1"]},"push":{"pushed":true}}`
	currentStep := "recheck"
	errorMessage := "recheck failed"
	run := storage.RunRecord{Status: "failed", CurrentStep: &currentStep, CheckpointJSON: &checkpoint, ErrorMessage: &errorMessage}
	view := loopResponse{}

	decorateLoopDiagnostics(&view, nil, &run)

	if view.Outcome == nil || view.Outcome.PrimaryFailure == nil || !view.Outcome.PartialSuccess {
		t.Fatalf("loop Outcome = %#v, want dashboard-facing historical partial success", view.Outcome)
	}
}
