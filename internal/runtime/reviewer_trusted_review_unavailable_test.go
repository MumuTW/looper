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
		Providers: []config.ProviderConfig{{ID: "fj", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "forgejo-native", Name: "Forgejo", Provider: "fj", Repo: "acme/looper", RepoPath: workDir}},
		Roles:     config.RoleConfigs{Reviewer: config.ReviewerRoleConfig{Behavior: config.ReviewerConfig{PublishMode: config.ReviewerPublishModeSingleReview}}},
	}
}

func nativeReviewAdapterInput(workDir string) reviewer.AgentRunInput {
	return reviewer.AgentRunInput{
		ExecutionID:      "reviewer_wrapper_unavailable",
		ProjectID:        "forgejo-native",
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

// When no usable wrapper exists, resolveTrustedLooperCLIPath hands the reviewer
// prompt an empty path, which makes the prompt instruct the agent to fail
// closed with `trusted looper review submit wrapper unavailable`. The agent has
// to actually run to deliver that, so Start must not abort trying to mint a
// proxy it already knows cannot work — otherwise the reviewer's one
// well-defined unavailable path is reachable on comment-only runs but not on
// the native-review runs it was written for.
func TestReviewerAgentStartsWithoutSocketWhenWrapperUnavailable(t *testing.T) {
	workDir := t.TempDir()
	scriptDir := t.TempDir()
	outputPath := filepath.Join(scriptDir, "child.env")
	scriptPath := filepath.Join(scriptDir, "dump-env")
	script := "#!/bin/sh\nif [ -n \"$LOOPER_TRUSTED_REVIEW_SOCK\" ]; then printf 'sock=set\\n' > \"" + outputPath + "\"; else printf 'sock=\\n' > \"" + outputPath + "\"; fi\nprintf '__LOOPER_RESULT__={\"summary\":\"done\"}\\n'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(scriptPath) error = %v", err)
	}

	customVendor := config.AgentVendor("custom")
	adapter := reviewerAgentExecutorAdapter{
		executor: agent.New(agent.ExecutorOptions{
			Config:            agent.ExecutorConfig{Vendor: customVendor, Params: map[string]any{"command": scriptPath}},
			ParamsOwnerVendor: &customVendor,
		}),
		// Empty because the capability probe rejected the binary, or none was
		// configured at all — the same value the prompt was built from.
		realLooper: "",
		trustedEnv: map[string]string{"FORGEJO_TOKEN": "test-token"},
		config:     nativeReviewAdapterConfig(workDir),
	}

	execHandle, err := adapter.Start(context.Background(), nativeReviewAdapterInput(workDir))
	if err != nil {
		t.Fatalf("Start() error = %v; want the run to proceed so the agent can report the wrapper as unavailable", err)
	}
	result, err := execHandle.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("result.Status = %q stderr=%q, want completed", result.Status, result.Stderr)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(outputPath) error = %v", err)
	}
	// Publish capability is the thing that must not appear. Skipping the mint
	// grants nothing; it leaves the agent exactly where a comment-only run is.
	if string(data) != "sock=\n" {
		t.Fatalf("child env dump = %q, want no trusted review socket", string(data))
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
