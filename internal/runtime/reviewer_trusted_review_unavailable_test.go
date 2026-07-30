package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/reviewer"
)

// nativeReviewAdapterConfig is a project that publishes native reviews, so a
// review-phase run here is one that would normally receive a proxy socket.
func nativeReviewAdapterConfig(workDir string) *config.Config {
	return &config.Config{
		Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindGitHub, BaseURL: "https://ghe.example.test", TokenEnv: stringPtr("GHES_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "ghes-native", Name: "GHES", Provider: "fj", Repo: "acme/looper", RepoPath: workDir}},
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSingleReview}}},
	}
}

func nativeReviewAdapterInput(workDir string) reviewer.AgentRunInput {
	return reviewer.AgentRunInput{
		ExecutionID:      "reviewer_wrapper_unavailable",
		ProjectID:        "ghes-native",
		RunID:            "run_reviewer",
		WorkingDirectory: workDir,
		Prompt:           "review",
		Timeout:          5 * time.Second,
		Metadata: map[string]any{
			"phase":               "review",
			"repo":                "acme/looper",
			"prNumber":            int64(42),
			"cleanReviewEvent":    "APPROVE",
			"blockingReviewEvent": "REQUEST_CHANGES",
			"expectedCommitID":    "head-42",
			"reviewerManual":      false,
			"reviewerRunID":       "run_reviewer",
		},
	}
}

// A native-review run without a usable wrapper is refused before the agent
// starts, so no run is spent reviewing a PR it cannot publish to.
//
// The message matters as much as the refusal. Runs that never mint a proxy —
// comment-only publish, thread-resolution classifiers — report this same
// condition from the agent side, quoting the prompt. Asserting equality with
// reviewer.TrustedWrapperUnavailableMessage is what keeps one phrase covering
// both surfaces, so an operator finds every affected run with one search
// instead of learning which half produced which wording.
func TestReviewerAgentStartRefusesWithThePromptsWordingWhenWrapperUnavailable(t *testing.T) {
	workDir := t.TempDir()
	customVendor := config.AgentVendor("custom")
	adapter := reviewerAgentExecutorAdapter{
		executor: agent.New(agent.ExecutorOptions{
			Config:            agent.ExecutorConfig{Vendor: customVendor, Params: map[string]any{"command": "/bin/true"}},
			ParamsOwnerVendor: &customVendor,
		}),
		// Empty because the capability probe rejected the binary, or none was
		// configured at all — the same value the prompt was built from.
		realLooper: "",
		trustedEnv: map[string]string{"PROVIDER_TOKEN": "test-token"},
		config:     nativeReviewAdapterConfig(workDir),
	}

	_, err := adapter.Start(context.Background(), nativeReviewAdapterInput(workDir))
	if err == nil {
		t.Fatal("Start() error = nil, want refusal before the agent runs")
	}
	if err.Error() != reviewer.TrustedWrapperUnavailableMessage {
		t.Fatalf("Start() error = %q, want exactly %q so the daemon and the agent report this condition identically", err, reviewer.TrustedWrapperUnavailableMessage)
	}
}

// Once a usable wrapper exists, a mint failure is the daemon's own — socket,
// binding or policy — and must still abort. Continuing would publish a review
// run that blames a missing wrapper for a daemon bug.
func TestReviewerAgentStartAbortsOnDaemonSideMintFailure(t *testing.T) {
	scriptDir := t.TempDir()
	realLooper := filepath.Join(scriptDir, "real-looper")
	if err := os.WriteFile(realLooper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(realLooper) error = %v", err)
	}
	customVendor := config.AgentVendor("custom")
	adapter := reviewerAgentExecutorAdapter{
		executor: agent.New(agent.ExecutorOptions{
			Config:            agent.ExecutorConfig{Vendor: customVendor, Params: map[string]any{"command": "/bin/true"}},
			ParamsOwnerVendor: &customVendor,
		}),
		realLooper: realLooper,
		config:     nativeReviewAdapterConfig(t.TempDir()),
	}

	// An empty working directory fails normalizeTrustedReviewCwd, which is a
	// daemon-selection fault rather than a missing wrapper.
	input := nativeReviewAdapterInput("")
	input.WorkingDirectory = ""

	if _, err := adapter.Start(context.Background(), input); err == nil {
		t.Fatal("Start() error = nil, wanted the run to abort on a daemon-side proxy failure")
	}
}
