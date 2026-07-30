package projects

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestServiceDiscoverProjectCancellationDoesNotWaitForSameProjectLock(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()

	service := &Service{
		DB:                coordinator.DB(),
		Repos:             repos,
		ScheduleDiscovery: func(func()) {},
		ListWorktrees: func(ctx context.Context, _ string) ([]WorktreeListEntry, error) {
			close(firstStarted)
			select {
			case <-releaseFirst:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
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
		t.Fatal("first discovery did not acquire the project lock")
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := service.DiscoverProject(canceledCtx, DiscoverInput{ProjectID: "looper"})
		secondDone <- err
	}()
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled DiscoverProject() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled discovery waited for the first same-project discovery")
	}

	release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first DiscoverProject() error = %v", err)
	}
}

type syncSnapshotGateQuerier struct {
	db      *sql.DB
	opened  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (q *syncSnapshotGateQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q *syncSnapshotGateQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil || !strings.Contains(query, "SELECT * FROM projects") {
		return rows, err
	}
	q.once.Do(func() {
		close(q.opened)
		<-q.release
	})
	return rows, nil
}

func (q *syncSnapshotGateQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func TestServiceSyncConfiguredKeepsDiscoveryStateWrittenWhileWaitingForProjectLock(t *testing.T) {
	coordinator := openCoordinator(t)
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	nowISO := currentISO(func() time.Time { return now })
	baseBranch := "main"
	metadata := `{"registrationDiscovery":{"status":"pending","snapshotMode":"off","updatedAt":"` + nowISO + `"},"repo":null,"worktreeRoot":null,"source":"config"}`
	seedRepos := storage.NewRepositories(coordinator.DB())
	if err := seedRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	gate := &syncSnapshotGateQuerier{db: coordinator.DB(), opened: make(chan struct{}), release: make(chan struct{})}
	service := &Service{
		DB:                coordinator.DB(),
		Repos:             storage.NewRepositories(gate),
		Now:               func() time.Time { return now },
		ScheduleDiscovery: func(func()) {},
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
			return nil, nil
		},
	}
	cfg := config.Config{Projects: []config.ProjectRefConfig{{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch}}}
	syncDone := make(chan error, 1)
	go func() { syncDone <- service.SyncConfigured(context.Background(), cfg, now) }()
	select {
	case <-gate.opened:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncConfigured() did not read its initial project snapshot")
	}

	discovered, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper", SnapshotMode: SnapshotModeOff})
	if err != nil {
		t.Fatalf("DiscoverProject() error = %v", err)
	}
	if discovered.Discovery.Status != DiscoveryStatusSucceeded {
		t.Fatalf("DiscoverProject() state = %#v, want succeeded", discovered.Discovery)
	}
	close(gate.release)
	if err := <-syncDone; err != nil {
		t.Fatalf("SyncConfigured() error = %v", err)
	}
	stored, err := seedRepos.Projects.GetByID(context.Background(), "looper")
	if err != nil || stored == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", stored, err)
	}
	if got := DiscoveryStateFromRecord(*stored); got.Status != DiscoveryStatusSucceeded {
		t.Fatalf("discovery state after sync = %#v, want completed discovery preserved", got)
	}
}

func TestServiceUpdateProjectRereadsMetadataAfterProjectLock(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	nowISO := currentISO(func() time.Time { return now })
	baseBranch := "main"
	before := `{"repo":null,"source":"api","registrationDiscovery":{"status":"running","snapshotMode":"off"}}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch, MetadataJSON: &before, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	service := &Service{DB: coordinator.DB(), Repos: repos, Now: func() time.Time { return now }, ScheduleDiscovery: func(func()) {}}
	unlock, err := service.lockProjectOperations(context.Background(), "looper")
	if err != nil {
		t.Fatalf("lockProjectOperations() error = %v", err)
	}
	updated := make(chan error, 1)
	repo := "acme/looper"
	go func() {
		_, err := service.UpdateProject(context.Background(), "looper", UpdateInput{Repo: UpdateStringField{Set: true, Value: &repo}})
		updated <- err
	}()

	// This models a discovery completion that writes while PATCH is waiting for
	// the keyed project lock. A whole-record update must not put this metadata
	// back to the stale pre-lock snapshot.
	during := `{"repo":null,"source":"api","concurrentDiscovery":"preserved","registrationDiscovery":{"status":"succeeded","snapshotMode":"off"}}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: &baseBranch, MetadataJSON: &during, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(concurrent discovery) error = %v", err)
	}
	unlock()
	if err := <-updated; err != nil {
		t.Fatalf("UpdateProject() error = %v", err)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil || stored == nil {
		t.Fatalf("Projects.GetByID() = %#v, %v", stored, err)
	}
	metadata := parseMetadata(stored.MetadataJSON)
	if metadata["concurrentDiscovery"] != "preserved" {
		t.Fatalf("metadata = %#v, want concurrent discovery metadata preserved", metadata)
	}
	if metadataString(metadata, "repo") != repo {
		t.Fatalf("metadata repo = %q, want %q", metadataString(metadata, "repo"), repo)
	}
}

