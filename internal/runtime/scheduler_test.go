package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator"
	"github.com/nexu-io/looper/internal/fixer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
)

func TestWorkerAgentExecutionAdapterPropagatesParseStatus(t *testing.T) {
	t.Parallel()

	adapter := workerAgentExecutionAdapter{execution: stubAgentExecution{result: agent.Result{Status: "completed", Summary: "done", Stdout: "ok", ParseStatus: "parsed", ChangedFiles: []string{"worker.go"}, Commits: []string{"abc123"}}}}

	result, err := adapter.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if result.ParseStatus != "parsed" {
		t.Fatalf("ParseStatus = %q, want parsed", result.ParseStatus)
	}
	if result.Status != "completed" || result.Summary != "done" || result.Stdout != "ok" || len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "worker.go" || len(result.Commits) != 1 || result.Commits[0] != "abc123" {
		t.Fatalf("result = %#v, want propagated agent result fields", result)
	}
}

func TestEveryRoleRegistersAgentExecutionForConfirmedStop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		start func(agentExecutionStarter, *ActiveExecutionRegistry) error
	}{
		{name: "planner", start: func(starter agentExecutionStarter, registry *ActiveExecutionRegistry) error {
			_, err := (plannerAgentExecutorAdapter{executor: starter, registry: registry}).Start(context.Background(), planner.AgentRunInput{LoopID: "loop_1", RunID: "run_1", ExecutionID: "exec_1"})
			return err
		}},
		{name: "reviewer", start: func(starter agentExecutionStarter, registry *ActiveExecutionRegistry) error {
			_, err := (reviewerAgentExecutorAdapter{executor: starter, registry: registry}).Start(context.Background(), reviewer.AgentRunInput{LoopID: "loop_1", RunID: "run_1", ExecutionID: "exec_1"})
			return err
		}},
		{name: "fixer", start: func(starter agentExecutionStarter, registry *ActiveExecutionRegistry) error {
			_, err := (fixerAgentExecutorAdapter{executor: starter, registry: registry}).Start(context.Background(), fixer.AgentRunInput{LoopID: "loop_1", RunID: "run_1", ExecutionID: "exec_1"})
			return err
		}},
		{name: "worker", start: func(starter agentExecutionStarter, registry *ActiveExecutionRegistry) error {
			_, err := (workerAgentExecutorAdapter{executor: starter, registry: registry}).Start(context.Background(), worker.AgentRunInput{LoopID: "loop_1", RunID: "run_1", ExecutionID: "exec_1"})
			return err
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			registry := NewActiveExecutionRegistry()
			execution := newBlockingRegistryExecution()
			if err := tt.start(stubAgentExecutionStarter{execution: execution}, registry); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			stopped := make(chan error, 1)
			go func() {
				_, err := registry.StopAndWait(context.Background(), "loop_1", "run_1", "exec_1", "test stop")
				stopped <- err
			}()
			select {
			case <-execution.killed:
			case <-time.After(time.Second):
				t.Fatal("registered execution was not killed")
			}
			execution.finish()
			if err := <-stopped; err != nil {
				t.Fatalf("StopAndWait() error = %v", err)
			}
		})
	}
}

func TestCoordinatorAgentExecutionRegistersForDaemonShutdown(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newBlockingRegistryExecution()
	adapter := coordinatorAgentExecutorAdapter{executor: stubAgentExecutionStarter{execution: execution}, registry: registry}
	if _, err := adapter.Start(context.Background(), agent.RunInput{ExecutionID: "coord_exec_1", Prompt: "triage", WorkingDirectory: t.TempDir()}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	returned := make(chan error, 1)
	go func() { returned <- registry.ShutdownAndWait(context.Background(), "test shutdown") }()
	select {
	case <-execution.killed:
	case <-time.After(time.Second):
		t.Fatal("coordinator execution was not killed during shutdown")
	}
	execution.finish()
	if err := <-returned; err != nil {
		t.Fatalf("ShutdownAndWait() error = %v", err)
	}
}

func TestReviewerRegistryStopCancelsTrustedPublicationOwnership(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	execution := newBlockingRegistryExecution()
	proxyCancelled := make(chan struct{})
	owned := &reviewerOwnedAgentExecution{execution: execution, cleanup: func() { close(proxyCancelled) }}
	registry.Register("loop_review", "run_review", "exec_review", owned)
	returned := make(chan error, 1)
	go func() {
		_, err := registry.StopAndWait(context.Background(), "loop_review", "run_review", "exec_review", "stop reviewer")
		returned <- err
	}()
	select {
	case <-execution.killed:
	case <-time.After(time.Second):
		t.Fatal("reviewer agent was not killed")
	}
	select {
	case <-proxyCancelled:
	case <-time.After(time.Second):
		t.Fatal("trusted publication proxy was not cancelled with reviewer ownership")
	}
	execution.finish()
	if err := <-returned; err != nil {
		t.Fatalf("StopAndWait() error = %v", err)
	}
}

func TestReviewerOwnedExecutionRejectsUnsupportedForcedResolution(t *testing.T) {
	t.Parallel()

	owned := &reviewerOwnedAgentExecution{execution: stubAgentExecution{}}
	if err := owned.ForceKill(); err == nil {
		t.Fatal("ForceKill() returned nil without an underlying confirmed-reap contract")
	}
}

func TestScheduledAgentRunRemainsOwnedAfterAgentPhase(t *testing.T) {
	t.Parallel()

	registry := NewActiveExecutionRegistry()
	runner := &cancellableWorkerScheduler{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	loopID := "loop_validation"
	if err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{ID: "queue_1", LoopID: &loopID, Type: "worker"}}, defaultSchedulerTickInput{Worker: runner, ActiveExecutions: registry}); err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("scheduled run did not start")
	}
	returned := make(chan error, 1)
	go func() {
		_, err := registry.StopAndWait(context.Background(), loopID, "run_1", "completed_agent", "stop validation")
		returned <- err
	}()
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("scheduled run context was not cancelled")
	}
	select {
	case err := <-returned:
		t.Fatalf("StopAndWait returned before runner cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(runner.release)
	if err := <-returned; err != nil {
		t.Fatalf("StopAndWait() error = %v", err)
	}
}

func TestRunScheduledQueueItemsCancelsClaimWhenLoopStopRejectsRegisterRun(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "register-run-reject.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "project_register_reject"
	loopID := "loop_register_reject"
	queueID := "queue_register_reject"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Register reject", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	claimed := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:register_reject", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(nowISO),
		StartedAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Queue.Upsert(context.Background(), claimed); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	registry := NewActiveExecutionRegistry()
	releaseStop := registry.BeginLoopStop(loopID)
	defer releaseStop()

	runner := &stubWorkerScheduler{}
	if err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{claimed}, defaultSchedulerTickInput{
		Repos:            repos,
		Now:              func() time.Time { return now },
		Worker:           runner,
		ActiveExecutions: registry,
	}); err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	if runner.processItemCount() != 0 {
		t.Fatalf("worker processed %d items after RegisterRun rejection, want 0", runner.processItemCount())
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil || updated.Status != "cancelled" {
		t.Fatalf("queue item = %#v, want status cancelled after loop-stop rejected run registration", updated)
	}
}

func TestRunScheduledQueueItemsRequeuesClaimWhenShutdownRejectsRegisterRun(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "register-run-shutdown.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "project_register_shutdown"
	loopID := "loop_register_shutdown"
	queueID := "queue_register_shutdown"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Register shutdown", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	claimed := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:register_shutdown", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(nowISO),
		StartedAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Queue.Upsert(context.Background(), claimed); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	registry := NewActiveExecutionRegistry()
	registry.BeginShutdown("daemon shutting down")

	runner := &stubWorkerScheduler{}
	if err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{claimed}, defaultSchedulerTickInput{
		Repos:            repos,
		Now:              func() time.Time { return now },
		Worker:           runner,
		ActiveExecutions: registry,
	}); err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	if runner.processItemCount() != 0 {
		t.Fatalf("worker processed %d items after RegisterRun rejection, want 0", runner.processItemCount())
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil || updated.Status != "queued" {
		t.Fatalf("queue item = %#v, want status queued after shutdown rejected run registration", updated)
	}
	if updated.ClaimedBy != nil || updated.ClaimedAt != nil || updated.StartedAt != nil {
		t.Fatalf("queue item claim fields = claimedBy=%v claimedAt=%v startedAt=%v, want cleared", updated.ClaimedBy, updated.ClaimedAt, updated.StartedAt)
	}
}

func TestRunScheduledQueueItemsSurfacesRequeueFailureAfterShutdownRejectsRegisterRun(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "register-run-shutdown-fail.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "project_register_shutdown_fail"
	loopID := "loop_register_shutdown_fail"
	queueID := "queue_register_shutdown_fail"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Register shutdown fail", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	claimed := storage.QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:register_shutdown_fail", Priority: storage.QueuePriorityWorker, Status: "running",
		AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, ClaimedBy: stringPtr("scheduler"), ClaimedAt: stringPtr(nowISO),
		StartedAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Queue.Upsert(context.Background(), claimed); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	registry := NewActiveExecutionRegistry()
	registry.BeginShutdown("daemon shutting down")
	// Close the DB so RequeueRunningByLoop fails after RegisterRun rejection.
	if err := coordinator.Close(); err != nil {
		t.Fatalf("coordinator.Close() error = %v", err)
	}

	runner := &stubWorkerScheduler{}
	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{claimed}, defaultSchedulerTickInput{
		Repos:            repos,
		Now:              func() time.Time { return now },
		Worker:           runner,
		ActiveExecutions: registry,
	})
	if err == nil {
		t.Fatal("runScheduledQueueItems() error = nil, want requeue failure after shutdown rejected registration")
	}
	if !strings.Contains(err.Error(), "requeue claimed queue item") {
		t.Fatalf("error = %q, want requeue claimed queue item context", err.Error())
	}
	if runner.processItemCount() != 0 {
		t.Fatalf("worker processed %d items after RegisterRun rejection, want 0", runner.processItemCount())
	}
}

