package reviewer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MumuTW/looper/internal/storage"
)

func TestRecordPublishedReviewProgressPersistsVerifiedOutcome(t *testing.T) {
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_review_event", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	runner := &Runner{db: fixture.coordinator.DB(), repos: fixture.repos, now: fixture.now}
	input := stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Loop: loop, Repo: repo, PRNumber: prNumber}
	if err := runner.recordPublishedReviewProgress(context.Background(), input, pendingReviewCheckpoint{HeadSHA: "head-1", Summary: "found blocker"}, ReviewEventComment, "blocking"); err != nil {
		t.Fatalf("recordPublishedReviewProgress() error = %v", err)
	}

	events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 1 || events[0].EventType != "pr.review.posted" {
		t.Fatalf("events = %#v, want one pr.review.posted event", events)
	}
	var payload struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(events[0].PayloadJSON), &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload.Outcome != "blocking" {
		t.Fatalf("event outcome = %q, want blocking", payload.Outcome)
	}
}
