package fixer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/storage"
)

func TestDiscoverPullRequestsRejectsHumanTakeoverLoop(t *testing.T) {
	t.Parallel()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "fixer.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-11T12:00:00.000Z"
	baseBranch := "main"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	// Seed a fixer loop in human_takeover status for PR #42
	targetID := "pr:acme/looper:42"
	prNumber := int64(42)
	repo := "acme/looper"
	seedLoop := storage.LoopRecord{
		ID:         "loop_takeover",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "fixer",
		TargetType: "pull_request",
		TargetID:   &targetID,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "human_takeover",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), seedLoop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	runner := New(Options{DB: coordinator.DB(), Repos: repos, GitHub: &fakeGitHubGateway{
		listOpen: []PullRequestSummary{
			{Number: 42, State: "OPEN", HeadSHA: "head-42"},
		},
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-42"},
		},
	}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: &testLogger{}, Now: func() time.Time { return now }})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}

	// No new loop or queue item should be created for PR #42
	for _, id := range result.CreatedLoopIDs {
		loop, err := repos.Loops.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("Loops.GetByID(%s) error = %v", id, err)
		}
		if loop != nil && loop.PRNumber != nil && *loop.PRNumber == 42 {
			t.Fatalf("expected no new loop for PR #42, got %#v", loop)
		}
	}
	if len(result.QueueItems) > 0 {
		for _, item := range result.QueueItems {
			if item.PRNumber != nil && *item.PRNumber == 42 {
				t.Fatalf("expected no new queue item for PR #42, got %#v", item)
			}
		}
	}

	// The original loop should still be in human_takeover status
	existingLoop, err := repos.Loops.GetByID(context.Background(), "loop_takeover")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if existingLoop == nil || existingLoop.Status != "human_takeover" {
		t.Fatalf("expected loop to remain human_takeover, got %#v", existingLoop)
	}
}

func TestDiscoverPullRequestTargetedRejectsHumanTakeoverLoop(t *testing.T) {
	t.Parallel()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "fixer.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	nowISO := "2026-04-11T12:00:00.000Z"
	baseBranch := "main"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	// Seed a fixer loop in human_takeover status for PR #42
	targetID := "pr:acme/looper:42"
	prNumber := int64(42)
	repo := "acme/looper"
	seedLoop := storage.LoopRecord{
		ID:         "loop_takeover",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "fixer",
		TargetType: "pull_request",
		TargetID:   &targetID,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "human_takeover",
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), seedLoop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	runner := New(Options{DB: coordinator.DB(), Repos: repos, GitHub: &fakeGitHubGateway{
		viewResponses: []PullRequestDetail{
			{Number: 42, State: "OPEN", HeadSHA: "head-42"},
		},
	}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: &testLogger{}, Now: func() time.Time { return now }})

	result, err := runner.DiscoverPullRequest(context.Background(), TargetedDiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42})
	if err != nil {
		t.Fatalf("DiscoverPullRequest() error = %v", err)
	}
	if result.Skipped != 1 {
		t.Fatalf("DiscoverPullRequest() skipped = %d, want 1 (human_takeover fence)", result.Skipped)
	}

	// The original loop should still be in human_takeover status
	existingLoop, err := repos.Loops.GetByID(context.Background(), "loop_takeover")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if existingLoop == nil || existingLoop.Status != "human_takeover" {
		t.Fatalf("expected loop to remain human_takeover, got %#v", existingLoop)
	}
}
