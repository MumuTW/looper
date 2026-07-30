package fixer

import (
	"testing"

	"github.com/nexu-io/looper/internal/agent"
)

// The fixer treats the agent's declared `outcome` as the authority for whether a
// repair completed. These tests pin that authority: a parseable marker is not
// enough, the outcome must be declared and recognized, and a blocked repair must
// name a failure kind from the set the prompt offers.

func TestFixerRepairTaskOutcomeUsesStructuredAuthority(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		result      AgentResult
		wantBlocked bool
		wantKind    QueueFailureKind
		wantErr     string
	}{
		{
			name:   "completed outcome authorizes advancing",
			result: AgentResult{Status: "completed", CompletionPayload: `{"outcome":"completed","summary":"applied fixes"}`},
		},
		{
			name:        "blocked outcome carries the declared kind",
			result:      AgentResult{Status: "completed", CompletionPayload: `{"outcome":"blocked","failure_kind":"manual_intervention","summary":"needs a human"}`},
			wantBlocked: true,
			wantKind:    FailureManualIntervention,
		},
		{
			name:    "no payload at all",
			result:  AgentResult{Status: "completed"},
			wantErr: "Fixer agent completed without required structured outcome",
		},
		{
			name:    "unparseable payload",
			result:  AgentResult{Status: "completed", CompletionPayload: `{not json`},
			wantErr: "Fixer agent completed with invalid structured outcome",
		},
		{
			name:    "summary only, no outcome declared",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"summary":"applied fixes"}`},
			wantErr: "Fixer agent completed with missing or unrecognized structured outcome",
		},
		{
			name:    "unrecognized outcome",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"outcome":"mostly-done","summary":"applied fixes"}`},
			wantErr: "Fixer agent completed with missing or unrecognized structured outcome",
		},
		{
			name:    "blocked without a failure kind",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"outcome":"blocked","summary":"gave up"}`},
			wantErr: "Fixer blocked outcome requires a valid failure_kind",
		},
		{
			name:    "blocked with an unsupported failure kind",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"outcome":"blocked","failure_kind":"non_retryable","summary":"gave up"}`},
			wantErr: "Fixer blocked outcome requires a valid failure_kind",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			blocked, message, kind, err := fixerRepairTaskOutcome(testCase.result)
			if testCase.wantErr != "" {
				if err == nil || err.message != testCase.wantErr {
					t.Fatalf("err = %v, want %q", err, testCase.wantErr)
				}
				if err.kind != FailureRetryableTransient {
					t.Fatalf("err.kind = %q, want retryable_transient", err.kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if blocked != testCase.wantBlocked {
				t.Fatalf("blocked = %v, want %v", blocked, testCase.wantBlocked)
			}
			if testCase.wantBlocked {
				if kind != testCase.wantKind {
					t.Fatalf("kind = %q, want %q", kind, testCase.wantKind)
				}
				if message != "needs a human" {
					t.Fatalf("message = %q, want the agent's summary", message)
				}
			}
		})
	}
}

// TestFixerRepairTaskOutcomeFallsBackToTranscript covers the adapters and the
// checkpoint fallback path, which do not carry the parsed payload. The outcome has
// to be recoverable from the transcript, including the Codex --json form where the
// marker is embedded in a JSON event rather than printed on a stdout line.
func TestFixerRepairTaskOutcomeFallsBackToTranscript(t *testing.T) {
	t.Parallel()

	t.Run("plain stdout marker", func(t *testing.T) {
		t.Parallel()
		result := AgentResult{Status: "completed", Stdout: "working...\n" + agent.CompletionMarkerPrefix + `{"outcome":"completed","summary":"applied fixes"}`}
		if _, _, _, err := fixerRepairTaskOutcome(result); err != nil {
			t.Fatalf("err = %v, want the stdout marker to satisfy the contract", err)
		}
	})

	t.Run("codex jsonl embedded marker", func(t *testing.T) {
		t.Parallel()
		jsonl := `{"type":"item.completed","item":{"type":"agent_message","text":"__LOOPER_RESULT__={\"outcome\":\"blocked\",\"failure_kind\":\"retryable_after_resume\",\"summary\":\"remote head moved\"}"}}`
		blocked, message, kind, err := fixerRepairTaskOutcome(AgentResult{Status: "completed", Stdout: jsonl})
		if err != nil {
			t.Fatalf("err = %v, want the JSONL-embedded marker to be translated", err)
		}
		if !blocked || kind != FailureRetryableAfterResume || message != "remote head moved" {
			t.Fatalf("(blocked, kind, message) = (%v, %q, %q), want the declared block", blocked, kind, message)
		}
	})
}

// TestIsTemplateCompletionPayloadSkipsEchoedFixerTemplate guards the interaction
// between the fixer prompt and template detection. The fixer template carries
// outcome/failure_kind alongside the placeholder summary, so a shape check keyed on
// a single "summary" key would treat an echoed template as a real completion.
func TestIsTemplateCompletionPayloadSkipsEchoedFixerTemplate(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		`{"summary":"<one-sentence summary>"}`,
		`{"outcome":"completed","summary":"<one-sentence summary>"}`,
		`{"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`,
	} {
		if !isTemplateCompletionPayload(payload) {
			t.Fatalf("isTemplateCompletionPayload(%s) = false, want the echoed template skipped", payload)
		}
	}
	for _, payload := range []string{
		`{"outcome":"completed","summary":"applied fixes"}`,
		`{"summary":"applied fixes"}`,
	} {
		if isTemplateCompletionPayload(payload) {
			t.Fatalf("isTemplateCompletionPayload(%s) = true, want a real completion kept", payload)
		}
	}
}
