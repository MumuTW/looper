package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
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

	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("ReconcileLegacyVerdictComments() error = %v", err)
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

	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("second ReconcileLegacyVerdictComments() error = %v", err)
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
	scanEvents, err := fixture.repos.Events.ListByEntity(context.Background(), "project", "project_1")
	if err != nil {
		t.Fatalf("scan completion events: %v", err)
	}
	if len(scanEvents) != 1 || scanEvents[0].EventType != legacyVerdictRetirementScanEventType {
		t.Fatalf("scan completion events = %#v, want one project watermark", scanEvents)
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

	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("ReconcileLegacyVerdictComments() error = %v, want warning-only failure", err)
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

	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("ReconcileLegacyVerdictComments() error = %v", err)
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

func TestLegacyVerdictRetirementCompletionInvalidatesForNewerAdvice(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.comments = []githubinfra.CommentInfo{{ID: 10, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\nold advice"}}
	appendReport := func(fingerprint string, offset time.Duration) {
		report := Report{
			Version: 2, Mode: "advise", ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
			Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Add(offset).Format(time.RFC3339Nano), SourceFingerprint: fingerprint,
		}
		projectID, entityType, entityID := report.ProjectID, "pull_request", "acme/looper#42"
		if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
			EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: report, CreatedAt: fixture.now.Add(offset),
		}); err != nil {
			t.Fatalf("append advise report: %v", err)
		}
	}
	appendReport("fingerprint-one", 0)
	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("first reconciliation error = %v", err)
	}
	if len(fixture.github.updateCommentCalls) != 1 {
		t.Fatalf("first retirement updates = %d, want one", len(fixture.github.updateCommentCalls))
	}

	fixture.github.comments[0].Body = legacyVerdictCommentMarker + "\nnew advice"
	appendReport("fingerprint-two", time.Minute)
	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("second reconciliation error = %v", err)
	}
	if len(fixture.github.updateCommentCalls) != 2 {
		t.Fatalf("retirement updates = %d, want newer advice to invalidate stale completion", len(fixture.github.updateCommentCalls))
	}
}

func TestLegacyVerdictRetirementRechecksAbsentCommentBeforeCompletion(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.currentUserLogin = "looper-bot"
	appendReport := func(projectID string, linked bool) {
		report := Report{
			Version: 2, Mode: "advise", ProjectID: projectID, Repo: "acme/looper", PRNumber: 42,
			Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Format(time.RFC3339Nano), SourceFingerprint: "race",
		}
		entityType, entityID := "pull_request", "acme/looper#42"
		var projectRef *string
		if linked {
			projectRef = &projectID
		}
		if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
			EventType: GateReportEventType, ProjectID: projectRef, EntityType: &entityType, EntityID: &entityID,
			Payload: report, CreatedAt: fixture.now,
		}); err != nil {
			t.Fatalf("append advise report: %v", err)
		}
	}
	appendReport("project_1", true)

	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("first ReconcileLegacyVerdictComments() error = %v", err)
	}
	scans, err := fixture.repos.Events.ListByEntity(context.Background(), "project", "project_1")
	if err != nil {
		t.Fatalf("scan events: %v", err)
	}
	if len(scans) != 0 {
		t.Fatalf("scan events after provisional absence = %#v, want none", scans)
	}

	fixture.github.comments = []githubinfra.CommentInfo{{ID: 7, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\n✅ old advice"}}
	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("second ReconcileLegacyVerdictComments() error = %v", err)
	}
	if len(fixture.github.updateCommentCalls) != 1 {
		t.Fatalf("comment updates = %d, want the late-published comment retired", len(fixture.github.updateCommentCalls))
	}
	scans, err = fixture.repos.Events.ListByEntity(context.Background(), "project", "project_1")
	if err != nil {
		t.Fatalf("scan events after confirmation: %v", err)
	}
	if len(scans) != 1 || scans[0].EventType != legacyVerdictRetirementScanEventType {
		t.Fatalf("scan events after confirmation = %#v, want one completion", scans)
	}
}

func TestLegacyVerdictRetirementCollapsesHistoricalReportsPerPullRequest(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.comments = []githubinfra.CommentInfo{{ID: 8, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\n✅ old advice"}}
	for index, fingerprint := range []string{"old", "new"} {
		report := Report{
			Version: 2, Mode: "advise", ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
			Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano), SourceFingerprint: fingerprint,
		}
		projectID, entityType, entityID := report.ProjectID, "pull_request", "acme/looper#42"
		if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
			EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
			Payload: report, CreatedAt: fixture.now.Add(time.Duration(index) * time.Minute),
		}); err != nil {
			t.Fatalf("append advise report %d: %v", index, err)
		}
	}
	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("ReconcileLegacyVerdictComments() error = %v", err)
	}
	if fixture.github.commentsCalls != 1 || len(fixture.github.updateCommentCalls) != 1 {
		t.Fatalf("forge retirement calls = comments %d updates %d, want one each", fixture.github.commentsCalls, len(fixture.github.updateCommentCalls))
	}
}

