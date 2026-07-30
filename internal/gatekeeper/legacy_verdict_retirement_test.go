package gatekeeper

import (
	"context"
	"testing"

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
	if got := fixture.github.comments[1].Body; got != legacyVerdictCommentMarker+"\nquoted for discussion" {
		t.Fatalf("human marker comment = %q, want unchanged", got)
	}

	if _, err := fixture.runner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if got := len(fixture.github.updateCommentCalls); got != 1 {
		t.Fatalf("retired comment updates = %d, want 1", got)
	}
}
