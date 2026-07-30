package storage

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// TestListLatestByEntityTypeAndEventTypesReturnsOneRowPerEntity is the cost
// defect: an entity is re-evaluated for as long as the daemon runs, so reading
// its whole lifecycle to keep only the last row puts the caller's cost on the
// lifetime of the daemon rather than on the number of entities.
//
// It also pins the partition. `owner/repo#12` names a pull request only within
// one project, so two projects can legitimately carry the identical entity id
// against different provider base URLs, and grouping on the id alone would
// silently drop one of them.
func TestListLatestByEntityTypeAndEventTypesReturnsOneRowPerEntity(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	entityType := "pull_request"
	entityID := "acme/looper#12"
	otherID := "acme/looper#13"
	projectA := "project_a"
	projectB := "project_b"
	for _, id := range []string{projectA, projectB} {
		if err := repos.Projects.Upsert(ctx, ProjectRecord{
			ID: id, Name: id, RepoPath: t.TempDir(),
			CreatedAt: "2026-04-11T11:00:00.000Z", UpdatedAt: "2026-04-11T11:00:00.000Z",
		}); err != nil {
			t.Fatalf("Projects.Upsert() error = %v", err)
		}
	}

	// One pull request evaluated many times, in one project.
	for i := 1; i <= 40; i++ {
		if err := repos.Events.Append(ctx, EventLogRecord{
			ID: fmt.Sprintf("event_a_%02d", i), EventType: "pull_request.merge_gate.evaluated",
			ProjectID: &projectA, EntityType: &entityType, EntityID: &entityID,
			PayloadJSON: fmt.Sprintf(`{"round":%d}`, i),
			CreatedAt:   fmt.Sprintf("2026-04-11T12:%02d:00.000Z", i),
		}); err != nil {
			t.Fatalf("Events.Append() error = %v", err)
		}
	}
	// A second pull request in the same project.
	if err := repos.Events.Append(ctx, EventLogRecord{
		ID: "event_a_other", EventType: "pull_request.merge_gate.evaluated",
		ProjectID: &projectA, EntityType: &entityType, EntityID: &otherID,
		PayloadJSON: `{"round":1}`, CreatedAt: "2026-04-11T12:05:00.000Z",
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}
	// The same entity id in a different project: a different physical repository
	// reachable at the same slug, and written last.
	if err := repos.Events.Append(ctx, EventLogRecord{
		ID: "event_b_1", EventType: "pull_request.merge_gate.evaluated",
		ProjectID: &projectB, EntityType: &entityType, EntityID: &entityID,
		PayloadJSON: `{"round":99}`, CreatedAt: "2026-04-11T13:00:00.000Z",
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}
	// An unrelated event type on the same entity must not become the answer.
	if err := repos.Events.Append(ctx, EventLogRecord{
		ID: "event_a_noise", EventType: "pull_request.comment.observed",
		ProjectID: &projectA, EntityType: &entityType, EntityID: &entityID,
		PayloadJSON: `{"round":-1}`, CreatedAt: "2026-04-11T14:00:00.000Z",
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}

	all, err := repos.Events.ListLatestByEntityTypeAndEventTypes(ctx, "", entityType, []string{"pull_request.merge_gate.evaluated"})
	if err != nil {
		t.Fatalf("ListLatestByEntityTypeAndEventTypes() error = %v", err)
	}
	gotIDs := make([]string, 0, len(all))
	for _, record := range all {
		gotIDs = append(gotIDs, record.ID)
	}
	// Exactly three rows for three (project, entity) pairs — not the 41 events
	// that produced them.
	if !slices.Equal(gotIDs, []string{"event_a_other", "event_a_40", "event_b_1"}) {
		t.Fatalf("records = %#v, want the latest row per project and entity", gotIDs)
	}

	scoped, err := repos.Events.ListLatestByEntityTypeAndEventTypes(ctx, projectB, entityType, []string{"pull_request.merge_gate.evaluated"})
	if err != nil {
		t.Fatalf("ListLatestByEntityTypeAndEventTypes(project) error = %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != "event_b_1" {
		t.Fatalf("scoped = %#v, want only the second project's row", scoped)
	}
}
