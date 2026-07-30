package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestResolveHermesArgsUsesScriptedOneShot(t *testing.T) {
	t.Parallel()
	model := "nous/llama"
	command, args := ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorHermes, Model: &model}, "/tmp/looper-worktree", "fix the issue")
	if command != "hermes" {
		t.Fatalf("command = %q, want hermes", command)
	}
	if want := []string{"--model", model, "-z", "fix the issue"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestResolveHermesArgsPreservesExplicitOneShotContract(t *testing.T) {
	t.Parallel()
	_, args := ResolveSpawn(ExecutorConfig{Vendor: config.AgentVendorHermes, Params: map[string]any{"args": []any{"-z", "operator prompt"}}}, "/tmp/looper-worktree", "ignored")
	if want := []string{"-z", "operator prompt"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestHermesFailsClosedWhenRestrictedToolNetworkIsRequired(t *testing.T) {
	t.Parallel()
	executor := New(ExecutorOptions{Config: ExecutorConfig{Vendor: config.AgentVendorHermes}})
	_, err := executor.Start(context.Background(), RunInput{Prompt: "hello", WorkingDirectory: t.TempDir(), RestrictToolNetwork: true})
	if err == nil || err.Error() != "tool-network restriction is supported only for codex; refusing validation-gated hermes execution" {
		t.Fatalf("restricted Hermes Start() error = %v", err)
	}
}
