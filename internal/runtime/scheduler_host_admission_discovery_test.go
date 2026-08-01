package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/hostresources"
	"github.com/MumuTW/looper/internal/storage"
)

// Host pressure must stop the work-producing discovery lanes, not only claims.
// The Coordinator lane starts agent processes directly during Discover (decide →
// ConfiguredExecutor.Start), bypassing the claim/slot chokepoint, so a hold that
// only withholds claims while discovery keeps launching defeats the guard. This
// is the regression: before the fix, executeClaimPhase returned (0,0,nil) on a
// hold and runDefaultSchedulerTick continued into every discovery lane.
func TestRunDefaultSchedulerTickSkipsDiscoveryLanesUnderHostPressure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "host-pressure.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.August, 1, 10, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)
	item := schedulerTestQueueItem("queue_host_pressure", "worker", nowISO)
	if err := repos.Queue.Upsert(context.Background(), item); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{}
	coordinatorRunner := &stubCoordinatorScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}

	hold := &hostresources.Decision{
		Admit:   false,
		Reasons: []string{hostresources.ReasonDiskLow},
		Detail:  []string{"2.0 GiB free on the state filesystem is below the 10.0 GiB floor"},
	}
	input := defaultSchedulerTickInput{
		Repos:              repos,
		Now:                func() time.Time { return now },
		MaxConcurrentRuns:  1,
		AsyncRunner:        immediateSchedulerRunner{},
		Planner:            plannerRunner,
		Coordinator:        coordinatorRunner,
		CoordinatorEnabled: func(string) bool { return true },
		Reviewer:           reviewerRunner,
		Fixer:              fixerRunner,
		Worker:             workerRunner,
		HostAdmission:      func() *hostresources.Decision { return hold },
	}
	if err := runDefaultSchedulerTick(context.Background(), input); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}

	// No discovery lane may run: each one would start forge work or, for the
	// Coordinator, an agent process on the pressured host.
	if len(plannerRunner.discoverCalls) != 0 {
		t.Fatalf("planner discover calls = %#v, want none under host pressure", plannerRunner.discoverCalls)
	}
	if len(coordinatorRunner.discoverCalls) != 0 {
		t.Fatalf("coordinator discover calls = %#v, want none under host pressure", coordinatorRunner.discoverCalls)
	}
	if len(reviewerRunner.discoverCalls) != 0 {
		t.Fatalf("reviewer discover calls = %#v, want none under host pressure", reviewerRunner.discoverCalls)
	}
	if len(fixerRunner.discoverCalls) != 0 {
		t.Fatalf("fixer discover calls = %#v, want none under host pressure", fixerRunner.discoverCalls)
	}
	if len(workerRunner.discoverCalls) != 0 {
		t.Fatalf("worker discover calls = %#v, want none under host pressure", workerRunner.discoverCalls)
	}
	// Claims are held too: no queued item is processed under pressure.
	if workerRunner.processItemCount() != 0 {
		t.Fatalf("worker processed %d items, want none started under host pressure", workerRunner.processItemCount())
	}
}

// When the host admits, discovery lanes run normally — the hold is a gate, not a
// permanent stop, and the admission path must not regress the discovery flow.
func TestRunDefaultSchedulerTickRunsDiscoveryLanesWhenTheHostAdmits(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "host-admit.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.August, 1, 10, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	plannerRunner := &stubPlannerScheduler{}
	coordinatorRunner := &stubCoordinatorScheduler{}
	input := defaultSchedulerTickInput{
		Repos:              repos,
		Now:                func() time.Time { return now },
		MaxConcurrentRuns:  1,
		AsyncRunner:        immediateSchedulerRunner{},
		Planner:            plannerRunner,
		Coordinator:        coordinatorRunner,
		CoordinatorEnabled: func(string) bool { return true },
		HostAdmission:      func() *hostresources.Decision { return &hostresources.Decision{Admit: true} },
	}
	if err := runDefaultSchedulerTick(context.Background(), input); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if len(plannerRunner.discoverCalls) != 1 {
		t.Fatalf("planner discover calls = %d, want 1 when the host admits", len(plannerRunner.discoverCalls))
	}
	if len(coordinatorRunner.discoverCalls) != 1 {
		t.Fatalf("coordinator discover calls = %d, want 1 when the host admits", len(coordinatorRunner.discoverCalls))
	}
}
