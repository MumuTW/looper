package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

func TestGatekeeperVerdictsRouteProjectsNewestCausalReportPerPullRequest(t *testing.T) {
	fixture := newTestFixture(t)
	services := fixture.runtime.Services()
	createdAt := fixture.now.UTC().Format(javaScriptISOString)
	for _, projectID := range []string{"project_a", "project_b"} {
		if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{
			ID: projectID, Name: projectID, RepoPath: "/tmp/" + projectID, CreatedAt: createdAt, UpdatedAt: createdAt,
		}); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", projectID, err)
		}
	}

	entityType := "pull_request"
	appendReport := func(id, projectID, repo string, prNumber int64, status string, eligible bool, evaluatedAt, eventCreatedAt string) {
		reportJSON, err := json.Marshal(gatekeeper.Report{
			Version: 2, Mode: "advise", Status: status, Eligible: eligible,
			ProjectID: projectID, Repo: repo, PRNumber: prNumber,
			ObservedHeadSHA: "head-" + id, RequiresFreshRevalidation: true,
			Reasons: func() []gatekeeper.Reason {
				if eligible {
					return []gatekeeper.Reason{}
				}
				return []gatekeeper.Reason{{Code: gatekeeper.ReasonHold, Subject: labels.HoldGlobal}}
			}(),
			Evidence:    gatekeeper.Evidence{PullRequestState: "OPEN", BaseRefName: "main", ProjectPolicyPermitsTarget: true},
			EvaluatedAt: evaluatedAt,
		})
		if err != nil {
			t.Fatalf("marshal report %s: %v", id, err)
		}
		entityID := fmt.Sprintf("%s#%d", repo, prNumber)
		if err := services.Repositories.Events.Append(context.Background(), storage.EventLogRecord{
			ID: id, EventType: gatekeeper.GateReportEventType, ProjectID: &projectID,
			EntityType: &entityType, EntityID: &entityID, PayloadJSON: string(reportJSON), CreatedAt: eventCreatedAt,
		}); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", id, err)
		}
	}

	appendReport("old", "project_a", "acme/looper", 42, gatekeeper.StatusEligible, true, "2026-04-11T12:00:00.000Z", "2026-04-11T12:00:00.000Z")
	appendReport("new", "project_a", "acme/looper", 42, gatekeeper.StatusBlocked, false, "2026-04-11T12:02:00.000Z", "2026-04-11T12:02:00.000Z")
	appendReport("other", "project_a", "acme/looper", 43, gatekeeper.StatusEligible, true, "2026-04-11T12:01:00.000Z", "2026-04-11T12:01:00.000Z")
	appendReport("foreign", "project_b", "acme/looper", 42, gatekeeper.StatusEligible, true, "2026-04-11T12:03:00.000Z", "2026-04-11T12:03:00.000Z")

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/gatekeeper/verdicts?projectId=project_a&limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %#v, want one newest verdict after the limit", items)
	}
	item := items[0].(map[string]any)
	assertEqual(t, item["id"], "new")
	assertEqual(t, item["projectId"], "project_a")
	assertEqual(t, item["repo"], "acme/looper")
	assertEqual(t, item["prNumber"], float64(42))
	assertEqual(t, item["status"], gatekeeper.StatusBlocked)
	assertEqual(t, item["eligible"], false)
	reasons := item["reasons"].([]any)
	assertEqual(t, reasons[0].(map[string]any)["code"], string(gatekeeper.ReasonHold))
	assertEqual(t, item["observedHeadSha"], "head-new")
	assertEqual(t, item["evaluatedAt"], "2026-04-11T12:02:00.000Z")
}

func TestGatekeeperVerdictsRouteRejectsOversizedLimit(t *testing.T) {
	fixture := newTestFixture(t)
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/gatekeeper/verdicts?limit=201", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], false)
	assertEqual(t, body["error"].(map[string]any)["code"], "VALIDATION_FAILED")
}