func TestCommentInfosToObjectsPreservesNestedAuthorLogin(t *testing.T) {
	t.Parallel()

	comments := commentInfosToObjects([]githubinfra.CommentInfo{{ID: 1, Author: "looper", AuthorAssociation: "MEMBER", Body: "summary", URL: "https://example.test/comment/1"}})
	if len(comments) != 1 {
		t.Fatalf("len(comments) = %d, want 1", len(comments))
	}
	author, ok := comments[0]["author"].(map[string]any)
	if !ok {
		t.Fatalf("author = %#v, want nested map", comments[0]["author"])
	}
	if author["login"] != "looper" {
		t.Fatalf("author login = %#v, want looper", author["login"])
	}
}

func TestRunDefaultSchedulerTickDiscoversStoredProjectsAndProcessesQueue(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_1", Seq: 1, ProjectID: "looper", Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{}
	coordinatorRunner := &stubCoordinatorScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}

	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:              repos,
		Now:                func() time.Time { return now },
		MaxConcurrentRuns:  1,
		Planner:            plannerRunner,
		Coordinator:        coordinatorRunner,
		CoordinatorEnabled: func(string) bool { return true },
		Reviewer:           reviewerRunner,
		Fixer:              fixerRunner,
		Worker:             workerRunner,
	})
	if err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if len(plannerRunner.discoverCalls) != 1 || plannerRunner.discoverCalls[0].ProjectID != "looper" || plannerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("planner discover calls = %#v, want stored project discovery", plannerRunner.discoverCalls)
	}
	if len(coordinatorRunner.discoverCalls) != 1 || coordinatorRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("coordinator discover calls = %#v, want stored project repo", coordinatorRunner.discoverCalls)
	}
	if len(reviewerRunner.discoverCalls) != 1 || reviewerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("reviewer discover calls = %#v, want stored project repo", reviewerRunner.discoverCalls)
	}
	if len(fixerRunner.discoverCalls) != 1 || fixerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("fixer discover calls = %#v, want stored project repo", fixerRunner.discoverCalls)
	}
	if len(workerRunner.discoverCalls) != 1 || workerRunner.discoverCalls[0].Repo != "nexu-io/looper" {
		t.Fatalf("worker discover calls = %#v, want stored project repo", workerRunner.discoverCalls)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 1
	})
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want one queued worker run", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickDefersStaleCatalogBeforeClaimAndDiscovery(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-catalog-binding.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 13, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	forgejoMetadata := `{"provider":"forgejo-main","repo":"forgejo/new"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "rebound", Name: "Rebound", RepoPath: filepath.Join(workingDir, "rebound"), BaseBranch: &baseBranch, MetadataJSON: &forgejoMetadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:rebound"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_rebound", Seq: 1, ProjectID: "rebound", Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("forgejo/new"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "rebound"
	loopID := "loop_rebound"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_rebound", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("forgejo/new"), DedupeKey: "worker:loop_rebound", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	captured := config.Config{
		Projects:  []config.ProjectRefConfig{{ID: "rebound", Repo: "github/old"}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}},
	}
	plannerRunner := &stubPlannerScheduler{}
	coordinatorRunner := &stubCoordinatorScheduler{}
	workerRunner := &stubWorkerScheduler{}
	input := defaultSchedulerTickInput{
		Repos:                   repos,
		Now:                     func() time.Time { return now },
		MaxConcurrentRuns:       1,
		Config:                  &captured,
		Planner:                 plannerRunner,
		Coordinator:             coordinatorRunner,
		Worker:                  workerRunner,
		CoordinatorEnabled:      func(string) bool { return true },
		PlannerDiscoveryEnabled: boolPtr(true),
	}
	if err := runDefaultSchedulerTick(context.Background(), input); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if err := runIndependentClaimPass(context.Background(), input); err != nil {
		t.Fatalf("runIndependentClaimPass() error = %v", err)
	}

	if len(plannerRunner.discoverCalls) != 0 {
		t.Fatalf("planner discover calls = %#v, want stale catalog pass deferred", plannerRunner.discoverCalls)
	}
	if len(coordinatorRunner.discoverCalls) != 0 || workerRunner.processItemCount() != 0 {
		t.Fatalf("coordinator calls = %#v, worker processed items = %#v; want no stale dispatch", coordinatorRunner.discoverCalls, workerRunner.processedItems)
	}
	queued, err := repos.Queue.GetByID(context.Background(), "queue_rebound")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queued == nil || queued.Status != "queued" {
		t.Fatalf("queue item = %#v, want unclaimed until catalog publication", queued)
	}
}

