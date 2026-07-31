package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

func TestGatekeeperAgreementsRouteProjectsCausalEvents(t *testing.T) {
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
	appendAgreement := func(id, projectID, repo, outcome, recordedAt, eventCreatedAt string) {
		entityID := repo + "#42"
		agreementJSON, err := json.Marshal(gatekeeper.AdviceAgreement{
			Version: 1, VerdictEventID: "verdict_" + id, ProjectID: projectID, Repo: repo, PRNumber: 42,
			VerdictEligible: true, VerdictHeadSHA: "head-1", Outcome: gatekeeper.AdviceOutcome(outcome),
			Agreement: outcome == string(gatekeeper.AdviceOutcomeMergedAsIs), TerminalState: "MERGED",
			TerminalHeadSHA: "head-1", TerminalAt: recordedAt, RecordedAt: recordedAt,
		})
		if err != nil {
			t.Fatalf("marshal agreement %s: %v", id, err)
		}
		if err := services.Repositories.Events.Append(context.Background(), storage.EventLogRecord{
			ID: id, EventType: gatekeeper.AdviceAgreementEventType, ProjectID: &projectID,
			EntityType: &entityType, EntityID: &entityID, PayloadJSON: string(agreementJSON), CreatedAt: eventCreatedAt,
		}); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", id, err)
		}
	}

	appendAgreement("agreement_old", "project_a", "acme/looper", string(gatekeeper.AdviceOutcomeMergedAfterChange), "2026-04-11T12:01:00.000Z", "2026-04-11T12:01:00.000Z")
	appendAgreement("agreement_new", "project_a", "acme/looper", string(gatekeeper.AdviceOutcomeMergedAsIs), "2026-04-11T12:02:00.000Z", "2026-04-11T12:02:00.000Z")
	appendAgreement("agreement_other", "project_b", "acme/other", string(gatekeeper.AdviceOutcomeClosed), "2026-04-11T12:03:00.000Z", "2026-04-11T12:03:00.000Z")

	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/gatekeeper/agreements?projectId=project_a&limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	data := parseJSONMap(t, recorder.Body.Bytes())["data"].(map[string]any)
	items := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %#v, want newest project-a agreement only", items)
	}
	item := items[0].(map[string]any)
	assertEqual(t, item["id"], "agreement_new")
	assertEqual(t, item["projectId"], "project_a")
	assertEqual(t, item["verdictEventId"], "verdict_agreement_new")
	assertEqual(t, item["outcome"], string(gatekeeper.AdviceOutcomeMergedAsIs))
	assertEqual(t, item["agreement"], true)
	assertEqual(t, item["createdAt"], "2026-04-11T12:02:00.000Z")
}

func TestGatekeeperAgreementsRouteRejectsOversizedLimit(t *testing.T) {
	fixture := newTestFixture(t)
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/gatekeeper/agreements?limit=201", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], false)
	assertEqual(t, body["error"].(map[string]any)["code"], "VALIDATION_FAILED")
}
