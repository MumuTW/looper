package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MumuTW/looper/internal/storage"
)

func TestLoopRouteProjectsPersistedReviewerConvergence(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	projectID := "project_convergence"
	loopID := "loop_convergence"
	targetID := "pr:acme/looper:42"
	repo := "acme/looper"
	prNumber := int64(42)
	metadata := `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":5,"consecutiveUnproductive":2,"items":{"review-1":{"id":"review-1","severity":"blocking","status":"open","fixerAttempts":1},"review-2":{"id":"review-2","severity":"non_blocking","status":"resolved"}},"history":[{"number":4,"productive":true,"newItemIds":["review-2"],"openItemIds":["review-1"]},{"number":5,"productive":false,"openItemIds":["review-1"]}]},"action":"escalate","reason":"stalled","status":"awaiting_human","updatedAt":"2026-07-31T18:00:00.000Z"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Convergence", RepoPath: "/tmp/convergence", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 42, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID,
		Repo: &repo, PRNumber: &prNumber, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/loops/42", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	projection := data["convergence"].(map[string]any)
	assertEqual(t, projection["action"], "escalate")
	assertEqual(t, projection["reason"], "stalled")
	assertEqual(t, projection["status"], "awaiting_human")
	state := projection["state"].(map[string]any)
	assertEqual(t, state["totalRounds"], float64(5))
	assertEqual(t, state["consecutiveUnproductive"], float64(2))
	items := state["items"].(map[string]any)
	assertEqual(t, items["review-1"].(map[string]any)["status"], "open")
	history := state["history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history = %#v, want two round summaries", history)
	}
	policy := projection["policy"].(map[string]any)
	assertEqual(t, policy["severityFloor"], "non_blocking")
}

func TestLoopRouteOmitsMalformedReviewerConvergence(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	targetID := "project_malformed"
	metadata := `{"convergence":{"policy":{"maxTotalRounds":"not-a-number"}}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: targetID, Name: "Malformed", RepoPath: "/tmp/malformed", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_malformed", Seq: 43, ProjectID: targetID, Type: "reviewer", TargetType: "project", TargetID: &targetID, Status: "waiting", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/loops/43", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	if _, ok := data["convergence"]; ok {
		t.Fatalf("convergence = %#v, want omitted for malformed metadata", data["convergence"])
	}
}

// TestLoopRouteOmitsConvergenceWithValidPolicyButMalformedState verifies the
// projection validates the full record, not just the policy: a syntactically
// valid policy paired with negative counters or unknown enums must be omitted
// so the dashboard never surfaces nonsensical convergence progress.
func TestLoopRouteOmitsConvergenceWithValidPolicyButMalformedState(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	projectID := "project_malformed_state"
	loopID := "loop_malformed_state"
	targetID := "pr:acme/looper:44"
	repo := "acme/looper"
	prNumber := int64(44)
	cases := map[string]string{
		"negative counter":     `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":-1}}}`,
		"unknown item status":  `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"items":{"review-1":{"id":"review-1","severity":"blocking","status":"wontfix"}}}}}`,
		"unknown action":       `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":1},"action":"retry"}}`,
		"unknown status label": `{"convergence":{"policy":{"maxConsecutiveUnproductive":3,"maxFixerAttemptsPerItem":4,"maxTotalRounds":40,"severityFloor":"non_blocking"},"state":{"totalRounds":1},"status":"paused"}}`,
	}
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "MalformedState", RepoPath: "/tmp/malformed-state", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	seq := int64(100)
	for name, metadata := range cases {
		t.Run(name, func(t *testing.T) {
			seq++
			if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: seq, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", TargetID: &targetID, Repo: &repo, PRNumber: &prNumber, Status: "waiting", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/loops/%d", seq), nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
			data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
			if _, ok := data["convergence"]; ok {
				t.Fatalf("convergence = %#v, want omitted for %s", data["convergence"], name)
			}
		})
	}
}
