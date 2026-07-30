package webhookforward

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/worker"
)

type fakePlanner struct {
	mu    sync.Mutex
	calls []planner.DiscoveryInput
}

func (f *fakePlanner) DiscoverIssues(ctx context.Context, input planner.DiscoveryInput) (planner.DiscoveryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, input)
	return planner.DiscoveryResult{}, nil
}

func (f *fakePlanner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeWorker struct {
	mu    sync.Mutex
	calls []worker.DiscoveryInput
}

func (f *fakeWorker) DiscoverIssues(ctx context.Context, input worker.DiscoveryInput) (worker.DiscoveryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, input)
	return worker.DiscoveryResult{}, nil
}

func (f *fakeWorker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func issuePayload(action, repo string, issueNumber int64) []byte {
	return []byte(`{"action":"` + action + `","repository":{"full_name":"` + repo + `"},"issue":{"number":` + itoa(issueNumber) + `}}`)
}

func TestForwardRoutesIssueLabelChangesToPlannerAndWorker(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	planner := &fakePlanner{}
	worker := &fakeWorker{}
	forwarder := New(Options{
		Repos:           repos,
		Config:          testConfig(t),
		Planner:         planner,
		WorkerDiscovery: worker,
		MaxConcurrent:   1,
		QueueCapacity:   8,
	})
	defer forwarder.Close()

	// Use different issue numbers to avoid coalescing (same key = same issue)
	for i, action := range []string{"labeled", "unlabeled"} {
		if _, err := forwarder.Forward(context.Background(), DeliveryRequest{
			DeliveryID: "label-" + action,
			EventType:  "issues",
			Payload:    issuePayload(action, "acme/looper", int64(42+i)),
		}); err != nil {
			t.Fatalf("Forward(%s) error = %v", action, err)
		}
	}

	// Wait for async processing
	time.Sleep(200 * time.Millisecond)

	if planner.callCount() != 2 {
		t.Fatalf("planner calls = %d, want 2", planner.callCount())
	}
	if worker.callCount() != 2 {
		t.Fatalf("worker calls = %d, want 2", worker.callCount())
	}
}

func TestForwardRoutesIssueAssignChangesToPlannerAndWorker(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	planner := &fakePlanner{}
	worker := &fakeWorker{}
	forwarder := New(Options{
		Repos:           repos,
		Config:          testConfig(t),
		Planner:         planner,
		WorkerDiscovery: worker,
		MaxConcurrent:   1,
		QueueCapacity:   8,
	})
	defer forwarder.Close()

	for i, action := range []string{"assigned", "unassigned"} {
		if _, err := forwarder.Forward(context.Background(), DeliveryRequest{
			DeliveryID: "assign-" + action,
			EventType:  "issues",
			Payload:    issuePayload(action, "acme/looper", int64(50+i)),
		}); err != nil {
			t.Fatalf("Forward(%s) error = %v", action, err)
		}
	}

	time.Sleep(200 * time.Millisecond)

	if planner.callCount() != 2 {
		t.Fatalf("planner calls = %d, want 2", planner.callCount())
	}
	if worker.callCount() != 2 {
		t.Fatalf("worker calls = %d, want 2", worker.callCount())
	}
}

func TestForwardIgnoresUnsupportedIssueActions(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	planner := &fakePlanner{}
	worker := &fakeWorker{}
	forwarder := New(Options{
		Repos:           repos,
		Config:          testConfig(t),
		Planner:         planner,
		WorkerDiscovery: worker,
		MaxConcurrent:   1,
		QueueCapacity:   8,
	})
	defer forwarder.Close()

	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{
		DeliveryID: "issue-opened",
		EventType:  "issues",
		Payload:    issuePayload("opened", "acme/looper", 42),
	}); err != nil {
		t.Fatalf("Forward(opened) error = %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if planner.callCount() != 0 {
		t.Fatalf("planner calls = %d, want 0", planner.callCount())
	}
	if worker.callCount() != 0 {
		t.Fatalf("worker calls = %d, want 0", worker.callCount())
	}
}

func TestForwardRejectsIssueMissingRepoOrNumber(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	planner := &fakePlanner{}
	worker := &fakeWorker{}
	forwarder := New(Options{
		Repos:           repos,
		Config:          testConfig(t),
		Planner:         planner,
		WorkerDiscovery: worker,
		MaxConcurrent:   1,
		QueueCapacity:   8,
	})
	defer forwarder.Close()

	// Missing repo
	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{
		DeliveryID: "issue-no-repo",
		EventType:  "issues",
		Payload:    []byte(`{"action":"labeled","repository":{},"issue":{"number":42}}`),
	}); err == nil {
		t.Fatal("expected error for missing repo")
	}

	// Missing number
	if _, err := forwarder.Forward(context.Background(), DeliveryRequest{
		DeliveryID: "issue-no-number",
		EventType:  "issues",
		Payload:    []byte(`{"action":"labeled","repository":{"full_name":"acme/looper"},"issue":{}}`),
	}); err == nil {
		t.Fatal("expected error for missing number")
	}
}
