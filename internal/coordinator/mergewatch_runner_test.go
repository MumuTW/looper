package coordinator

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/coordinator/mergewatch"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

func TestRecordPostMergeEventPreservesForgeMergedAtAndIsIdempotent(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	forgeMergedAt := "2026-05-14T11:58:07.000Z"
	snapshot := mergewatch.PRSnapshot{Repo: "acme/looper", PRNumber: 42, HeadSHA: "head-42", MergedAt: forgeMergedAt, Merged: true}
	if err := fixture.runner.recordPostMergeEvent(context.Background(), fixture.projectID, snapshot.Repo, 7, snapshot); err != nil {
		t.Fatalf("recordPostMergeEvent() error = %v", err)
	}
	if err := fixture.runner.recordPostMergeEvent(context.Background(), fixture.projectID, snapshot.Repo, 7, snapshot); err != nil {
		t.Fatalf("second recordPostMergeEvent() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v, err=%v, want one idempotent merge event", events, err)
	}
	if events[0].EventType != eventlog.CoordinatorPullRequestMergedEventType || events[0].CreatedAt != "2026-05-14T12:00:00.000Z" {
		t.Fatalf("event = %#v, want durable coordinator observation", events[0])
	}
	if !containsAll(events[0].PayloadJSON, `"mergedAt":"`+forgeMergedAt+`"`, `"headSha":"head-42"`) {
		t.Fatalf("payload = %s, want forge mergedAt and head", events[0].PayloadJSON)
	}
}

func TestApplyMergeWatchRecordsMergifyMergeEvidence(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	fixture.github.prDetails[42] = githubinfra.PullRequestDetail{
		Number: 42, Body: "Closes #7", State: "closed", HeadSHA: "head-42", BaseRefName: "main",
		Labels: []string{labels.DefaultPlanTrigger, labels.AutoMerge}, MergedAt: "2026-05-14T11:58:07.000Z",
		Mergeable: boolPtr(true), MergeableState: "clean",
	}
	loaded := []loadedIssue{{
		detail:      githubinfra.IssueDetail{Number: 7, Labels: []string{"triaged"}},
		rawTimeline: []map[string]any{{"source": map[string]any{"issue": map[string]any{"pull_request": map[string]any{"number": 42}}}}},
	}}
	if _, err := fixture.runner.applyMergeWatch(context.Background(), fixture.projectID, "acme/looper", t.TempDir(), loaded, fixture.cfg.Roles); err != nil {
		t.Fatalf("applyMergeWatch() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil || len(events) != 1 || events[0].EventType != eventlog.CoordinatorPullRequestMergedEventType {
		t.Fatalf("merge events = %#v, %v, want one Coordinator merge event", events, err)
	}
}