func TestRunDefaultSchedulerTickClaimsQueuedWorkBeforeDiscovery(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-claim-first.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectID := "looper"
	loopTarget := "project:looper"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_worker_claim_first", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &loopTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopID := "loop_worker_claim_first"
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_claim_first", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: loopTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_claim_first", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &queueStatusCheckingPlannerScheduler{t: t, repos: repos, queueItemID: "queue_worker_claim_first"}

	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Worker:            &stubWorkerScheduler{},
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if !plannerRunner.checkedClaimedStatus {
		t.Fatal("planner discovery did not verify claimed queue item status")
	}
}

func TestRunDefaultSchedulerTickSkipsCoordinatorWhenDisabled(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinatorDB := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-coordinator-disabled.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinatorDB.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	runner := &stubCoordinatorScheduler{}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:              repos,
		Now:                func() time.Time { return now },
		Coordinator:        runner,
		CoordinatorEnabled: func(string) bool { return false },
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if len(runner.discoverCalls) != 0 {
		t.Fatalf("coordinator discover calls = %#v, want none when disabled", runner.discoverCalls)
	}
}

func TestRunDefaultSchedulerTickSecondClaimPassDoesNotExceedAvailableSlots(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-second-pass-slots.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)
	queued := schedulerTestQueueItem("queue_worker_existing", "worker", nowISO)
	if err := repos.Queue.Upsert(context.Background(), queued); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	discovered := schedulerTestQueueItem("queue_worker_discovered", "worker", nowISO)
	workerRunner := &discoveringWorkerScheduler{repos: repos, nowISO: nowISO, item: discovered}
	var wakes int32
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:                repos,
		Now:                  func() time.Time { return now },
		MaxConcurrentRuns:    1,
		AsyncRunner:          immediateSchedulerRunner{},
		RequestSchedulerWake: func() { atomic.AddInt32(&wakes, 1) },
		Worker:               workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 1 || workerRunner.processedItems[0] != queued.ID {
		t.Fatalf("worker processed items = %#v, want only first-pass item", workerRunner.processedItems)
	}
	item, err := repos.Queue.GetByID(context.Background(), discovered.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item == nil || item.Status != "queued" {
		t.Fatalf("discovered item = %#v, want still queued because first-pass run used the only slot", item)
	}
	if got := atomic.LoadInt32(&wakes); got != 0 {
		t.Fatalf("scheduler wake requests = %d, want none when discovered work could not be claimed", got)
	}
}

