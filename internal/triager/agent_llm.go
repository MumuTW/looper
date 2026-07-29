package triager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/eventlog"
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
	if strings.TrimSpace(result.Stdout) == "" {
		return "", fmt.Errorf("triager returned empty stdout")
	}
	return strings.TrimSpace(result.Stdout), nil
}
