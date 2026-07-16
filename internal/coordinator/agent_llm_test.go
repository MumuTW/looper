package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/triage"
)

func TestAgentLLMRejectsOutputFromFailedExecution(t *testing.T) {
	executor := agent.New(agent.ExecutorOptions{Config: agent.ExecutorConfig{
		Vendor: config.AgentVendor("custom"),
		Params: map[string]any{
			"command": "/bin/sh",
			"args":    []any{"-c", `printf '{"summary":"looks valid"}\n'; exit 7`},
		},
	}})
	llm := NewAgentLLM(executor, time.Now, time.Second, time.Second)

	_, err := llm.Complete(context.Background(), triage.Request{
		Prompt:           "triage this issue",
		WorkingDirectory: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Complete() error = nil, want failed execution rejection")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("Complete() error = %q, want failed status", err)
	}
}
