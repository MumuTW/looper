package triager

import (
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/agent"
)

func TestCompletedAgentMessageRejectsFailedExecution(t *testing.T) {
	_, err := completedAgentMessage(agent.Result{Status: "timeout", Summary: "idle timeout", Stdout: eligibleDecisionJSON()})
	if err == nil || !strings.Contains(err.Error(), `status "timeout"`) {
		t.Fatalf("completedAgentMessage() error = %v", err)
	}
}

func TestCompletedAgentMessageUsesCodexFinalMessage(t *testing.T) {
	stdout := "{\"type\":\"thread.started\",\"thread_id\":\"th_1\"}\n" +
		"{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":" + quotedJSON(eligibleDecisionJSON()) + "}}\n" +
		"{\"type\":\"turn.completed\"}"
	got, err := completedAgentMessage(agent.Result{Status: "completed", Stdout: stdout})
	if err != nil {
		t.Fatalf("completedAgentMessage() error = %v", err)
	}
	if got != eligibleDecisionJSON() {
		t.Fatalf("completedAgentMessage() = %q", got)
	}
}

func TestValidateDecisionOutputUsesStrictRunnerSchema(t *testing.T) {
	if !ValidateDecisionOutput(eligibleDecisionJSON()) {
		t.Fatal("ValidateDecisionOutput() = false, want true for a complete decision")
	}
	if ValidateDecisionOutput(`{"unexpected":true}`) {
		t.Fatal("ValidateDecisionOutput() = true, want false for an unknown field")
	}
	if ValidateDecisionOutput(eligibleDecisionJSON() + " trailing") {
		t.Fatal("ValidateDecisionOutput() = true, want false for trailing content")
	}
}

func quotedJSON(value string) string {
	quoted := strings.ReplaceAll(value, `\`, `\\`)
	quoted = strings.ReplaceAll(quoted, `"`, `\"`)
	return `"` + quoted + `"`
}
