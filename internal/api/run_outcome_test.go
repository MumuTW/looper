package api

import (
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
}
