package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestHermesUsesExactlyOneSchedulerOwnedPrompt(t *testing.T) {
	t.Parallel()
	model := "anthropic/claude-sonnet-4"
	command, args := ResolveSpawn(ExecutorConfig{
		Vendor: config.AgentVendorHermes,
		Model:  &model,
		Params: map[string]any{"args": []any{
			"--provider", "anthropic",
			"-z", "operator replacement",
			"--query=second replacement",
			"--zen", "third replacement",
		}},
	}, "/tmp/worktree", "scheduler task")
	if command != "hermes" {
		t.Fatalf("command = %q, want hermes", command)
	}
	if got := countAny(args, "-z", "--zen", "-q", "--query"); got != 1 {
		t.Fatalf("prompt flags = %d in %#v, want exactly one", got, args)
	}
	if !slices.Equal(args[len(args)-2:], []string{"-z", "scheduler task"}) {
		t.Fatalf("args = %#v, want scheduler prompt last", args)
	}
	if slices.Contains(args, "operator replacement") || slices.Contains(args, "second replacement") || slices.Contains(args, "third replacement") {
		t.Fatalf("args retained an operator replacement prompt: %#v", args)
	}
	if !slices.Contains(args, "--yolo") || !slices.Contains(args, "--model") || !slices.Contains(args, model) {
		t.Fatalf("args = %#v, want normal authorized execution and model override", args)
	}
}

func TestHermesHasNoResumeOrRestrictedAssessmentCapability(t *testing.T) {
	t.Parallel()
	if nativeResumeSupported(config.AgentVendorHermes) || InteractiveTakeoverSupported(config.AgentVendorHermes) {
		t.Fatal("Hermes resume capability must remain false until its session contract is reviewed")
	}
	if _, err := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendorHermes}}).Start(context.Background(), RunInput{
		Prompt: "assess", WorkingDirectory: t.TempDir(), RestrictToolNetwork: true,
	}); err == nil || !strings.Contains(err.Error(), "cannot deny tool network access") {
		t.Fatal("restricted Hermes execution unexpectedly allowed")
	}
}

func countAny(values []string, needles ...string) int {
	count := 0
	for _, value := range values {
		for _, needle := range needles {
			if value == needle || strings.HasPrefix(value, needle+"=") {
				count++
			}
		}
	}
	return count
}