func TestRunDefaultSchedulerTickSecondClaimPassDoesNotDoubleClaim(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-no-double-claim.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)
	queued := schedulerTestQueueItem("queue_worker_existing", "worker", nowISO)
	if err := repos.Queue.Upsert(context.Background(), queued); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	workerRunner := &stubWorkerScheduler{}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 2,
		AsyncRunner:       immediateSchedulerRunner{},
		Worker:            workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 1 || workerRunner.processedItems[0] != queued.ID {
		t.Fatalf("worker processed items = %#v, want existing item exactly once", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickSecondClaimPassPreservesQueueEligibilityRules(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-second-pass-eligibility.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	lockKey := "repo:nexu-io/looper"
	lockedA := schedulerTestQueueItem("queue_worker_lock_a", "worker", nowISO)
	lockedA.LockKey = &lockKey
	lockedB := schedulerTestQueueItem("queue_worker_lock_b", "worker", nowISO)
	lockedB.LockKey = &lockKey
	repo := "nexu-io/looper"
	pr := int64(281)
	reviewerItem := schedulerTestQueueItem("queue_reviewer_pr", "reviewer", nowISO)
	reviewerItem.Repo = &repo
	reviewerItem.PRNumber = &pr
	fixerItem := schedulerTestQueueItem("queue_fixer_pr", "fixer", nowISO)
	fixerItem.Repo = &repo
	fixerItem.PRNumber = &pr

	plannerRunner := &enqueueingPlannerScheduler{repos: repos, items: []storage.QueueItemRecord{lockedA, lockedB}}
	reviewerRunner := &enqueueingReviewerScheduler{repos: repos, item: reviewerItem}
	fixerRunner := &enqueueingFixerScheduler{repos: repos, item: fixerItem}
	workerRunner := &stubWorkerScheduler{}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 4,
		AsyncRunner:       immediateSchedulerRunner{},
		Planner:           plannerRunner,
		Reviewer:          reviewerRunner,
		Fixer:             fixerRunner,
		Worker:            workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want one of two same-lock discovered workers", workerRunner.processedItems)
	}
	if reviewerRunner.processItemCount() != 1 {
		t.Fatalf("reviewer processed items = %#v, want reviewer claimed", reviewerRunner.processedItems)
	}
	if fixerRunner.processItemCount() != 0 {
		t.Fatalf("fixer processed items = %#v, want fixer gated behind reviewer", fixerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickDiscoveryWakeIsBounded(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-wake-bounded.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	var wakes int32
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:                repos,
		Now:                  func() time.Time { return now },
		MaxConcurrentRuns:    4,
		RequestSchedulerWake: func() { atomic.AddInt32(&wakes, 1) },
		Planner:              &enqueueingPlannerScheduler{repos: repos, items: []storage.QueueItemRecord{schedulerTestQueueItem("queue_worker_from_planner_a", "worker", nowISO), schedulerTestQueueItem("queue_worker_from_planner_b", "worker", nowISO)}},
		Reviewer:             &enqueueingReviewerScheduler{repos: repos, item: schedulerTestQueueItem("queue_reviewer_wake", "reviewer", nowISO)},
		Fixer:                &enqueueingFixerScheduler{repos: repos, item: schedulerTestQueueItem("queue_fixer_wake", "fixer", nowISO)},
		Worker:               &discoveringWorkerScheduler{repos: repos, nowISO: nowISO, item: schedulerTestQueueItem("queue_worker_wake", "worker", nowISO)},
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if got := atomic.LoadInt32(&wakes); got != 3 {
		t.Fatalf("scheduler wake requests = %d, want one wake per claim phase that picked up discovered work", got)
	}
}

func TestIndependentClaimPassClaimsQueuedWorkWhileDiscoveryIsBlocked(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-claim-pump.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)

	plannerRunner := &blockingPlannerDiscoveryScheduler{started: make(chan struct{}), release: make(chan struct{})}
	workerRunner := &stubWorkerScheduler{}
	input := defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 5,
		ClaimMu:           &sync.Mutex{},
		AsyncRunner:       immediateSchedulerRunner{},
		Planner:           plannerRunner,
		Worker:            workerRunner,
	}

	tickDone := make(chan error, 1)
	go func() {
		tickDone <- runDefaultSchedulerTick(context.Background(), input)
	}()

	select {
	case <-plannerRunner.started:
	case <-time.After(time.Second):
		t.Fatal("planner discovery did not block")
	}

	for i := 0; i < 5; i++ {
		item := schedulerTestQueueItem(fmt.Sprintf("queue_worker_claim_pump_%d", i), "worker", nowISO)
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	claimDone := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := runIndependentClaimPass(context.Background(), input); err != nil {
				claimDone <- err
				return
			}
			if workerRunner.processItemCount() == 5 {
				claimDone <- nil
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		claimDone <- fmt.Errorf("timed out waiting for independent claim pass to process all items")
	}()

	select {
	case err := <-claimDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("independent claim pass did not finish before timeout")
	}

	close(plannerRunner.release)
	if err := <-tickDone; err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
}

func TestRunDefaultSchedulerTickReconcilesLiveStaleRunsWhenCapacityIsFull(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-live-reconcile.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	oldISO := formatJavaScriptISOString(now.Add(-2 * time.Hour))
	insertSchedulerProject(t, repos, workingDir, nowISO)
	projectID := "looper"
	staleLoopID := "loop_reconcile_stale"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: staleLoopID, Seq: 2, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: stringPtr("pr:nexu-io/looper:42"), Repo: stringPtr("nexu-io/looper"), PRNumber: int64Ptr(42), Status: "running", CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Loops.Upsert(stale) error = %v", err)
	}
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_reconcile_stale", LoopID: staleLoopID, Status: "running", CurrentStep: stringPtr("review"), StartedAt: oldISO, LastHeartbeatAt: stringPtr(oldISO), CreatedAt: oldISO, UpdatedAt: oldISO}); err != nil {
		t.Fatalf("Runs.Upsert(stale) error = %v", err)
	}
	queuedLoopID := "loop_worker_queued"
	loopTarget := "project:project_1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: queuedLoopID, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &loopTarget, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert(queued) error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_after_reconcile", ProjectID: &projectID, LoopID: &queuedLoopID, Type: "worker", TargetType: "project", TargetID: loopTarget, DedupeKey: "worker:loop_worker_queued", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	workerRunner := &stubWorkerScheduler{}
	reconcileCalls := 0
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		AsyncRunner:       immediateSchedulerRunner{},
		ReconcileStaleRuns: func(context.Context) (StaleRunReconcileSummary, error) {
			reconcileCalls++
			if err := interruptRecoveryRun(context.Background(), repos, storage.RunRecord{ID: "run_reconcile_stale", LoopID: staleLoopID}, storage.LoopRecord{ID: staleLoopID, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "running"}, nowISO, "Interrupted stale running run during stale-run reconciliation"); err != nil {
				return StaleRunReconcileSummary{}, err
			}
			return StaleRunReconcileSummary{Mode: "live", CandidateRuns: 1, InterruptedRuns: 1, LoopsRequeued: 1, RunIDs: []string{"run_reconcile_stale"}, LoopIDs: []string{staleLoopID}}, nil
		},
		Worker: workerRunner,
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	if reconcileCalls == 0 {
		t.Fatal("reconcile calls = 0, want at least one live reconcile attempt")
	}
	waitForSchedulerCondition(t, func() bool { return workerRunner.processItemCount() == 1 })
	if workerRunner.processItemCount() != 1 || workerRunner.processedItems[0] != "queue_worker_after_reconcile" {
		t.Fatalf("worker processed items = %#v, want queued item claimed after live reconcile", workerRunner.processedItems)
	}
}

func TestRunDefaultSchedulerTickLogsClaimPhasesAndSlowLanes(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler-logs.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	insertSchedulerProject(t, repos, workingDir, nowISO)
	logger := &capturingSchedulerLogger{}
	cfg, err := config.DefaultConfig(workingDir)
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Scheduler.SlowLaneWarnThresholdMS = 1
	cfg.Projects = []config.ProjectRefConfig{{ID: "looper", Repo: "nexu-io/looper"}}

	plannerRunner := &sleepingPlannerScheduler{delay: 5 * time.Millisecond}
	if err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:                    repos,
		Logger:                   logger,
		Now:                      func() time.Time { return now },
		MaxConcurrentRuns:        1,
		Config:                   &cfg,
		Planner:                  plannerRunner,
		ReviewerDiscoveryEnabled: boolPtr(false),
		FixerDiscoveryEnabled:    boolPtr(false),
		WorkerDiscoveryEnabled:   boolPtr(false),
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}

	logger.requireMessage(t, "scheduler tick start")
	logger.requireMessage(t, "scheduler tick end")
	logger.requireMessage(t, "scheduler tick summary")
	logger.requireContextValue(t, "scheduler claim phase", "phase", "pre_discovery")
	logger.requireContextValue(t, "scheduler claim phase", "phase", "post_planner_discovery")
	logger.requireContextValue(t, "scheduler lane start", "lane", "planner discovery")
	logger.requireContextValue(t, "scheduler lane end", "lane", "planner discovery")
	logger.requireContextValue(t, "scheduler lane slow", "lane", "planner discovery")
}

