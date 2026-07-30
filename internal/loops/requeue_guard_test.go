package loops

import (
	"fmt"
	"sync"
	"testing"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

func (r *guardRegistry) size() int {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	return len(r.entries)
}

func TestPullRequestTargetGuardKeyMatchesLoopRecordKey(t *testing.T) {
	t.Parallel()
	repo := "acme/looper"
	prNumber := int64(42)
	targetID := "pr:acme/looper:42"
	loop := storage.LoopRecord{
		ProjectID:  "proj",
		Type:       string(domain.LoopTypeFixer),
		TargetType: string(domain.LoopTargetTypePullRequest),
		TargetID:   &targetID,
		Repo:       &repo,
		PRNumber:   &prNumber,
	}
	fromRecord := LoopTargetGuardKeyFromRecord(loop)
	fromPR := PullRequestTargetGuardKey("proj", repo, prNumber)
	if fromRecord == "" || fromRecord != fromPR {
		t.Fatalf("PR keys = record %q / explicit %q, want equal non-empty", fromRecord, fromPR)
	}
	// Cross-type identity: reviewer discovery and fixer discard share one mutex.
	reviewerKey := LoopTargetGuardKey("proj", string(domain.LoopTypeReviewer), string(domain.LoopTargetTypePullRequest), TargetKeyFromLoopRecord(loop))
	if reviewerKey != fromPR {
		t.Fatalf("reviewer key %q != fixer PR key %q", reviewerKey, fromPR)
	}
}

func TestLoopTargetGuardKeyOmitsTypeForPullRequest(t *testing.T) {
	t.Parallel()
	fixer := LoopTargetGuardKey("proj", "fixer", "pull_request", "pull_request:acme/looper:42")
	worker := LoopTargetGuardKey("proj", "worker", "pull_request", "pull_request:acme/looper:42")
	if fixer == "" || fixer != worker {
		t.Fatalf("PR keys = %q / %q, want equal non-empty shared key", fixer, worker)
	}
	issueFixer := LoopTargetGuardKey("proj", "fixer", "issue", "issue:acme/looper:7")
	issueWorker := LoopTargetGuardKey("proj", "worker", "issue", "issue:acme/looper:7")
	if issueFixer == "" || issueFixer == issueWorker {
		t.Fatalf("issue keys = %q / %q, want distinct type-scoped keys", issueFixer, issueWorker)
	}
	if got := LoopTargetGuardKey("proj", "worker", "project", "project:proj"); got != "" {
		t.Fatalf("project worker key = %q, want empty (concurrent workers)", got)
	}
}

func TestTargetLeaseKeyKeepsProjectWorkersIndependent(t *testing.T) {
	first := storage.LoopRecord{
		ID: "worker_1", ProjectID: "proj", Type: string(domain.LoopTypeWorker), TargetType: string(domain.LoopTargetTypeProject),
	}
	second := first
	second.ID = "worker_2"

	if got := TargetLeaseKeyFromRecord(first); got != "proj|worker:worker_1" {
		t.Fatalf("TargetLeaseKeyFromRecord(first) = %q, want project-scoped worker key", got)
	}
	if got := TargetLeaseKeyFromRecord(second); got != "proj|worker:worker_2" {
		t.Fatalf("TargetLeaseKeyFromRecord(second) = %q, want independent project worker key", got)
	}
}

func TestTargetLeaseKeySharesIssueAcrossRoles(t *testing.T) {
	targetID := "issue:acme/looper:7"
	planner := storage.LoopRecord{
		ID: "planner_1", ProjectID: "proj", Type: string(domain.LoopTypePlanner), TargetType: string(domain.LoopTargetTypeIssue), TargetID: &targetID,
	}
	worker := planner
	worker.ID = "worker_1"
	worker.Type = string(domain.LoopTypeWorker)

	if got := TargetLeaseKeyFromRecord(planner); got != "proj|issue:acme/looper:7" {
		t.Fatalf("TargetLeaseKeyFromRecord(planner) = %q, want issue target key", got)
	}
	if got := TargetLeaseKeyFromRecord(worker); got != "proj|issue:acme/looper:7" {
		t.Fatalf("TargetLeaseKeyFromRecord(worker) = %q, want shared issue target key", got)
	}
}

func TestGuardRegistryReclaimsEntriesAfterChurn(t *testing.T) {
	t.Parallel()

	registry := newGuardRegistry()
	for i := 0; i < 5000; i++ {
		release := registry.lock(fmt.Sprintf("loop_churn_%d", i))
		release()
	}

	if got := registry.size(); got != 0 {
		t.Fatalf("registry.size() = %d after churn, want 0", got)
	}
}

func TestGuardRegistryMutualExclusionSurvivesReclamation(t *testing.T) {
	t.Parallel()

	registry := newGuardRegistry()
	const goroutines = 8
	const iterations = 200
	counter := 0
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				release := registry.lock("loop_contended")
				counter++
				release()
			}
		}()
	}
	wg.Wait()

	if counter != goroutines*iterations {
		t.Fatalf("counter = %d, want %d: mutual exclusion violated across reclaim/recreate", counter, goroutines*iterations)
	}
	if got := registry.size(); got != 0 {
		t.Fatalf("registry.size() = %d after contention, want 0", got)
	}
}

func TestGuardRegistryReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	registry := newGuardRegistry()
	release := registry.lock("loop_idempotent")
	release()
	release()

	// The key must be lockable again after double release.
	again := registry.lock("loop_idempotent")
	again()
	if got := registry.size(); got != 0 {
		t.Fatalf("registry.size() = %d, want 0", got)
	}
}

func TestLockLoopRequeueAndTargetStillSerialize(t *testing.T) {
	t.Parallel()

	// Public wrappers over the reclaiming registry keep their contract: the
	// same key excludes concurrent holders, and empty target keys are no-ops.
	release := LockLoopRequeue("loop_public_api")
	locked := make(chan struct{})
	go func() {
		inner := LockLoopRequeue("loop_public_api")
		close(locked)
		inner()
	}()
	select {
	case <-locked:
		t.Fatal("second LockLoopRequeue acquired while first still held")
	default:
	}
	release()
	<-locked

	noop := LockLoopTarget("  ")
	noop()
}
