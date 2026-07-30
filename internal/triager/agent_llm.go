package triager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/eventlog"
)

type agentLLM struct {
	executor    *agent.ConfiguredExecutor
	timeout     time.Duration
	idleTimeout time.Duration
}

func NewAgentLLM(executor *agent.ConfiguredExecutor, timeout, idleTimeout time.Duration) LLM {
	return agentLLM{executor: executor, timeout: timeout, idleTimeout: idleTimeout}
}

func (l agentLLM) Complete(ctx context.Context, req Request) (string, error) {
	if l.executor == nil {
		return "", fmt.Errorf("triager executor is not configured")
	}
	if strings.TrimSpace(req.WorkingDirectory) == "" {
		return "", fmt.Errorf("triager working directory is required")
	}
	execution, err := l.executor.Start(ctx, agent.RunInput{
		ExecutionID:      eventlog.NewEventID("triager"),
		Prompt:           req.Prompt,
		WorkingDirectory: req.WorkingDirectory,
		Timeout:          l.timeout,
		HeartbeatTimeout: l.idleTimeout,
	})
	if err != nil {
		return "", err
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return "", err
	}
	return completedAgentMessage(result)
}

func completedAgentMessage(result agent.Result) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(result.Status), "completed") {
		return "", fmt.Errorf("triager execution ended with status %q: %s", result.Status, strings.TrimSpace(result.Summary))
	}
	message := agent.FinalMessage(result.Stdout)
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("triager returned empty stdout")
	}
	return strings.TrimSpace(message), nil
}
