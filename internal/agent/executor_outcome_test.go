package agent

import (
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
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
		{status: "completed", parseStatus: "parsed", completionPayload: `{"outcome":"mostly-done","summary":"partial"}`, wantReported: true},
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
			exec := outcomeExecution(&outcomes)
			exec.reportOutcome(tt.status, tt.parseStatus, tt.completionPayload, "")

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
			if !got.StartedAt.Equal(exec.startedAt) {
				t.Fatalf("outcome StartedAt = %s, want %s", got.StartedAt, exec.startedAt)
			}
			if got.Status != tt.status || got.LoopID != "loop_1" || got.RunID != "run_1" || got.ExecutionID != "exec_1" || got.ProjectID != "proj" {
				t.Fatalf("outcome identity not carried through: %+v", got)
			}
		})
	}
}

func TestReportOutcomeWithoutSinkIsSafe(t *testing.T) {
	execution := &execution{executor: &ConfiguredExecutor{}, input: RunInput{}}
	execution.reportOutcome("failed", "missing", "", "")
}

func TestReportOutcomeCarriesEffectiveVendor(t *testing.T) {
	outcomes := make([]Outcome, 0, 1)
	exec := outcomeExecution(&outcomes)
	exec.executor.config.Vendor = config.AgentVendorClaudeCode
	exec.reportOutcome("failed", "missing", "", "")
	if len(outcomes) != 1 {
		t.Fatalf("reported %d outcomes, want 1", len(outcomes))
	}
	if got := outcomes[0].Vendor; got != string(config.AgentVendorClaudeCode) {
		t.Fatalf("outcome Vendor = %q, want %q", got, config.AgentVendorClaudeCode)
	}
}

func TestReportOutcomeAcceptsRawJSONContract(t *testing.T) {
	outcomes := make([]Outcome, 0, 1)
	exec := outcomeExecution(&outcomes)
	exec.input.CompletionContract = CompletionContractRawJSON
	exec.reportOutcome("completed", "missing", "", `{"disposition":"valid"}`)
	if len(outcomes) != 1 || !outcomes[0].Succeeded {
		t.Fatalf("raw JSON outcome = %#v, want one successful outcome", outcomes)
	}

	outcomes = outcomes[:0]
	exec.reportOutcome("completed", "missing", "", "not json")
	if len(outcomes) != 1 || outcomes[0].Succeeded {
		t.Fatalf("invalid raw JSON outcome = %#v, want one failed outcome", outcomes)
	}
}

func TestReportOutcomeAcceptsRawJSONEnvelopeContract(t *testing.T) {
	outcomes := make([]Outcome, 0, 1)
	exec := outcomeExecution(&outcomes)
	exec.input.CompletionContract = CompletionContractRawJSONEnvelope
	exec.reportOutcome("completed", "missing", "", "classifier result:\n{\"decisions\":[]}\n")
	if len(outcomes) != 1 || !outcomes[0].Succeeded {
		t.Fatalf("raw JSON envelope outcome = %#v, want one successful outcome", outcomes)
	}
}

func TestReportOutcomeUsesRawJSONCompletionValidator(t *testing.T) {
	outcomes := make([]Outcome, 0, 1)
	exec := outcomeExecution(&outcomes)
	exec.input.CompletionContract = CompletionContractRawJSON
	exec.input.CompletionValidator = func(message string) bool { return message == `{"ok":true}` }
	exec.reportOutcome("completed", "missing", "", "event stream\n{\"ok\":true}\n")
	if len(outcomes) != 1 || !outcomes[0].Succeeded {
		t.Fatalf("valid semantic raw JSON outcome = %#v, want one successful outcome", outcomes)
	}

	outcomes = outcomes[:0]
	exec.reportOutcome("completed", "missing", "", "{\"ok\":false}")
	if len(outcomes) != 1 || outcomes[0].Succeeded {
		t.Fatalf("invalid semantic raw JSON outcome = %#v, want one failed outcome", outcomes)
	}
}

func TestReportOutcomeUsesFileCompletionValidator(t *testing.T) {
	outcomes := make([]Outcome, 0, 1)
	exec := outcomeExecution(&outcomes)
	exec.input.CompletionContract = CompletionContractFile
	exec.input.CompletionValidator = func(string) bool { return true }
	exec.reportOutcome("completed", "missing", "", "agent wrote assessment to file")
	if len(outcomes) != 1 || !outcomes[0].Succeeded {
		t.Fatalf("valid file-backed outcome = %#v, want one successful outcome", outcomes)
	}

	outcomes = outcomes[:0]
	exec.input.CompletionValidator = func(string) bool { return false }
	exec.reportOutcome("completed", "missing", "", "agent wrote malformed assessment")
	if len(outcomes) != 1 || outcomes[0].Succeeded {
		t.Fatalf("invalid file-backed outcome = %#v, want one failed outcome", outcomes)
	}
}

func TestReportOutcomeRequiresFixerOutcomeContract(t *testing.T) {
	outcomes := make([]Outcome, 0, 1)
	exec := outcomeExecution(&outcomes)
	exec.input.CompletionContract = CompletionContractFixerMarker
	exec.reportOutcome("completed", "parsed", `{"summary":"applied fixes"}`, "")
	if len(outcomes) != 1 || outcomes[0].Succeeded {
		t.Fatalf("summary-only fixer outcome = %#v, want one failed outcome", outcomes)
	}

	outcomes = outcomes[:0]
	exec.reportOutcome("completed", "parsed", `{"outcome":"completed","summary":"applied fixes"}`, "")
	if len(outcomes) != 1 || !outcomes[0].Succeeded {
		t.Fatalf("declared fixer outcome = %#v, want one successful outcome", outcomes)
	}
}

func TestReportOutcomeAcceptsReviewerOutcomes(t *testing.T) {
	for _, outcome := range []string{"clean", "non_blocking", "blocking", "actionable"} {
		outcome := outcome
		t.Run(outcome, func(t *testing.T) {
			outcomes := make([]Outcome, 0, 1)
			exec := outcomeExecution(&outcomes)
			exec.input.CompletionContract = CompletionContractReviewerMarker
			exec.reportOutcome("completed", "parsed", `{"outcome":"`+outcome+`","summary":"review complete"}`, "")
			if len(outcomes) != 1 || !outcomes[0].Succeeded {
				t.Fatalf("reviewer outcome %q = %#v, want one successful outcome", outcome, outcomes)
			}
		})
	}

	outcomes := make([]Outcome, 0, 1)
	exec := outcomeExecution(&outcomes)
	exec.input.CompletionContract = CompletionContractReviewerMarker
	exec.reportOutcome("completed", "parsed", `{"outcome":"mostly-done","summary":"invalid review outcome"}`, "")
	if len(outcomes) != 1 || outcomes[0].Succeeded {
		t.Fatalf("invalid reviewer outcome = %#v, want one failed outcome", outcomes)
	}
}
