package coordinator

import "testing"

func TestCoordinatorAgentCompletionUsesCodexFinalMessage(t *testing.T) {
	stdout := `{"type":"thread.started","thread_id":"th_1"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"disposition\":\"valid\",\"comment\":\"looks good\"}"}}
{"type":"turn.completed"}`

	message, err := coordinatorAgentMessage(stdout)
	if err != nil {
		t.Fatalf("coordinatorAgentMessage() error = %v", err)
	}
	if message != `{"disposition":"valid","comment":"looks good"}` {
		t.Fatalf("FinalMessage() = %q, want coordinator payload", message)
	}
}
