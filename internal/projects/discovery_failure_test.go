package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/storage"
)

func TestServiceDiscoverProjectRetriesListFailures(t *testing.T) {
	tests := []struct {
		name            string
		fail            func() error
		wantWorktrees   int
		wantPullRequest int
	}{
		{name: "worktree list", fail: func() error { return errors.New("git worktree failed") }, wantPullRequest: 1},
		{name: "pull request list", fail: func() error { return errors.New("gh pr list failed") }, wantWorktrees: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := openCoordinator(t)
			repos := storage.NewRepositories(coordinator.DB())
			shouldFail := true
			service := &Service{
				DB:                coordinator.DB(),
				Repos:             repos,
				ScheduleDiscovery: func(func()) {},
				ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
					if test.name == "worktree list" && shouldFail {
						return nil, test.fail()
					}
					return []WorktreeListEntry{{Path: "/tmp/looper", Branch: "main", HeadSHA: "abc123"}}, nil
				},
				ListOpenPullRequests: func(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
					if test.name == "pull request list" && shouldFail {
						return nil, test.fail()
					}
					return []PullRequestSummary{{Number: 1, State: "OPEN"}}, nil
				},
			}
			repo := "acme/looper"
			if _, err := service.AddProject(context.Background(), AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main", Repo: &repo}); err != nil {
				t.Fatalf("AddProject() error = %v", err)
			}

			failed, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper"})
			if err == nil || !strings.Contains(err.Error(), test.fail().Error()) {
				t.Fatalf("DiscoverProject() error = %v, want %q", err, test.fail())
			}
			if failed.Discovery.Status != DiscoveryStatusFailed || !strings.Contains(failed.Discovery.Error, test.fail().Error()) {
				t.Fatalf("failed discovery = %#v, want failed state with %q", failed.Discovery, test.fail())
			}
			if failed.Discovery.DiscoveredWorktrees != test.wantWorktrees || failed.Discovery.DiscoveredPullRequests != test.wantPullRequest {
				t.Fatalf("failed discovery counts = %#v, want worktrees=%d pullRequests=%d", failed.Discovery, test.wantWorktrees, test.wantPullRequest)
			}
			if len(failed.Discovery.Warnings) != 1 || !strings.Contains(failed.Discovery.Warnings[0], test.fail().Error()) {
				t.Fatalf("failed discovery warnings = %#v, want command warning with %q", failed.Discovery.Warnings, test.fail())
			}
			stored, err := repos.Projects.GetByID(context.Background(), "looper")
			if err != nil || stored == nil {
				t.Fatalf("Projects.GetByID() = (%#v, %v), want stored project", stored, err)
			}
			if got := DiscoveryStateFromRecord(*stored); got.Status != DiscoveryStatusFailed || !strings.Contains(got.Error, test.fail().Error()) {
				t.Fatalf("persisted discovery = %#v, want failed state with %q", got, test.fail())
			}

			shouldFail = false
			retried, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper"})
			if err != nil {
				t.Fatalf("DiscoverProject() retry error = %v", err)
			}
			if retried.Discovery.Status != DiscoveryStatusSucceeded {
				t.Fatalf("retry discovery = %#v, want succeeded", retried.Discovery)
			}
			stored, err = repos.Projects.GetByID(context.Background(), "looper")
			if err != nil || stored == nil {
				t.Fatalf("Projects.GetByID() after retry = (%#v, %v), want stored project", stored, err)
			}
			if got := DiscoveryStateFromRecord(*stored); got.Status != DiscoveryStatusSucceeded {
				t.Fatalf("persisted retry discovery = %#v, want succeeded", got)
			}
		})
	}
}

func TestServiceDiscoverProjectKeepsCancellationPendingForRestart(t *testing.T) {
	t.Parallel()

	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	service := &Service{
		DB:                coordinator.DB(),
		Repos:             repos,
		ScheduleDiscovery: func(func()) {},
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
			return nil, fmt.Errorf("list worktrees: %w", context.Canceled)
		},
	}
	if _, err := service.AddProject(context.Background(), AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main"}); err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}

	result, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscoverProject() error = %v, want context canceled", err)
	}
	if result.Discovery.Status != DiscoveryStatusPending || result.Discovery.Error != "" {
		t.Fatalf("canceled discovery = %#v, want resumable pending state", result.Discovery)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil || stored == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want stored project", stored, err)
	}
	if got := DiscoveryStateFromRecord(*stored); got.Status != DiscoveryStatusPending || got.Error != "" {
		t.Fatalf("persisted canceled discovery = %#v, want resumable pending state", got)
	}
}
