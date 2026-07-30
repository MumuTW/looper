package fixer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

// Cross-component contract: every runner's loop-metadata merge decodes through
// the shared strict decoder, so a malformed stored value blocks the merge
// instead of being replaced with only the updates.
func TestMergeLoopMetadataJSONRejectsMalformedCurrentValue(t *testing.T) {
	t.Parallel()

	malformed := `{"issueUrl":`
	got, err := mergeLoopMetadataJSON(&malformed, map[string]any{"issueUrl": "https://example.com/issues/1"})
	if err == nil {
		t.Fatalf("mergeLoopMetadataJSON(malformed) = %q, want error", got)
	}
	if !errors.Is(err, loops.ErrMalformedLoopMetadata) {
		t.Fatalf("mergeLoopMetadataJSON(malformed) error = %v, want ErrMalformedLoopMetadata", err)
	}

	valid := `{"worktreeId":"wt-1"}`
	out, err := mergeLoopMetadataJSON(&valid, map[string]any{"issueUrl": "https://example.com/issues/1"})
	if err != nil {
		t.Fatalf("mergeLoopMetadataJSON(valid) error = %v", err)
	}
	if !strings.Contains(out, `"worktreeId"`) || !strings.Contains(out, `"issueUrl"`) {
		t.Fatalf("mergeLoopMetadataJSON(valid) = %q, want existing keys preserved and update applied", out)
	}
}

func TestRunPushStepRejectsMalformedMetadataBeforePush(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AllowAutoPush: true, Now: fixture.now, Logger: fixture.logger})
	malformed := `{"fixEvidence":`

	_, err := runner.runPushStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Loop:    storage.LoopRecord{ID: "loop_1", MetadataJSON: &malformed},
		Checkpoint: fixerCheckpoint{
			Worktree: &checkpointWorktree{Path: t.TempDir(), Branch: "feature/fix", BaseHeadSHA: "base"},
		},
	})
	if !errors.Is(err, loops.ErrMalformedLoopMetadata) {
		t.Fatalf("runPushStep() error = %v, want ErrMalformedLoopMetadata", err)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("Push calls = %#v, want none before metadata preflight", git.pushCalls)
	}
}