func TestRunScheduledQueueItemsDispatchesEachSupportedType(t *testing.T) {
	t.Parallel()

	queueItems := []storage.QueueItemRecord{{ID: "planner-item", Type: "planner"}, {ID: "reviewer-item", Type: "reviewer"}, {ID: "fixer-item", Type: "fixer"}, {ID: "worker-item", Type: "worker"}}
	plannerRunner := &stubPlannerScheduler{}
	reviewerRunner := &stubReviewerScheduler{}
	fixerRunner := &stubFixerScheduler{}
	workerRunner := &stubWorkerScheduler{}

	err := runScheduledQueueItems(context.Background(), queueItems, defaultSchedulerTickInput{
		Planner:  plannerRunner,
		Reviewer: reviewerRunner,
		Fixer:    fixerRunner,
		Worker:   workerRunner,
	})
	if err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return plannerRunner.processItemCount() == 1 && reviewerRunner.processItemCount() == 1 && fixerRunner.processItemCount() == 1 && workerRunner.processItemCount() == 1
	})
	if plannerRunner.processItemCount() != 1 || reviewerRunner.processItemCount() != 1 || fixerRunner.processItemCount() != 1 || workerRunner.processItemCount() != 1 {
		t.Fatalf("processed items = planner:%#v reviewer:%#v fixer:%#v worker:%#v, want planner/reviewer/fixer/worker once", plannerRunner.processedItems, reviewerRunner.processedItems, fixerRunner.processedItems, workerRunner.processedItems)
	}
}

func TestRunScheduledQueueItemsProcessesItemsConcurrently(t *testing.T) {
	t.Parallel()

	runner := &parallelWorkerScheduler{
		secondStarted: make(chan struct{}),
	}
	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}, {Type: "worker"}}, defaultSchedulerTickInput{
		Worker: runner,
	})
	if err != nil {
		t.Fatalf("runScheduledQueueItems() error = %v", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return atomic.LoadInt32(&runner.calls) == 2
	})
	if got := atomic.LoadInt32(&runner.calls); got != 2 {
		t.Fatalf("worker ProcessNext calls = %d, want 2", got)
	}
}

func TestRunScheduledQueueItemsReturnsBeforeClaimedRunsFinish(t *testing.T) {
	t.Parallel()

	runner := &blockingWorkerScheduler{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	returned := make(chan error, 1)
	go func() {
		returned <- runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}}, defaultSchedulerTickInput{Worker: runner})
	}()

	select {
	case <-runner.started:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("worker ProcessNext did not start")
	}

	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("runScheduledQueueItems() error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("runScheduledQueueItems() did not return before claimed run finished")
	}

	close(runner.release)
}

func TestClaimAndRunScheduledQueueItemsUsesLongTermRetryOnlyForIdleSlots(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "long-retry-idle.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_retry", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_retry", Seq: 1, ProjectID: "project_retry", Type: "worker", TargetType: "project", Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopID := "loop_retry"
	retryKind := "retryable_transient"
	for _, item := range []storage.QueueItemRecord{
		{ID: "fresh_1", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_retry", DedupeKey: "d_fresh_1", Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: -1, CreatedAt: "2026-04-21T07:40:00.000Z", UpdatedAt: nowISO},
		{ID: "fresh_2", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_retry", DedupeKey: "d_fresh_2", Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: -1, CreatedAt: "2026-04-21T07:41:00.000Z", UpdatedAt: nowISO},
		{ID: "long_retry", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_retry", DedupeKey: "d_long_retry", Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, Attempts: storage.QueueLongTermRetryAttemptThreshold, MaxAttempts: -1, LastErrorKind: &retryKind, CreatedAt: "2026-04-21T07:00:00.000Z", UpdatedAt: nowISO},
	} {
		if err := repos.Queue.Upsert(context.Background(), item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	workerRunner := &stubWorkerScheduler{}
	claimed, err := claimAndRunScheduledQueueItems(context.Background(), 2, defaultSchedulerTickInput{Repos: repos, Now: func() time.Time { return now }, Worker: workerRunner})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems() error = %v", err)
	}
	if len(claimed) != 2 || claimed[0].ID != "fresh_1" || claimed[1].ID != "fresh_2" {
		t.Fatalf("claimed = %#v, want only fresh items when fresh work fills slots", claimed)
	}

	workerRunner = &stubWorkerScheduler{}
	claimed, err = claimAndRunScheduledQueueItems(context.Background(), 2, defaultSchedulerTickInput{Repos: repos, Now: func() time.Time { return now }, Worker: workerRunner})
	if err != nil {
		t.Fatalf("claimAndRunScheduledQueueItems(second) error = %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "long_retry" {
		t.Fatalf("claimed second = %#v, want long retry after ordinary work is claimed", claimed)
	}
}

func TestRunScheduledQueueItemsRejectsUnsupportedType(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "mystery"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "unsupported queue item type") {
		t.Fatalf("runScheduledQueueItems() error = %v, want unsupported queue item type", err)
	}
}

func TestRunScheduledQueueItemsErrorsWhenRunnerMissing(t *testing.T) {
	t.Parallel()

	err := runScheduledQueueItems(context.Background(), []storage.QueueItemRecord{{Type: "worker"}}, defaultSchedulerTickInput{})
	if err == nil || !strings.Contains(err.Error(), "worker runner is not configured") {
		t.Fatalf("runScheduledQueueItems() error = %v, want missing worker runner error", err)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientCaptureFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-retry.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{err: errors.New("gh timeout")},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || *updated.LastError != "gh timeout" || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable gh timeout", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemStopsRetryingAtMaxAttempts(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-terminal.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_terminal_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109:terminal", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{Repos: repos, Now: func() time.Time { return now }, Snapshotter: stubSnapshotScheduler{err: errors.New("gh timeout")}})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want failed queue item")
	}
	if updated.Status != "manual_intervention" || updated.Attempts != 1 || updated.FinishedAt == nil {
		t.Fatalf("queue item = %#v, want manual_intervention terminal item with one attempt", updated)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientPersistenceFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-upsert-retry.sqlite"), backupDir)
	repos := storage.NewRepositories(&failingSnapshotUpsertQuerier{db: coordinator.DB(), err: errors.New("database is locked")})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_upsert_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || !strings.Contains(*updated.LastError, "database is locked") || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable database is locked", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientProjectLookupFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-project-lookup-retry.sqlite"), backupDir)
	setupRepos := storage.NewRepositories(coordinator.DB())
	repos := storage.NewRepositories(&failingProjectLookupQuerier{db: coordinator.DB()})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := setupRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_project_lookup_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := setupRepos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	if updated.LastError == nil || !strings.Contains(*updated.LastError, "get project by id") || updated.LastErrorKind == nil || *updated.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%v, %v), want retryable project lookup failure", updated.LastError, updated.LastErrorKind)
	}
}

func TestProcessSnapshotQueueItemRetriesTransientCompletionFailure(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-complete-retry.sqlite"), backupDir)
	setupRepos := storage.NewRepositories(coordinator.DB())
	repos := storage.NewRepositories(&failingQueueCompleteQuerier{db: coordinator.DB(), err: errors.New("database is locked")})
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "looper"
	repo := "nexu-io/looper"
	prNumber := int64(109)
	if err := setupRepos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_complete_1", ProjectID: &projectID, Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := setupRepos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Queue.GetByID() = nil, want retried queue item")
	}
	if updated.Status != "queued" || updated.Attempts != 1 || updated.FinishedAt != nil {
		t.Fatalf("queue item = %#v, want queued retry with one attempt and no finished_at", updated)
	}
	assertQueueRetryError(t, updated, "database is locked")
}

