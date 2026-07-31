package webhookforward

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/gatekeeper"
)

func TestForwardTriggersGatekeeperFromHeadCheckAndReviewCompletion(t *testing.T) {
	repos := newTestRepositories(t)
	seedProject(t, repos, "project_1", "acme/looper")
	runner := &fakeTargetedGatekeeper{calls: make(chan gatekeeper.EvaluationInput, 3)}
	forwarder := New(Options{
		Repos: repos, Config: testConfig(t), Gatekeeper: runner,
		Reviewer:      newFakeTargetedRunner(nil),
		Fixer:         targetedFixerAdapter{runner: newFakeTargetedRunner(nil)},
		MaxConcurrent: 1, QueueCapacity: 8,
	})
	defer forwarder.Close()

	requests := []DeliveryRequest{
		{
			DeliveryID: "head-update", EventType: "pull_request",
			Payload: []byte(`{"action":"synchronize","repository":{"full_name":"acme/looper"},"pull_request":{"number":42,"head":{"sha":"head-2"}}}`),
		},
		{
			DeliveryID: "check-complete", EventType: "check_run",
			Payload: []byte(`{"action":"completed","repository":{"full_name":"acme/looper"},"check_run":{"head_sha":"head-2","conclusion":"success","pull_requests":[{"number":42}]}}`),
		},
		{
			DeliveryID: "review-complete", EventType: "pull_request_review",
			Payload: []byte(`{"action":"submitted","repository":{"full_name":"acme/looper"},"pull_request":{"number":42,"head":{"sha":"head-2"}}}`),
		},
	}
	for _, request := range requests {
		result, err := forwarder.Forward(context.Background(), request)
		if err != nil {
			t.Fatalf("Forward(%s) error = %v", request.DeliveryID, err)
		}
		if result.Status != "accepted" {
			t.Fatalf("Forward(%s) status = %q, want accepted", request.DeliveryID, result.Status)
		}
		waitGatekeeperCall(t, runner.calls)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.seen) != 3 {
		t.Fatalf("gatekeeper calls = %#v, want 3", runner.seen)
	}
	for _, call := range runner.seen {
		if call.ProjectID != "project_1" || call.Repo != "acme/looper" || call.PRNumber != 42 || call.ExpectedHeadSHA != "head-2" {
			t.Fatalf("gatekeeper call = %#v, want head-bound project_1/acme/looper#42", call)
		}
	}
}

type fakeTargetedGatekeeper struct {
	mu    sync.Mutex
	seen  []gatekeeper.EvaluationInput
	calls chan gatekeeper.EvaluationInput
}

func (f *fakeTargetedGatekeeper) EvaluatePullRequest(_ context.Context, input gatekeeper.EvaluationInput) (gatekeeper.Report, error) {
	f.mu.Lock()
	f.seen = append(f.seen, input)
	f.mu.Unlock()
	f.calls <- input
	return gatekeeper.Report{}, nil
}

func waitGatekeeperCall(t *testing.T, calls <-chan gatekeeper.EvaluationInput) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for gatekeeper evaluation")
	}
}
