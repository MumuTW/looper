package agent

import (
	"testing"
	"time"
)

func outcomeExecution(sink *[]Outcome) *execution {
	executor := &ConfiguredExecutor{onOutcome: func(o Outcome) { *sink = append(*sink, o) }}
	return &execution{
		executor:    executor,
		executionID: "exec_1",
		startedAt:   time.Date(2026, 8, 1, 22, 0, 0, 0, time.UTC),
		input:       RunInput{ProjectID: "proj", LoopID: "loop_1", RunID: "run_1"},
	}
}

func TestReportOutcomeMapsTerminalStatus(t *testing.T) {
	tests := []struct {
		status            string
		parseStatus       string
		completionPayload string
		wantReported      bool
		wantSucceeded     bool
	}{
		{status: "completed", parseStatus: "parsed", completionPayload: `{"outcome":"completed","summary":"done"}`, wantReported: true, wantSucceeded: true},
		// A parsed marker that declares a retryable block is a failed run to
		// every role runner. Recording it as a success would let a provider
		// that politely reports "rate limited" dilute the ratio on every
		// attempt while the runner keeps retrying.
		{status: "completed", parseStatus: "parsed", completionPayload: `{"outcome":"blocked","failure_kind":"retryable_transient","summary":"rate limited"}`, wantReported: true},
		// A block needing a human is not the provider failing, and backing off
		// from the provider would not help.
		{status: "completed", parseStatus: "parsed", completionPayload: `{"outcome":"blocked","failure_kind":"manual_intervention","summary":"needs a decision"}`, wantReported: true, wantSucceeded: true},
		{status: "completed", parseStatus: "missing", wantReported: true},
		{status: "completed", parseStatus: "invalid_json", wantReported: true},
		{status: "failed", parseStatus: "missing", wantReported: true},
		// A hung agent and a refused one both mean work is not getting done.
		{status: "timeout", parseStatus: "missing", wantReported: true},
		// Looper killed it. That is looper's decision, not evidence about the
		// provider, and counting it would let an operator stop trip the gate.
		{status: "killed"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.status, func(t *testing.T) {
			outcomes := make([]Outcome, 0, 1)
			outcomeExecution(&outcomes).reportOutcome(tt.status, tt.parseStatus, tt.completionPayload)

			if !tt.wantReported {
				if len(outcomes) != 0 {
					t.Fatalf("status %q was reported as %+v; it must not reach the health gate", tt.status, outcomes)
				}
				return
			}
			if len(outcomes) != 1 {
				t.Fatalf("status %q produced %d outcomes, want 1", tt.status, len(outcomes))
			}
			got := outcomes[0]
			if got.Succeeded != tt.wantSucceeded {
				t.Fatalf("status %q Succeeded = %v, want %v", tt.status, got.Succeeded, tt.wantSucceeded)
			}
			// The health gate needs this to tell a probe from a long-running
			// execution admitted before the gate opened.
			if got.StartedAt.IsZero() {
				t.Fatalf("outcome carried no StartedAt: %+v", got)
			}
			if got.Status != tt.status || got.LoopID != "loop_1" || got.RunID != "run_1" || got.ExecutionID != "exec_1" || got.ProjectID != "proj" {
				t.Fatalf("outcome identity not carried through: %+v", got)
			}
		})
	}
}

func TestReportOutcomeWithoutSinkIsSafe(t *testing.T) {
	execution := &execution{executor: &ConfiguredExecutor{}, input: RunInput{}}
	execution.reportOutcome("failed", "missing", "")
}
