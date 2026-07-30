package worker

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestWorkerPromptRequiresTestFirstWorkflowAndPREvidence(t *testing.T) {
	t.Parallel()

	prompt, err := buildWorkerPrompt(
		t.TempDir(),
		workerInput{Title: "Implement it", Repo: "acme/app", BaseBranch: "main"},
		nil,
		false,
		config.DefaultDisclosureConfig(),
		"codex",
		"",
	)
	if err != nil {
		t.Fatalf("buildWorkerPrompt() error = %v", err)
	}
	for _, phrase := range []string{
		"When feasible, add or update a failing automated test first",
		"smallest implementation that makes it pass",
		"refactor while keeping the suite green",
		"automated coverage added or updated",
		"explain why coverage is not feasible",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("prompt missing %q:\n%s", phrase, prompt)
		}
	}
}

func TestLooperManagedPullRequestBodyHasVerificationEvidenceSection(t *testing.T) {
	t.Parallel()

	body := buildPullRequestBody(
		workerInput{Title: "Implement it", Repo: "acme/app", BaseBranch: "main"},
		&checkpointPlan{Items: []string{"add coverage", "implement behavior"}},
		&checkpointExecution{Summary: "Added a regression test and ran make check."},
	)
	if !strings.Contains(body, "## Verification") || !strings.Contains(body, "Added a regression test and ran make check.") {
		t.Fatalf("PR body lacks verification evidence:\n%s", body)
	}
}
