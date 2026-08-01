package planner

import (
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestBuildPlannerPromptIncludesPriorFixerFailureContext(t *testing.T) {
	prompt, _ := buildPlannerPrompt(
		storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		customInstructionConfig(nil),
		&checkpointIssue{Repo: "acme/looper", IssueNumber: 7, Title: "Retry", FailureContext: "headSha=head-7; fixItemsFingerprint=abc"},
		&checkpointWorktree{Branch: "looper/planner/7-retry", BaseBranch: "main"},
		false, config.DefaultDisclosureConfig(), "", "",
	)
	for _, want := range []string{"Prior fixer failure context", "headSha=head-7", "fixItemsFingerprint=abc"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("planner prompt missing %q:\n%s", want, prompt)
		}
	}
}
