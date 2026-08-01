package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/hostresources"
	"github.com/MumuTW/looper/internal/storage"
)

// hostAdmissionClaimFixture seeds one runnable worker item and returns a tick
// input with a free slot for it.
func hostAdmissionClaimFixture(t *testing.T, name string) (defaultSchedulerTickInput, *stubWorkerScheduler) {
	t.Helper()
	root := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(root, name+".sqlite"), t.TempDir())
	repositories := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.August, 1, 10, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repositories, root, nowISO)
	item := schedulerTestQueueItem("queue_"+name, "worker", nowISO)
	if err := repositories.Queue.Upsert(context.Background(), item); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	runner := &stubWorkerScheduler{}
	return defaultSchedulerTickInput{
		Repos:             repositories,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		AsyncRunner:       immediateSchedulerRunner{},
		Worker:            runner,
	}, runner
}

// A refused host must stop new work from starting. Without this the guard is
// decorative: it would report pressure while the scheduler kept claiming.
func TestClaimPhaseWithholdsSlotsUnderHostPressure(t *testing.T) {
	t.Parallel()

	input, runner := hostAdmissionClaimFixture(t, "host_refused")
	input.HostAdmission = func() *hostresources.Decision {
		return &hostresources.Decision{
			Admit:   false,
			Reasons: []string{hostresources.ReasonDiskLow},
			Detail:  []string{"2.0 GiB free on the state filesystem is below the 10.0 GiB floor"},
		}
	}

	claimed, slots, err := executeClaimPhase(context.Background(), "test", input, nil, false)
	if err != nil {
		t.Fatalf("executeClaimPhase() error = %v", err)
	}
	if claimed != 0 || slots != 0 {
		t.Fatalf("claimed=%d slots=%d, want both zero under host pressure", claimed, slots)
	}
	if runner.processItemCount() != 0 {
		t.Fatalf("processed %d items, want none started under host pressure", runner.processItemCount())
	}
}

func TestClaimPhaseProceedsWhenTheHostAdmits(t *testing.T) {
	t.Parallel()

	input, runner := hostAdmissionClaimFixture(t, "host_admitted")
	input.HostAdmission = func() *hostresources.Decision {
		return &hostresources.Decision{Admit: true}
	}

	if _, _, err := executeClaimPhase(context.Background(), "test", input, nil, false); err != nil {
		t.Fatalf("executeClaimPhase() error = %v", err)
	}
	if runner.processItemCount() != 1 {
		t.Fatalf("processed %d items, want 1", runner.processItemCount())
	}
}

// A nil decision is "no opinion" — an unreadable platform or a disabled guard.
// It must behave exactly as it did before the guard existed.
func TestClaimPhaseProceedsWithoutAHostOpinion(t *testing.T) {
	t.Parallel()

	input, runner := hostAdmissionClaimFixture(t, "host_no_opinion")
	input.HostAdmission = func() *hostresources.Decision { return nil }

	if _, _, err := executeClaimPhase(context.Background(), "test", input, nil, false); err != nil {
		t.Fatalf("executeClaimPhase() error = %v", err)
	}
	if runner.processItemCount() != 1 {
		t.Fatalf("processed %d items, want 1", runner.processItemCount())
	}
}