func TestProcessSnapshotQueueItemFailsNonRetryableInvalidItem(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "snapshot-project-missing-fail.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	repo := "nexu-io/looper"
	prNumber := int64(109)
	queueItem := storage.QueueItemRecord{ID: "queue_snapshot_invalid_item", Type: "snapshot", TargetType: "pull_request", TargetID: "nexu-io/looper#109", Repo: &repo, PRNumber: &prNumber, DedupeKey: "snapshot:nexu-io/looper:109", Priority: storage.QueuePriorityReviewer, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Queue.Upsert(context.Background(), queueItem); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	err := processSnapshotQueueItem(context.Background(), queueItem, defaultSchedulerTickInput{
		Repos:       repos,
		Now:         func() time.Time { return now },
		Snapshotter: stubSnapshotScheduler{},
	})
	if err != nil {
		t.Fatalf("processSnapshotQueueItem() error = %v", err)
	}
	updated, err := repos.Queue.GetByID(context.Background(), queueItem.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if updated == nil || updated.Status != "manual_intervention" || updated.FinishedAt == nil {
		t.Fatalf("queue item = %#v, want parked queue item", updated)
	}
	if updated.LastError == nil || *updated.LastError != "invalid snapshot queue item" || updated.LastErrorKind == nil || *updated.LastErrorKind != "non_retryable" {
		t.Fatalf("queue item error = (%v, %v), want non-retryable invalid-item failure", updated.LastError, updated.LastErrorKind)
	}
}

func TestSchedulerAvailableSlotsAccountsForRunningQueueItems(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "slots.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectID := "looper"
	loopID := "loop_worker_running"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	projectTarget := "project:looper"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_running", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project:looper", DedupeKey: "worker:running", Priority: 1, Status: "running", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert(running) error = %v", err)
	}

	available, err := schedulerAvailableSlots(context.Background(), repos, 1)
	if err != nil {
		t.Fatalf("schedulerAvailableSlots() error = %v", err)
	}
	if available != 0 {
		t.Fatalf("schedulerAvailableSlots() = %d, want 0", available)
	}
}

func TestRunDefaultSchedulerTickContinuesAfterDiscoveryError(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	backupDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "errors.sqlite"), backupDir)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 21, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	projectTarget := "project:looper"
	projectID := "looper"
	loopID := "loop_worker_1"
	if err := repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &projectTarget, Repo: stringPtr("nexu-io/looper"), Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_worker_1", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectTarget, Repo: stringPtr("nexu-io/looper"), DedupeKey: "worker:loop_worker_1", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	plannerRunner := &stubPlannerScheduler{discoverErr: errors.New("planner boom")}
	workerRunner := &stubWorkerScheduler{}
	err := runDefaultSchedulerTick(context.Background(), defaultSchedulerTickInput{
		Repos:             repos,
		Now:               func() time.Time { return now },
		MaxConcurrentRuns: 1,
		Planner:           plannerRunner,
		Worker:            workerRunner,
	})
	if err == nil || !strings.Contains(err.Error(), "planner discovery failed") {
		t.Fatalf("runDefaultSchedulerTick() error = %v, want joined discovery error", err)
	}
	waitForSchedulerCondition(t, func() bool {
		return workerRunner.processItemCount() == 1
	})
	if workerRunner.processItemCount() != 1 {
		t.Fatalf("worker processed items = %#v, want queue processing to continue", workerRunner.processedItems)
	}
}

func TestGithubCLIAutoPROpeningAvailableRechecksAuthenticatedCLIWithoutConfiguredPath(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	authenticatedPath := filepath.Join(rootDir, "gh-authenticated")
	writeExecutable(t, authenticatedPath, `#!/bin/sh
case "$*" in
  "auth status --hostname github.com")
    exit 0
    ;;
  *)
    printf '{}'
    ;;
esac
`)
	unauthenticatedPath := filepath.Join(rootDir, "gh-unauthenticated")
	writeExecutable(t, unauthenticatedPath, `#!/bin/sh
case "$*" in
  "auth status --hostname github.com")
    exit 1
    ;;
  *)
    printf '{}'
    ;;
esac
`)

	logger := &testLogger{}
	authenticatedGateway := githubinfra.New(githubinfra.Options{GHPath: authenticatedPath, CWD: rootDir})
	unauthenticatedGateway := githubinfra.New(githubinfra.Options{GHPath: unauthenticatedPath, CWD: rootDir})

	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &authenticatedPath}}, authenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true for authenticated gh cli")
	}
	if githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &unauthenticatedPath}}, unauthenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = true, want false for unauthenticated gh cli")
	}
	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{}, authenticatedGateway, logger, "nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true when gateway can recheck authenticated gh cli without configured path")
	}
}

func TestGithubCLIAutoPROpeningAvailableUsesRepoHostname(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	logPath := filepath.Join(rootDir, "gh.log")
	scriptPath := filepath.Join(rootDir, "gh")
	writeExecutable(t, scriptPath, fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  "auth status --hostname github.example.com")
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
`, logPath))

	logger := &testLogger{}
	gateway := githubinfra.New(githubinfra.Options{GHPath: scriptPath, CWD: rootDir})
	if !githubCLIAutoPROpeningAvailable(context.Background(), config.Config{Tools: config.ToolPathsConfig{GHPath: &scriptPath}}, gateway, logger, "github.example.com/nexu-io/looper", rootDir) {
		t.Fatal("githubCLIAutoPROpeningAvailable() = false, want true for repo host auth")
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if log := string(logBytes); !strings.Contains(log, "auth status --hostname github.example.com") {
		t.Fatalf("gh log = %q, want repo hostname auth status", log)
	}
}

type stubPlannerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []planner.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

type stubCoordinatorScheduler struct {
	mu            sync.Mutex
	discoverCalls []coordinator.DiscoveryInput
	discoverErr   error
}

type immediateSchedulerRunner struct{}

func (immediateSchedulerRunner) Go(fn func()) { fn() }

func insertSchedulerProject(t *testing.T, repos *storage.Repositories, workingDir, nowISO string) {
	t.Helper()
	baseBranch := "main"
	projectMetadata := `{"repo":"nexu-io/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "looper", Name: "Looper", RepoPath: filepath.Join(workingDir, "repo"), BaseBranch: &baseBranch, MetadataJSON: &projectMetadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
}

func schedulerTestQueueItem(id, queueType, nowISO string) storage.QueueItemRecord {
	projectID := "looper"
	return storage.QueueItemRecord{ID: id, ProjectID: &projectID, Type: queueType, TargetType: "project", TargetID: "project:looper", Repo: stringPtr("nexu-io/looper"), DedupeKey: "dedupe:" + id, Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}
}

type queueStatusCheckingPlannerScheduler struct {
	t                    *testing.T
	repos                *storage.Repositories
	queueItemID          string
	checkedClaimedStatus bool
}

type enqueueingPlannerScheduler struct {
	stubPlannerScheduler
	repos *storage.Repositories
	items []storage.QueueItemRecord
}

func (s *enqueueingPlannerScheduler) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.stubPlannerScheduler.DiscoverIssues(ctx, input)
	for _, item := range s.items {
		if err := s.repos.Queue.Upsert(ctx, item); err != nil {
			return planner.DiscoveryResult{}, err
		}
	}
	return planner.DiscoveryResult{QueueItems: append([]storage.QueueItemRecord(nil), s.items...)}, nil
}

