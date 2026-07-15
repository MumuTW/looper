package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/disk"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/storage"
)

func TestEffectiveBlockedConditionMigratesLegacyMarkers(t *testing.T) {
	productMetadata := `{"awaitingProductSpec":true}`
	record, inferred := effectiveBlockedCondition(storage.LoopRecord{Type: "planner", Status: "paused", MetadataJSON: &productMetadata})
	if !inferred || record.Kind != loopcondition.ProductSpec {
		t.Fatalf("effectiveBlockedCondition(product) = %#v, %v", record, inferred)
	}
	humanMetadata := `{"hitl":{"question":"pick","status":"awaiting"}}`
	record, inferred = effectiveBlockedCondition(storage.LoopRecord{Type: "worker", Status: "awaiting_human", MetadataJSON: &humanMetadata})
	if !inferred || record.Kind != loopcondition.HumanAnswered {
		t.Fatalf("effectiveBlockedCondition(human) = %#v, %v", record, inferred)
	}
}

func TestBlockedConditionRegistryContainsEveryNamedCondition(t *testing.T) {
	runtime := &Runtime{}
	registry := runtime.blockedConditionRegistry(&config.Config{}, nil, nil)
	for _, kind := range []loopcondition.Kind{
		loopcondition.ProductSpec,
		loopcondition.DiskRecovered,
		loopcondition.CISettled,
		loopcondition.ReviewUpdated,
		loopcondition.HumanAnswered,
		loopcondition.InfraRecovered,
	} {
		if registry[kind] == nil {
			t.Fatalf("registry[%q] is nil", kind)
		}
	}
}

func TestResumeBlockedLoopClearsConditionAndRequeues(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "project_1"
	loopID := "loop_blocked"
	targetID := "project:project_1"
	metadata, err := loopcondition.Set(nil, loopcondition.Record{Kind: loopcondition.DiskRecovered, Since: nowISO})
	if err != nil {
		t.Fatalf("condition.Set() error = %v", err)
	}
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repositories.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	manualKind := "manual_intervention"
	if err := repositories.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_blocked", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:loop_blocked", Priority: 1, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: -1, LastErrorKind: &manualKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if err := resumeBlockedLoop(ctx, repositories, loop, loopcondition.Record{Kind: loopcondition.DiskRecovered}, func() time.Time { return now }); err != nil {
		t.Fatalf("resumeBlockedLoop() error = %v", err)
	}
	gotLoop, err := repositories.Loops.GetByID(ctx, loopID)
	if err != nil || gotLoop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", gotLoop, err)
	}
	if gotLoop.Status != "queued" || gotLoop.NextRunAt == nil || *gotLoop.NextRunAt != "2026-07-15T12:00:01.000Z" {
		t.Fatalf("loop after resume = %#v", gotLoop)
	}
	if _, ok := loopcondition.Read(gotLoop.MetadataJSON); ok {
		t.Fatal("blocked condition was not cleared")
	}
	active, err := repositories.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || active == nil || active.Status != "queued" {
		t.Fatalf("active queue after resume = %#v, %v", active, err)
	}
}

func TestDiskConditionClearedUsesHighWatermark(t *testing.T) {
	original := diskUsageStat
	t.Cleanup(func() { diskUsageStat = original })
	cfg := &config.Config{}
	cfg.Daemon.DiskBackpressure.Enabled = true
	cfg.Daemon.DiskBackpressure.Path = "/worktrees"
	cfg.Daemon.DiskBackpressure.HighWatermarkPercent = 85
	diskUsageStat = func(string) (disk.Usage, error) { return disk.Usage{UsedPercent: 84.9}, nil }
	if ready, err := diskConditionCleared(cfg); err != nil || !ready {
		t.Fatalf("diskConditionCleared() = %v, %v; want ready", ready, err)
	}
	diskUsageStat = func(string) (disk.Usage, error) { return disk.Usage{UsedPercent: 85}, nil }
	if ready, err := diskConditionCleared(cfg); err != nil || ready {
		t.Fatalf("diskConditionCleared() = %v, %v; want blocked", ready, err)
	}
	diskUsageStat = func(string) (disk.Usage, error) { return disk.Usage{}, errors.New("stat failed") }
	if ready, err := diskConditionCleared(cfg); err == nil || ready {
		t.Fatalf("diskConditionCleared() = %v, %v; want observable check failure", ready, err)
	}
}