func TestWriteDiscoveryStatePreservesCurrentProjectMetadata(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	oldMetadata := `{"source":"old"}`
	currentMetadata := `{"source":"current","concurrent":"preserved"}`
	nowISO := "2026-07-30T00:00:00.000Z"
	project := storage.ProjectRecord{ID: "looper", MetadataJSON: &oldMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	current := project
	current.MetadataJSON = &currentMetadata
	current.UpdatedAt = "2026-07-30T01:00:00.000Z"
	if err := repos.Projects.Upsert(context.Background(), current); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	service := &Service{Repos: repos}

	if err := service.writeDiscoveryState(context.Background(), &project, DiscoveryState{Status: DiscoveryStatusSucceeded, UpdatedAt: "2026-07-30T02:00:00.000Z"}); err != nil {
		t.Fatalf("writeDiscoveryState() error = %v", err)
	}
	stored, err := repos.Projects.GetByID(context.Background(), "looper")
	if err != nil || stored == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", stored, err)
	}
	metadata := parseMetadata(stored.MetadataJSON)
	if metadata["source"] != "current" || metadata["concurrent"] != "preserved" {
		t.Fatalf("metadata = %#v, want current non-discovery fields preserved", metadata)
	}
	if got := DiscoveryStateFromRecord(*stored).Status; got != DiscoveryStatusSucceeded {
		t.Fatalf("discovery status = %q, want succeeded", got)
	}
}

type failSecondWorktreeUpsertQuerier struct {
	db       *sql.DB
	mu       sync.Mutex
	attempts int
	fail     bool
}

func (q *failSecondWorktreeUpsertQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "INSERT INTO worktrees") {
		q.mu.Lock()
		q.attempts++
		shouldFail := q.fail && q.attempts == 2
		q.mu.Unlock()
		if shouldFail {
			return nil, errors.New("second worktree upsert failed")
		}
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failSecondWorktreeUpsertQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failSecondWorktreeUpsertQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func TestServiceDiscoverProjectRetainsPartialCountsAfterWorktreeWriteFailure(t *testing.T) {
	coordinator := openCoordinator(t)
	gate := &failSecondWorktreeUpsertQuerier{db: coordinator.DB(), fail: true}
	repos := storage.NewRepositories(gate)
	service := &Service{
		DB:                coordinator.DB(),
		Repos:             repos,
		ScheduleDiscovery: func(func()) {},
		ListWorktrees: func(context.Context, string) ([]WorktreeListEntry, error) {
			return []WorktreeListEntry{
				{Path: "/tmp/looper-main", Branch: "main", HeadSHA: "aaa"},
				{Path: "/tmp/looper-feature", Branch: "feature", HeadSHA: "bbb"},
			}, nil
		},
	}
	if _, err := service.AddProject(context.Background(), AddInput{ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: "main"}); err != nil {
		t.Fatalf("AddProject() error = %v", err)
	}

	failed, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper", SnapshotMode: SnapshotModeOff})
	if err == nil || !strings.Contains(err.Error(), "second worktree upsert failed") {
		t.Fatalf("DiscoverProject() error = %v, want partial write failure", err)
	}
	if failed.Discovery.Status != DiscoveryStatusFailed || failed.Discovery.DiscoveredWorktrees != 1 {
		t.Fatalf("failed discovery = %#v, want failed state with one persisted worktree", failed.Discovery)
	}
	items, err := repos.Worktrees.ListByProject(context.Background(), "looper")
	if err != nil {
		t.Fatalf("Worktrees.ListByProject() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("worktrees after partial failure = %#v, want one", items)
	}

	gate.mu.Lock()
	gate.fail = false
	gate.mu.Unlock()
	retried, err := service.DiscoverProject(context.Background(), DiscoverInput{ProjectID: "looper", SnapshotMode: SnapshotModeOff})
	if err != nil {
		t.Fatalf("DiscoverProject() retry error = %v", err)
	}
	if retried.Discovery.Status != DiscoveryStatusSucceeded || retried.Discovery.DiscoveredWorktrees != 2 {
		t.Fatalf("retried discovery = %#v, want succeeded state with two worktrees", retried.Discovery)
	}
}