type enqueueingReviewerScheduler struct {
	stubReviewerScheduler
	repos *storage.Repositories
	item  storage.QueueItemRecord
}

func (s *enqueueingReviewerScheduler) DiscoverPullRequests(ctx context.Context, input reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error) {
	s.stubReviewerScheduler.DiscoverPullRequests(ctx, input)
	if err := s.repos.Queue.Upsert(ctx, s.item); err != nil {
		return reviewer.DiscoveryResult{}, err
	}
	return reviewer.DiscoveryResult{QueueItems: []storage.QueueItemRecord{s.item}}, nil
}

func (s *enqueueingReviewerScheduler) DiscoverPullRequest(ctx context.Context, input reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	return reviewer.DiscoveryResult{}, nil
}

type enqueueingFixerScheduler struct {
	stubFixerScheduler
	repos *storage.Repositories
	item  storage.QueueItemRecord
}

func (s *enqueueingFixerScheduler) DiscoverPullRequests(ctx context.Context, input fixer.DiscoveryInput) (fixer.DiscoveryResult, error) {
	s.stubFixerScheduler.DiscoverPullRequests(ctx, input)
	if err := s.repos.Queue.Upsert(ctx, s.item); err != nil {
		return fixer.DiscoveryResult{}, err
	}
	return fixer.DiscoveryResult{QueueItems: []storage.QueueItemRecord{s.item}}, nil
}

func (s *enqueueingFixerScheduler) DiscoverPullRequest(ctx context.Context, input fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, nil
}

func (s *enqueueingFixerScheduler) DiscoverPullRequestsForBaseBranchUpdate(_ context.Context, _ fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, nil
}

type discoveringWorkerScheduler struct {
	stubWorkerScheduler
	repos  *storage.Repositories
	nowISO string
	item   storage.QueueItemRecord
}

func (s *discoveringWorkerScheduler) DiscoverIssues(ctx context.Context, input worker.DiscoveryInput) (worker.DiscoveryResult, error) {
	s.stubWorkerScheduler.DiscoverIssues(ctx, input)
	if err := s.repos.Queue.Upsert(ctx, s.item); err != nil {
		return worker.DiscoveryResult{}, err
	}
	return worker.DiscoveryResult{QueueItems: []storage.QueueItemRecord{s.item}}, nil
}

type blockingPlannerDiscoveryScheduler struct {
	stubPlannerScheduler
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingPlannerDiscoveryScheduler) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.stubPlannerScheduler.DiscoverIssues(ctx, input)
	s.once.Do(func() { close(s.started) })
	<-s.release
	return planner.DiscoveryResult{}, nil
}

type sleepingPlannerScheduler struct {
	stubPlannerScheduler
	delay time.Duration
}

func (s *sleepingPlannerScheduler) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.stubPlannerScheduler.DiscoverIssues(ctx, input)
	time.Sleep(s.delay)
	return planner.DiscoveryResult{}, nil
}

type capturingSchedulerLogger struct {
	mu      sync.Mutex
	entries []schedulerLogEntry
}

type schedulerLogEntry struct {
	message string
	context map[string]any
}

func (l *capturingSchedulerLogger) Debug(message string, context map[string]any) {
	l.append(message, context)
}
func (l *capturingSchedulerLogger) Info(message string, context map[string]any) {
	l.append(message, context)
}
func (l *capturingSchedulerLogger) Warn(message string, context map[string]any) {
	l.append(message, context)
}
func (l *capturingSchedulerLogger) Error(message string, context map[string]any) {
	l.append(message, context)
}

func (l *capturingSchedulerLogger) append(message string, context map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	copyContext := map[string]any{}
	for key, value := range context {
		copyContext[key] = value
	}
	l.entries = append(l.entries, schedulerLogEntry{message: message, context: copyContext})
}

func (l *capturingSchedulerLogger) requireMessage(t *testing.T, message string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.message == message {
			return
		}
	}
	t.Fatalf("logger messages = %#v, want %q", l.entries, message)
}

func (l *capturingSchedulerLogger) requireContextValue(t *testing.T, message, key string, want any) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if entry.message == message && entry.context[key] == want {
			return
		}
	}
	t.Fatalf("logger entries = %#v, want %q with %s=%v", l.entries, message, key, want)
}

func (s *queueStatusCheckingPlannerScheduler) DiscoverIssues(ctx context.Context, _ planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.t.Helper()
	item, err := s.repos.Queue.GetByID(ctx, s.queueItemID)
	if err != nil {
		s.t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if item == nil {
		s.t.Fatal("Queue.GetByID() = nil, want claimed queue item")
	}
	if item.Status != "running" {
		s.t.Fatalf("queue item status during discovery = %q, want running", item.Status)
	}
	if item.ClaimedBy == nil || *item.ClaimedBy != "scheduler" {
		s.t.Fatalf("queue item claimed_by during discovery = %v, want scheduler", item.ClaimedBy)
	}
	s.checkedClaimedStatus = true
	return planner.DiscoveryResult{}, nil
}

func (s *queueStatusCheckingPlannerScheduler) ProcessNext(context.Context, string) (*planner.ProcessResult, error) {
	return nil, nil
}

func (s *queueStatusCheckingPlannerScheduler) ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*planner.ProcessResult, error) {
	return nil, nil

}

func (s *stubPlannerScheduler) DiscoverIssues(_ context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return planner.DiscoveryResult{}, s.discoverErr
}

func (s *stubPlannerScheduler) ProcessNext(_ context.Context, claimedBy string) (*planner.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubPlannerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*planner.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &planner.ProcessResult{}, s.processErr
}

