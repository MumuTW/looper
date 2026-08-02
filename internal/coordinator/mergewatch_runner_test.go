package coordinator

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/coordinator/mergewatch"
	"github.com/MumuTW/looper/internal/eventlog"
)

func TestRecordPostMergeEventPreservesForgeMergedAt(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	forgeMergedAt := "2026-05-14T11:58:07.000Z"
	if err := fixture.runner.recordPostMergeEvent(context.Background(), fixture.projectID, "acme/looper", 42, mergewatch.PRSnapshot{
		Repo: "acme/looper", PRNumber: 42, HeadSHA: "head-42", Merged: true, MergedAt: forgeMergedAt,
	}); err != nil {
		t.Fatalf("recordPostMergeEvent() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v, err=%v, want one merge event", events, err)
	}
	if events[0].EventType != eventlog.CoordinatorPullRequestMergedEventType || events[0].CreatedAt != "2026-05-14T12:00:00.000Z" {
		t.Fatalf("event = %#v, want durable observation timestamp", events[0])
	}
	if !containsAll(events[0].PayloadJSON, `"mergedAt":"`+forgeMergedAt+`"`, `"headSha":"head-42"`) {
		t.Fatalf("payload = %s, want forge mergedAt and head", events[0].PayloadJSON)
	}
}
