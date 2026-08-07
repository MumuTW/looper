package storage

import (
	"context"
	"fmt"
	"testing"
)

func TestListTriageSourceStateWindowBoundsHistoryAndRotates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	projectID, entityType := "project_triage", "github_issue"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID: projectID, Name: "triage", RepoPath: t.TempDir(),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	appendEvent := func(id, eventType, payload, createdAt string) {
		t.Helper()
		if err := repos.Events.Append(ctx, EventLogRecord{
			ID: id, EventType: eventType, ProjectID: &projectID, EntityType: &entityType,
			PayloadJSON: payload, CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("Events.Append(%q) error = %v", id, err)
		}
	}

	// Terminal history is deliberately much larger than the requested page.
	// Routed timestamps sort before enrollments to prove terminality is event
	// existence for the same payload key, not textual time ordering.
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("aaa-terminal-%03d", i)
		appendEvent("enroll-"+key, "triage.enrolled", fmt.Sprintf(`{"idempotencyKey":%q,"projectId":%q,"repo":"acme/looper"}`, key, projectID), "z")
		appendEvent("route-"+key, "triage.routed", fmt.Sprintf(`{"reportKey":%q}`, key), "a")
	}
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("zzz-pending-%03d", i)
		appendEvent("enroll-"+key, "triage.enrolled", fmt.Sprintf(`{"idempotencyKey":%q,"projectId":%q,"repo":"acme/looper"}`, key, projectID), "m")
	}

	first, err := repos.Events.ListTriageSourceStateWindow(ctx, projectID, "", 3)
	if err != nil {
		t.Fatalf("ListTriageSourceStateWindow(first) error = %v", err)
	}
	if got := sourceKeys(first); fmt.Sprint(got) != "[aaa-terminal-000 aaa-terminal-001 aaa-terminal-002]" {
		t.Fatalf("first source keys = %v", got)
	}
	second, err := repos.Events.ListTriageSourceStateWindow(ctx, projectID, first[len(first)-1].SourceKey, 3)
	if err != nil {
		t.Fatalf("ListTriageSourceStateWindow(second) error = %v", err)
	}
	if got := sourceKeys(second); fmt.Sprint(got) != "[aaa-terminal-003 aaa-terminal-004 aaa-terminal-005]" {
		t.Fatalf("second source keys = %v", got)
	}

	terminal, err := repos.Events.ListTriageSourceStates(ctx, projectID, []string{"aaa-terminal-000"})
	if err != nil {
		t.Fatalf("ListTriageSourceStates() error = %v", err)
	}
	if len(terminal) != 1 || !terminal[0].Projected {
		t.Fatalf("terminal state = %#v, want projected", terminal)
	}
}

func TestListAwaitingTriageSourceStatesProjectsCurrentRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	projectID, entityType := "project_awaiting", "github_issue"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID: projectID, Name: "awaiting", RepoPath: t.TempDir(),
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	appendEvent := func(id, eventType, payload, createdAt string) {
		t.Helper()
		if err := repos.Events.Append(ctx, EventLogRecord{
			ID: id, EventType: eventType, ProjectID: &projectID, EntityType: &entityType,
			PayloadJSON: payload, CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("Events.Append(%q) error = %v", id, err)
		}
	}
	appendEvent("enroll-pending", "triage.enrolled", `{"idempotencyKey":"pending"}`, "2026-01-01T00:00:00Z")
	appendEvent("report-pending", "triage.report", `{"idempotencyKey":"pending","policy":{"action":"await_human_confirmation"}}`, "2026-01-01T00:01:00Z")
	appendEvent("enroll-confirmed", "triage.enrolled", `{"idempotencyKey":"confirmed"}`, "2026-01-01T00:02:00Z")
	appendEvent("report-confirmed", "triage.report", `{"idempotencyKey":"confirmed","policy":{"action":"await_human_confirmation"}}`, "2026-01-01T00:03:00Z")
	appendEvent("confirm-confirmed", "triage.confirmed", `{"reportKey":"confirmed"}`, "2026-01-01T00:04:00Z")
	appendEvent("enroll-routed", "triage.enrolled", `{"idempotencyKey":"routed"}`, "2026-01-01T00:05:00Z")
	appendEvent("report-routed", "triage.report", `{"idempotencyKey":"routed","policy":{"action":"route_planner"}}`, "2026-01-01T00:06:00Z")
	appendEvent("route-routed", "triage.routed", `{"reportKey":"routed"}`, "2026-01-01T00:07:00Z")

	records, err := repos.Events.ListAwaitingTriageSourceStates(ctx, projectID)
	if err != nil {
		t.Fatalf("ListAwaitingTriageSourceStates() error = %v", err)
	}
	if got := sourceKeys(records); fmt.Sprint(got) != "[pending]" {
		t.Fatalf("awaiting source keys = %v, want only pending", got)
	}
}

func sourceKeys(records []TriageSourceStateRecord) []string {
	keys := make([]string, len(records))
	for i := range records {
		keys[i] = records[i].SourceKey
	}
	return keys
}