func (s *stubPlannerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubPlannerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

func (s *stubCoordinatorScheduler) DiscoverIssues(_ context.Context, input coordinator.DiscoveryInput) (coordinator.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return coordinator.DiscoveryResult{Ticked: true}, s.discoverErr
}

type stubReviewerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []reviewer.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

func (s *stubReviewerScheduler) DiscoverPullRequests(_ context.Context, input reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return reviewer.DiscoveryResult{}, s.discoverErr
}

func (s *stubReviewerScheduler) DiscoverPullRequest(_ context.Context, _ reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	return reviewer.DiscoveryResult{}, s.discoverErr
}

func (s *stubReviewerScheduler) ProcessNext(_ context.Context, claimedBy string) (*reviewer.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubReviewerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*reviewer.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &reviewer.ProcessResult{}, s.processErr
}

func (s *stubReviewerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubReviewerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubFixerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []fixer.DiscoveryInput
	processClaims  []string
	processedItems []string
	discoverErr    error
	processErr     error
}

func (s *stubFixerScheduler) DiscoverPullRequests(_ context.Context, input fixer.DiscoveryInput) (fixer.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return fixer.DiscoveryResult{}, s.discoverErr
}

func (s *stubFixerScheduler) DiscoverPullRequest(_ context.Context, _ fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, s.discoverErr
}

func (s *stubFixerScheduler) DiscoverPullRequestsForBaseBranchUpdate(_ context.Context, _ fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error) {
	return fixer.DiscoveryResult{}, s.discoverErr
}

func (s *stubFixerScheduler) ProcessNext(_ context.Context, claimedBy string) (*fixer.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubFixerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*fixer.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &fixer.ProcessResult{}, s.processErr
}

func (s *stubFixerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubFixerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

type stubSnapshotScheduler struct {
	err error
}

func (s stubSnapshotScheduler) CapturePullRequestSnapshot(_ context.Context, input githubinfra.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	return storage.PullRequestSnapshotRecord{ID: "snapshot_1", ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber, HeadSHA: "head", CapturedAt: input.CapturedAt, CreatedAt: input.CapturedAt}, s.err
}

type failingSnapshotUpsertQuerier struct {
	db  *sql.DB
	err error
}

func (q *failingSnapshotUpsertQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "INSERT INTO pull_request_snapshots") {
		return nil, q.err
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingSnapshotUpsertQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingSnapshotUpsertQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

type failingProjectLookupQuerier struct {
	db *sql.DB
}

func (q *failingProjectLookupQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingProjectLookupQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingProjectLookupQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "FROM projects WHERE id") {
		return q.db.QueryRowContext(ctx, `SELECT * FROM missing_projects_table`)
	}
	return q.db.QueryRowContext(ctx, query, args...)
}

type failingQueueCompleteQuerier struct {
	db  *sql.DB
	err error
}

func (q *failingQueueCompleteQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, "SET status = 'completed'") {
		return nil, q.err
	}
	return q.db.ExecContext(ctx, query, args...)
}

func (q *failingQueueCompleteQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *failingQueueCompleteQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func assertQueueRetryError(t *testing.T, item *storage.QueueItemRecord, message string) {
	t.Helper()
	lastError := "<nil>"
	if item.LastError != nil {
		lastError = *item.LastError
	}
	lastErrorKind := "<nil>"
	if item.LastErrorKind != nil {
		lastErrorKind = *item.LastErrorKind
	}
	if item.LastError == nil || !strings.Contains(*item.LastError, message) || item.LastErrorKind == nil || *item.LastErrorKind != "retryable_transient" {
		t.Fatalf("queue item error = (%s, %s), want retryable %s", lastError, lastErrorKind, message)
	}
}

type parallelWorkerScheduler struct {
	calls         int32
	secondStarted chan struct{}
}

func (s *parallelWorkerScheduler) ProcessNext(_ context.Context, _ string) (*worker.ProcessResult, error) {
	switch atomic.AddInt32(&s.calls, 1) {
	case 1:
		select {
		case <-s.secondStarted:
			return nil, nil
		case <-time.After(250 * time.Millisecond):
			return nil, errors.New("second worker item did not start concurrently")
		}
	case 2:
		close(s.secondStarted)
	}
	return nil, nil
}

func (s *parallelWorkerScheduler) ProcessClaimedQueueItem(ctx context.Context, _ storage.QueueItemRecord) (*worker.ProcessResult, error) {
	return s.ProcessNext(ctx, "")
}

type blockingWorkerScheduler struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingWorkerScheduler) ProcessNext(_ context.Context, _ string) (*worker.ProcessResult, error) {
	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	return nil, nil
}

func (s *blockingWorkerScheduler) ProcessClaimedQueueItem(ctx context.Context, _ storage.QueueItemRecord) (*worker.ProcessResult, error) {
	return s.ProcessNext(ctx, "")
}

type stubWorkerScheduler struct {
	mu             sync.Mutex
	discoverCalls  []worker.DiscoveryInput
	discoverErr    error
	processClaims  []string
	processedItems []string
	processErr     error
}

func (s *stubWorkerScheduler) DiscoverIssues(_ context.Context, input worker.DiscoveryInput) (worker.DiscoveryResult, error) {
	s.mu.Lock()
	s.discoverCalls = append(s.discoverCalls, input)
	s.mu.Unlock()
	return worker.DiscoveryResult{}, s.discoverErr
}

func (s *stubWorkerScheduler) ProcessNext(_ context.Context, claimedBy string) (*worker.ProcessResult, error) {
	s.mu.Lock()
	s.processClaims = append(s.processClaims, claimedBy)
	s.mu.Unlock()
	return nil, s.processErr
}

func (s *stubWorkerScheduler) ProcessClaimedQueueItem(_ context.Context, queueItem storage.QueueItemRecord) (*worker.ProcessResult, error) {
	s.mu.Lock()
	s.processedItems = append(s.processedItems, queueItem.ID)
	s.mu.Unlock()
	return &worker.ProcessResult{}, s.processErr
}

func (s *stubWorkerScheduler) processClaimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processClaims)
}

func (s *stubWorkerScheduler) processItemCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.processedItems)
}

func waitForSchedulerCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not satisfied before timeout")
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

type stubAgentExecution struct {
	result agent.Result
	err    error
}

func (s stubAgentExecution) Wait(context.Context) (agent.Result, error) {
	return s.result, s.err
}

func (s stubAgentExecution) Kill(string) error {
	return nil
}

type stubAgentExecutionStarter struct {
	execution agent.Execution
	err       error
}

func (s stubAgentExecutionStarter) Start(context.Context, agent.RunInput) (agent.Execution, error) {
	return s.execution, s.err
}

type cancellableWorkerScheduler struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *cancellableWorkerScheduler) ProcessNext(context.Context, string) (*worker.ProcessResult, error) {
	return nil, nil
}

func (s *cancellableWorkerScheduler) ProcessClaimedQueueItem(ctx context.Context, _ storage.QueueItemRecord) (*worker.ProcessResult, error) {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	close(s.cancelled)
	<-s.release
	return nil, ctx.Err()
}
