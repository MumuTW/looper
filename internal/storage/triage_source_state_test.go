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

func sourceKeys(records []TriageSourceStateRecord) []string {
	keys := make([]string, len(records))
	for i := range records {
		keys[i] = records[i].SourceKey
	}
	return keys
}
