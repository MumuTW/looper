package planner

import (
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestMergePlannerFailureContextIncludesReplanReason(t *testing.T) {
	got := mergePlannerFailureContext("prior context", map[string]any{
		"replanReason":  "validation failed",
		"workGraphID":   "workgraph_1",
		"failedNodeKey": "storage",
	})
	for _, want := range []string{"prior context", "Work graph replan context", "validation failed", "workgraph_1", "Failed node: storage"} {
		if !strings.Contains(got, want) {
			t.Fatalf("mergePlannerFailureContext() missing %q:\n%s", want, got)
		}
	}
}

func TestBuildPlannerPromptIncludesPriorFixerFailureContext(t *testing.T) {
	prompt, _ := buildPlannerPrompt(
		storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		customInstructionConfig(nil),
		&checkpointIssue{Repo: "acme/looper", IssueNumber: 7, Title: "Retry", FailureContext: "headSha=head-7; fixItemsFingerprint=abc"},
		&checkpointWorktree{Branch: "looper/planner/7-retry", BaseBranch: "main"},
		false, config.DefaultDisclosureConfig(), "", "", true,
	)
	for _, want := range []string{"Prior fixer failure context", "headSha=head-7", "fixItemsFingerprint=abc"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("planner prompt missing %q:\n%s", want, prompt)
		}
	}
}
