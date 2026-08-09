package reviewer

import "testing"

func TestParseReviewerThreadResolutionOutputUsesCodexFinalMessage(t *testing.T) {
	stdout := `{"type":"thread.started","thread_id":"th_1"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"decisions\":[{\"threadId\":\"thread_1\",\"decision\":\"objectively_fixed\",\"evidence\":\"nil check\",\"confidence\":\"high\"}]}"}}
{"type":"turn.completed"}`

	decisions, err := parseReviewerThreadResolutionOutput(stdout)
	if err != nil {
		t.Fatalf("parseReviewerThreadResolutionOutput() error = %v", err)
	}
	if len(decisions) != 1 || decisions[0].ThreadID != "thread_1" || decisions[0].Decision != "objectively_fixed" {
		t.Fatalf("decisions = %#v, want final assistant decision", decisions)
	}
}
