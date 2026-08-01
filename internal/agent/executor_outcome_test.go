package agent

import "testing"

func outcomeExecution(sink *[]Outcome) *execution {
	executor := &ConfiguredExecutor{onOutcome: func(o Outcome) { *sink = append(*sink, o) }}
	return &execution{
		executor:    executor,
		executionID: "exec_1",
		input:       RunInput{ProjectID: "proj", LoopID: "loop_1", RunID: "run_1"},
	}
}

func TestReportOutcomeMapsTerminalStatus(t *testing.T) {
	tests := []struct {
		status        string
		wantReported  bool
		wantSucceeded bool
	}{
		{status: "completed", wantReported: true, wantSucceeded: true},
		{status: "failed", wantReported: true},
		// A hung agent and a refused one both mean work is not getting done.
		{status: "timeout", wantReported: true},
		// Looper killed it. That is looper's decision, not evidence about the
		// provider, and counting it would let an operator stop trip the gate.
		{status: "killed"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.status, func(t *testing.T) {
			outcomes := make([]Outcome, 0, 1)
			outcomeExecution(&outcomes).reportOutcome(tt.status)

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
			if got.Status != tt.status || got.LoopID != "loop_1" || got.RunID != "run_1" || got.ExecutionID != "exec_1" || got.ProjectID != "proj" {
				t.Fatalf("outcome identity not carried through: %+v", got)
			}
		})
	}
}

func TestReportOutcomeWithoutSinkIsSafe(t *testing.T) {
	execution := &execution{executor: &ConfiguredExecutor{}, input: RunInput{}}
	execution.reportOutcome("failed")
}