func TestLegacyVerdictRetirementUsesFallbackCWDForRemovedCheckout(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.comments = []githubinfra.CommentInfo{{ID: 9, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\n✅ old advice"}}
	missingPath := filepath.Join(t.TempDir(), "removed-checkout")
	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: missingPath, CreatedAt: fixture.now.Format(time.RFC3339Nano), UpdatedAt: fixture.now.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	report := Report{Version: 2, Mode: "advise", ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Format(time.RFC3339Nano)}
	projectID, entityType, entityID := report.ProjectID, "pull_request", "acme/looper#42"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: report, CreatedAt: fixture.now}); err != nil {
		t.Fatalf("append advise report: %v", err)
	}
	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("ReconcileLegacyVerdictComments() error = %v", err)
	}
	if len(fixture.github.updateCommentCalls) != 1 || fixture.github.updateCommentCalls[0].CWD != "" {
		t.Fatalf("comment update = %#v, want empty fallback CWD", fixture.github.updateCommentCalls)
	}
}

func TestLegacyVerdictRetirementProcessesOrphanedReportEvents(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.comments = []githubinfra.CommentInfo{{ID: 10, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\n✅ old advice"}}
	report := Report{Version: 2, Mode: "advise", ProjectID: "deleted-project", Repo: "acme/looper", PRNumber: 42, Status: StatusEligible, Eligible: true, EvaluatedAt: fixture.now.Format(time.RFC3339Nano)}
	entityType, entityID := "pull_request", "acme/looper#42"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{EventType: GateReportEventType, EntityType: &entityType, EntityID: &entityID, Payload: report, CreatedAt: fixture.now}); err != nil {
		t.Fatalf("append orphan advise report: %v", err)
	}
	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("ReconcileLegacyVerdictComments() error = %v", err)
	}
	if len(fixture.github.updateCommentCalls) != 1 {
		t.Fatalf("comment updates = %d, want orphaned report retired", len(fixture.github.updateCommentCalls))
	}
	scans, err := fixture.repos.Events.ListByEntity(context.Background(), "project", "deleted-project")
	if err != nil {
		t.Fatalf("orphan scan events: %v", err)
	}
	if len(scans) != 1 || scans[0].EventType != legacyVerdictRetirementScanEventType {
		t.Fatalf("orphan scan events = %#v, want one completion", scans)
	}
}

func TestLegacyVerdictRetirementMergesLinkedAndOrphanedHistory(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.currentUserLogin = "looper-bot"
	fixture.github.comments = []githubinfra.CommentInfo{{ID: 11, Author: "looper-bot", Body: legacyVerdictCommentMarker + "\n✅ old advice"}}
	appendReport := func(eventProjectID *string, evaluatedAt, fingerprint string) string {
		report := Report{Version: 2, Mode: "advise", ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, Status: StatusEligible, Eligible: true, EvaluatedAt: evaluatedAt, SourceFingerprint: fingerprint}
		entityType, entityID := "pull_request", "acme/looper#42"
		if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{EventType: GateReportEventType, ProjectID: eventProjectID, EntityType: &entityType, EntityID: &entityID, Payload: report, CreatedAt: fixture.now}); err != nil {
			t.Fatalf("append advise report: %v", err)
		}
		events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", entityID)
		if err != nil {
			t.Fatalf("list report events: %v", err)
		}
		return events[len(events)-1].ID
	}
	linkedProjectID := "project_1"
	appendReport(&linkedProjectID, fixture.now.Format(time.RFC3339Nano), "linked")
	latestEventID := appendReport(nil, fixture.now.Add(time.Minute).Format(time.RFC3339Nano), "orphan")

	if err := fixture.runner().ReconcileLegacyVerdictComments(context.Background()); err != nil {
		t.Fatalf("ReconcileLegacyVerdictComments() error = %v", err)
	}
	if fixture.github.commentsCalls != 1 || len(fixture.github.updateCommentCalls) != 1 {
		t.Fatalf("forge retirement calls = comments %d updates %d, want one newest candidate", fixture.github.commentsCalls, len(fixture.github.updateCommentCalls))
	}
	retirements, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("retirement events: %v", err)
	}
	var retirement legacyVerdictRetirement
	for _, event := range retirements {
		if event.EventType == legacyVerdictRetirementEventType {
			if err := json.Unmarshal([]byte(event.PayloadJSON), &retirement); err != nil {
				t.Fatalf("decode retirement event: %v", err)
			}
		}
	}
	if retirement.ReportEventID != latestEventID {
		t.Fatalf("retirement report event = %q, want newest orphan event %q", retirement.ReportEventID, latestEventID)
	}
}
