package planner

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

func TestPersistPlannerPullRequestReferenceDoesNotPartiallyLinkMalformedMetadata(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Now: fixture.now, Logger: fixture.logger})
	malformed := `{"planner":`
	targetID := "issue:acme/looper:42"
	loop := storage.LoopRecord{ID: "loop_planner_malformed", ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &targetID, Status: "running", MetadataJSON: &malformed, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	err := runner.persistPlannerPullRequestReference(context.Background(), stepInput{Loop: loop}, checkpointIssue{Repo: "acme/looper", IssueNumber: 42}, checkpointWorktree{Branch: "planner/42"}, checkpointPullRequest{Number: 77, URL: "https://example.test/pr/77"})
	if !errors.Is(err, loops.ErrMalformedLoopMetadata) {
		t.Fatalf("persistPlannerPullRequestReference() error = %v, want ErrMalformedLoopMetadata", err)
	}
	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || persisted == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", persisted, err)
	}
	if persisted.Repo != nil || persisted.PRNumber != nil || derefString(persisted.MetadataJSON) != malformed {
		t.Fatalf("persisted loop = %#v, want no partial PR linkage and original metadata", persisted)
	}
}
