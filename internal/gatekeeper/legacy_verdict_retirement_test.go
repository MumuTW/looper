package gatekeeper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

func TestDiscoverPullRequestsRetiresLegacyAdviseVerdictEvenWhenEvaluationIsSkipped(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	pullRequest := githubinfra.PullRequestSummary{
		Number: 42, State: "OPEN", HeadSHA: "head-1", BaseRefName: "main", ReviewDecision: "APPROVED", UpdatedAt: fixture.now.Format("2006-01-02T15:04:05Z"),
	}
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{pullRequest}
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.comments = []githubinfra.CommentInfo{
		{ID: 1, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\n✅ **Merge gate: eligible.**"},
		{ID: 2, Author: "human", Body: legacyVerdictCommentMarker + "\nquoted for discussion"},
	}

	legacy := Report{
		Version: 2, Mode: "advise", ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Format("2006-01-02T15:04:05Z"),
		SourceFingerprint: sourceFingerprint(pullRequest),
	}
	projectID, entityType, entityID := legacy.ProjectID, "pull_request", "acme/looper#42"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: legacy, CreatedAt: fixture.now,
	}); err != nil {
		t.Fatalf("append legacy Gatekeeper report: %v", err)
	}

	result, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if result.Skipped != 1 || result.Evaluated != 0 {
		t.Fatalf("discovery result = %#v, want one skipped retirement pass", result)
	}
	if got := fixture.github.updateCommentCalls; len(got) != 1 || got[0].CommentID != 1 || got[0].Body != retiredLegacyVerdictBody {
		t.Fatalf("comment updates = %#v, want only the owned comment retired", got)
	}
	if got := fixture.github.commentsCalls; got != 1 {
		t.Fatalf("comment reads after first pass = %d, want one", got)
	}
	if got := fixture.github.comments[1].Body; got != legacyVerdictCommentMarker+"\nquoted for discussion" {
		t.Fatalf("human marker comment = %q, want unchanged", got)
	}

	if _, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if got := len(fixture.github.updateCommentCalls); got != 1 {
		t.Fatalf("retired comment updates = %d, want 1", got)
	}
	if got := fixture.github.commentsCalls; got != 1 {
		t.Fatalf("retired comment reads = %d, want no second-tick forge read", got)
	}
}

func TestDiscoverPullRequestsPersistsObserveReportWhenLegacyRetirementFails(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	pullRequest := githubinfra.PullRequestSummary{
		Number: 42, State: "OPEN", HeadSHA: "head-1", BaseRefName: "main", ReviewDecision: "APPROVED",
		UpdatedAt: fixture.now.Add(time.Minute).Format("2006-01-02T15:04:05Z"),
	}
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{pullRequest}
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.commentsErr = errors.New("GitHub comments unavailable")

	legacy := Report{
		Version: 2, Mode: "advise", ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Format("2006-01-02T15:04:05Z"),
		SourceFingerprint: sourceFingerprint(githubinfra.PullRequestSummary{Number: 42, HeadSHA: "old-head"}),
	}
	projectID, entityType, entityID := legacy.ProjectID, "pull_request", "acme/looper#42"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: legacy, CreatedAt: fixture.now,
	}); err != nil {
		t.Fatalf("append legacy Gatekeeper report: %v", err)
	}

	result, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v, want current report despite retirement failure", err)
	}
	if result.Evaluated != 1 || len(result.Reports) != 1 {
		t.Fatalf("discovery result = %#v, want one evaluated report", result)
	}
	if result.Reports[0].Version != reportVersion {
		t.Fatalf("report = %#v, want current observe report", result.Reports[0])
	}
}

func TestDiscoverPullRequestsRetiresHistoricalAdviseOutsideOpenPage(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.comments = []githubinfra.CommentInfo{{ID: 9, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\n✅ **Merge gate: eligible.**"}}
	legacy := Report{
		Version: 2, Mode: "advise", ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Format(time.RFC3339Nano),
	}
	projectID, entityType, entityID := legacy.ProjectID, "pull_request", "acme/looper#42"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: legacy, CreatedAt: fixture.now,
	}); err != nil {
		t.Fatalf("append legacy Gatekeeper report: %v", err)
	}

	result, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if result.Evaluated != 0 || result.Skipped != 0 {
		t.Fatalf("discovery result = %#v, want no open-page evaluations", result)
	}
	if len(fixture.github.updateCommentCalls) != 1 || fixture.github.updateCommentCalls[0].CommentID != 9 {
		t.Fatalf("comment updates = %#v, want historical PR retirement outside page", fixture.github.updateCommentCalls)
	}
}
