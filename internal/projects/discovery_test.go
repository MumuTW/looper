package projects

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceAddProjectDoesNotWaitForOtherProjectDiscovery(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	discoveryStarted := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	var scheduled sync.Once
	var discoveries sync.WaitGroup
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		ListWorktrees: func(_ context.Context, repoPath string) ([]WorktreeListEntry, error) {
			if repoPath == "/tmp/project-a" {
				close(discoveryStarted)
				<-releaseDiscovery
			}
			return nil, nil
		},
		ScheduleDiscovery: func(run func()) {
			scheduled.Do(func() {
				discoveries.Add(1)
				go func() {
					defer discoveries.Done()
					run()
				}()
			})
		},
	}
	t.Cleanup(func() {
		close(releaseDiscovery)
		discoveries.Wait()
	})

	if _, err := service.AddProject(context.Background(), AddInput{ID: "project-a", Name: "Project A", RepoPath: "/tmp/project-a", BaseBranch: "main"}); err != nil {
		t.Fatalf("AddProject(project A) error = %v", err)
	}
	select {
	case <-discoveryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("project A discovery did not start")
	}

	added := make(chan error, 1)
	go func() {
		_, err := service.AddProject(context.Background(), AddInput{ID: "project-b", Name: "Project B", RepoPath: "/tmp/project-b", BaseBranch: "main"})
		added <- err
	}()
	select {
	case err := <-added:
		if err != nil {
			t.Fatalf("AddProject(project B) error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("project B registration waited for project A discovery")
	}
}

// A bare Service has no lifecycle owner for asynchronous work. Registration
// must still commit and return pending, but it must not leave a goroutine using
// SQLite after its caller tears the service down.
func TestServiceAddProjectWithoutLifecycleOwnerLeavesDiscoveryPendingAndCanCloseStorage(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	discoveryStarted := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	t.Cleanup(func() { close(releaseDiscovery) })
	service := &Service{
		DB:    coordinator.DB(),
		Repos: repos,
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
			close(discoveryStarted)
			<-releaseDiscovery
			return nil, nil
		},
	}

	added, err := service.AddProject(context.Background(), AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}
	if added.Discovery.Status != DiscoveryStatusPending {
		t.Fatalf("AddProject().Discovery.Status = %q, want pending", added.Discovery.Status)
	}
	select {
	case <-discoveryStarted:
		t.Fatal("AddProject() started unowned post-commit discovery")
	case <-time.After(100 * time.Millisecond):
	}
	stored, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil || stored == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want registered project", stored, err)
	}
	if got := DiscoveryStateFromRecord(*stored); got.Status != DiscoveryStatusPending {
		t.Fatalf("stored discovery = %#v, want pending without lifecycle owner", got)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}
}

func TestServiceDiscoverProjectSerializesSameProjectRetries(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()

	var callsMu sync.Mutex
	calls := 0
	service := &Service{
		DB:                coordinator.DB(),
		Repos:             repos,
		ScheduleDiscovery: func(func()) {},
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
			callsMu.Lock()
			calls++
			call := calls
			callsMu.Unlock()
			switch call {
			case 1:
				close(firstStarted)
				<-releaseFirst
			case 2:
				close(secondStarted)
			}
			return nil, nil
		},
	}
	if _, err := service.AddProject(context.Background(), AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main"}); err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper"})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first discovery did not start")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper"})
		secondDone <- err
	}()
	select {
	case <-secondStarted:
		t.Fatal("same-project retry entered discovery before the first run finished")
	case <-time.After(150 * time.Millisecond):
	}

	release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first DiscoverProject() error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second DiscoverProject() error = %v", err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 2 {
		t.Fatalf("ListWorktrees calls = %d, want 2 serialized retries", calls)
	}
}

func TestServiceProjectOperationLocksAreEvictedAfterUse(t *testing.T) {
	service := &Service{}
	for _, id := range []string{"missing-a", "missing-b", "missing-c"} {
		unlock, err := service.lockProjectOperations(context.Background(), id)
		if err != nil {
			t.Fatalf("lockProjectOperations(%q) error = %v", id, err)
		}
		unlock()
	}
	service.projectLocksMu.Lock()
	defer service.projectLocksMu.Unlock()
	if len(service.projectLocks) != 0 {
		t.Fatalf("projectLocks retained %d entries after release", len(service.projectLocks))
	}
}

func TestServiceResumeRunningDiscoveriesReschedulesPersistedWork(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	nowISO := "2026-07-30T00:00:00.000Z"
	runningMetadata := `{"registrationDiscovery":{"status":"running","snapshotMode":"full"}}`
	pendingMetadata := `{"registrationDiscovery":{"status":"pending","snapshotMode":"off"}}`
	succeededMetadata := `{"registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	for _, record := range []storage.ProjectRecord{
		{ID: "running", MetadataJSON: &runningMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "pending", MetadataJSON: &pendingMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "succeeded", MetadataJSON: &succeededMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
		{ID: "archived", Archived: true, MetadataJSON: &runningMetadata, CreatedAt: nowISO, UpdatedAt: nowISO},
	} {
		if err := repos.Projects.Upsert(context.Background(), record); err != nil {
			t.Fatalf("Projects.Upsert(%q) error = %v", record.ID, err)
		}
	}
	scheduled := 0
	service := &Service{
		Repos: repos,
		ScheduleDiscovery: func(run func()) {
			scheduled++
			run()
		},
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) { return nil, nil },
	}
	if err := service.ResumeRunningDiscoveries(context.Background()); err != nil {
		t.Fatalf("ResumeRunningDiscoveries() error = %v", err)
	}
	if scheduled != 2 {
		t.Fatalf("scheduled = %d, want persisted pending and running discoveries", scheduled)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "running")
	if err != nil || stored == nil {
		t.Fatalf("Projects.GetByID(running) = (%#v, %v)", stored, err)
	}
	discovery := DiscoveryStateFromRecord(*stored)
	if discovery.Status != DiscoveryStatusSucceeded || discovery.SnapshotMode != SnapshotModeFull {
		t.Fatalf("resumed discovery = %#v, want succeeded full discovery", discovery)
	}
}

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
