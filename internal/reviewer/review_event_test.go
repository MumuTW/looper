package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
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
		Event   string `json:"event"`
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(events[0].PayloadJSON), &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload.Event != "COMMENT" {
		t.Fatalf("event = %q, want COMMENT (the verified review event must be persisted)", payload.Event)
	}
	if payload.Outcome != "blocking" {
		t.Fatalf("event outcome = %q, want blocking", payload.Outcome)
	}
}

// TestRunPublishStepPersistsVerifiedMarkerEventNotAgentNative covers the
// Reviewer-to-Gatekeeper contract: on the agent-native publish path the pending
// checkpoint carries the AGENT_NATIVE placeholder, but the durable pr.review.posted
// event must record the verified GitHub review event (here REQUEST_CHANGES) so
// Gatekeeper's latestCodexReviewForHead projects it instead of rejecting it and
// stranding the head on a stale clean outcome.
func TestRunPublishStepPersistsVerifiedMarkerEventNotAgentNative(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	prNumber := int64(42)
	loop := storage.LoopRecord{ID: "loop_publish_verified_event", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{reviewMarkerEvent: ReviewEventRequestChanges, reviewMarkerOutcome: "blocking"}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, ReviewEvents: config.ReviewerReviewEventsConfig{Blocking: config.ReviewerReviewEventRequestChanges}, LoopConfig: testReviewerLoopConfig()})

	if _, err := runner.runPublishStep(context.Background(), stepInput{
		Project:  storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Loop:     loop,
		Run:      storage.RunRecord{},
		Repo:     repo,
		PRNumber: prNumber,
		Checkpoint: reviewerCheckpoint{
			Snapshot:      &checkpointSnapshot{HeadSHA: "abc123"},
			PendingReview: &pendingReviewCheckpoint{HeadSHA: "abc123", IdempotencyKey: "reviewer:loop_publish_verified_event:abc123", Event: reviewEventAgentNative, Summary: "found blocker", Outcome: "blocking"},
		},
	}); err != nil {
		t.Fatalf("runPublishStep() error = %v", err)
	}

	events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	var posted map[string]any
	for _, event := range events {
		if event.EventType != "pr.review.posted" {
			continue
		}
		if err := json.Unmarshal([]byte(event.PayloadJSON), &posted); err != nil {
			t.Fatalf("decode event payload: %v", err)
		}
	}
	if posted == nil {
		t.Fatalf("no pr.review.posted event recorded, want the verified review event persisted")
	}
	if event, _ := posted["event"].(string); event != "REQUEST_CHANGES" {
		t.Fatalf("pr.review.posted event = %q, want REQUEST_CHANGES (the verified marker event, not the AGENT_NATIVE placeholder)", event)
	}
	if outcome, _ := posted["outcome"].(string); outcome != "blocking" {
		t.Fatalf("pr.review.posted outcome = %q, want blocking", outcome)
	}
	if raw, err := json.Marshal(posted); err == nil && strings.Contains(string(raw), "AGENT_NATIVE") {
		t.Fatalf("pr.review.posted payload %s must not carry the AGENT_NATIVE placeholder", raw)
	}
}
