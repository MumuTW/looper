package fixer

import (
	"testing"

	"github.com/MumuTW/looper/internal/roles"
	"github.com/MumuTW/looper/internal/storage"
)

// The first failure recorded is the causal one. Later failures -- a contract error
// raised while parking, a problem on the way out -- must not overwrite it, because a
// run's stored story should be what broke it, not what happened most recently.

func TestRecordFailureKeepsTheFirstCauseAsPrimary(t *testing.T) {
	t.Parallel()
	checkpoint := fixerCheckpoint{}

	checkpoint.recordFailure(stepRepair, &loopError{message: "agent timed out", kind: roles.FailureRetryableTransient})
	checkpoint.recordFailure(stepPush, &loopError{message: "remote head changed", kind: roles.FailureRetryableAfterResume})
	checkpoint.recordFailure(stepRecheck, &loopError{message: "needs a human", kind: roles.FailureManualIntervention})

	if checkpoint.Outcome == nil || checkpoint.Outcome.PrimaryFailure == nil {
		t.Fatalf("Outcome = %#v, want a recorded primary failure", checkpoint.Outcome)
	}
	primary := checkpoint.Outcome.PrimaryFailure
	if primary.Step != string(stepRepair) || primary.Message != "agent timed out" {
		t.Fatalf("PrimaryFailure = %#v, want the first failure at repair", primary)
	}
	if primary.Retryable == nil || !*primary.Retryable {
		t.Fatalf("PrimaryFailure.Retryable = %v, want true for retryable_transient", primary.Retryable)
	}
	if len(checkpoint.Outcome.SecondaryIssues) != 2 {
		t.Fatalf("SecondaryIssues = %#v, want the two later failures", checkpoint.Outcome.SecondaryIssues)
	}
	if checkpoint.Outcome.SecondaryIssues[0].Step != string(stepPush) {
		t.Fatalf("SecondaryIssues[0] = %#v, want push recorded second", checkpoint.Outcome.SecondaryIssues[0])
	}
	// Manual intervention is not retryable, and the distinction has to survive.
	last := checkpoint.Outcome.SecondaryIssues[1]
	if last.Retryable == nil || *last.Retryable {
		t.Fatalf("SecondaryIssues[1].Retryable = %v, want false for manual_intervention", last.Retryable)
	}
}

func TestRecordFailureIgnoresNilFailure(t *testing.T) {
	t.Parallel()
	checkpoint := fixerCheckpoint{}
	checkpoint.recordFailure(stepRepair, nil)
	if checkpoint.Outcome != nil {
		t.Fatalf("Outcome = %#v, want nothing recorded for a nil failure", checkpoint.Outcome)
	}
}

func runWith(t *testing.T, status, currentStep, checkpointJSON, errorMessage string) storage.RunRecord {
	t.Helper()
	record := storage.RunRecord{ID: "run_1", LoopID: "loop_1", Status: status}
	if currentStep != "" {
		record.CurrentStep = stringPtr(currentStep)
	}
	if checkpointJSON != "" {
		record.CheckpointJSON = stringPtr(checkpointJSON)
	}
	if errorMessage != "" {
		record.ErrorMessage = stringPtr(errorMessage)
	}
	return record
}

func TestDeriveRunOutcomeReturnsNilWhenThereIsNothingToSay(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		run  storage.RunRecord
	}{
		{name: "still running", run: runWith(t, "running", string(stepRepair), `{"fixItems":[{"id":"c1"}]}`, "")},
		{name: "no checkpoint", run: runWith(t, "failed", string(stepRepair), "", "boom")},
		{name: "unparseable checkpoint", run: runWith(t, "failed", string(stepRepair), `{not json`, "boom")},
		{name: "not a fixer checkpoint", run: runWith(t, "failed", "", `{"resumePolicy":"replay_step"}`, "boom")},
		{name: "succeeded with no stored outcome", run: runWith(t, "success", string(stepRecheck), `{"fixItems":[{"id":"c1"}]}`, "")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := DeriveRunOutcome(testCase.run); got != nil {
				t.Fatalf("DeriveRunOutcome() = %#v, want nil", got)
			}
		})
	}
}

// TestDeriveRunOutcomeBackfillsHistoricalFailures is why the projection is derived
// rather than simply returned: runs recorded before the outcome field existed still
// have to report their failure story.
func TestDeriveRunOutcomeBackfillsHistoricalFailures(t *testing.T) {
	t.Parallel()

	t.Run("no stored outcome at all", func(t *testing.T) {
		t.Parallel()
		run := runWith(t, "failed", string(stepPush), `{"fixItems":[{"id":"c1"}]}`, "push rejected")
		got := DeriveRunOutcome(run)
		if got == nil || got.PrimaryFailure == nil {
			t.Fatalf("DeriveRunOutcome() = %#v, want a backfilled primary failure", got)
		}
		if got.PrimaryFailure.Step != string(stepPush) || got.PrimaryFailure.Message != "push rejected" {
			t.Fatalf("PrimaryFailure = %#v, want the run's own step and message", got.PrimaryFailure)
		}
	})

	t.Run("stored outcome without a primary failure", func(t *testing.T) {
		t.Parallel()
		run := runWith(t, "failed", string(stepRecheck), `{"resumePolicy":"manual_intervention","outcome":{}}`, "parked")
		got := DeriveRunOutcome(run)
		if got == nil || got.PrimaryFailure == nil {
			t.Fatalf("DeriveRunOutcome() = %#v, want the gap backfilled", got)
		}
		if got.PrimaryFailure.Kind != roles.FailureManualIntervention {
			t.Fatalf("PrimaryFailure.Kind = %q, want manual_intervention from the parked policy", got.PrimaryFailure.Kind)
		}
		if got.PrimaryFailure.Retryable == nil || *got.PrimaryFailure.Retryable {
			t.Fatalf("PrimaryFailure.Retryable = %v, want false for a parked run", got.PrimaryFailure.Retryable)
		}
	})

	t.Run("stored outcome is authoritative", func(t *testing.T) {
		t.Parallel()
		run := runWith(t, "failed", string(stepRecheck), `{"outcome":{"primaryFailure":{"step":"repair","message":"agent timed out"}}}`, "a later message")
		got := DeriveRunOutcome(run)
		if got == nil || got.PrimaryFailure == nil {
			t.Fatalf("DeriveRunOutcome() = %#v, want the stored outcome", got)
		}
		if got.PrimaryFailure.Message != "agent timed out" {
			t.Fatalf("PrimaryFailure.Message = %q, want the stored cause rather than the run's error message", got.PrimaryFailure.Message)
		}
	})
}

func TestRunStatusIsFailure(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"failed", "interrupted", "parse_failed", "FAILED"} {
		if !runStatusIsFailure(status) {
			t.Fatalf("runStatusIsFailure(%q) = false, want true", status)
		}
	}
	for _, status := range []string{"success", "running", "skipped", ""} {
		if runStatusIsFailure(status) {
			t.Fatalf("runStatusIsFailure(%q) = true, want false", status)
		}
	}
}
