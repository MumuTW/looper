package triager

import (
	"context"
	"fmt"
	"testing"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

func TestPendingForgeReadBudgetIsTickWideAndRotatesProjectTurns(t *testing.T) {
	t.Parallel()
	runner := &Runner{}
	projects := make([]string, 7)
	for i := range projects {
		projects[i] = fmt.Sprintf("project-%d", i)
	}

	first := runner.BeginPendingForgeReadTick(projects)
	for i := 0; i < 6; i++ {
		for call := 0; call < 2; call++ {
			if !first.Reserve(projects[i]) {
				t.Fatalf("first tick project %d call %d was denied", i, call)
			}
		}
	}
	if first.Reserve(projects[6]) {
		t.Fatal("seventh project reserved past the shared 12-call tick cap")
	}
	if got := first.Remaining(); got != 0 {
		t.Fatalf("first.Remaining() = %d, want 0", got)
	}

	second := runner.BeginPendingForgeReadTick(projects)
	for call := 0; call < 2; call++ {
		if !second.Reserve(projects[6]) {
			t.Fatalf("second tick rotating project call %d was denied", call)
		}
	}
}

func TestDiscoverIssuesChargesActualPendingForgeCalls(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.listEmpty = true
	fixture.github.details = map[int64]githubinfra.IssueDetail{}
	runner := fixture.runner()

	for index := 0; index < 10; index++ {
		number := int64(200 + index)
		detail, report := pendingReportFixture(fixture.now, number, ActionRoutePlanner)
		fixture.github.details[number] = detail
		if err := runner.persistReport(context.Background(), report); err != nil {
			t.Fatalf("persistReport(%d) error = %v", number, err)
		}
	}

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{
		ProjectID: "project_1", Repo: "acme/looper",
		PendingForgeReadBudget: NewPendingForgeReadBudget(pendingForgeReadLimit),
	})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.PendingForgeReads != pendingForgeReadLimit || result.PendingSourcesDeferred != 4 {
		t.Fatalf("result = %#v, want %d charged reads and 4 deferred sources", result, pendingForgeReadLimit)
	}
	if got := len(fixture.github.viewRequests) + len(fixture.github.timelineRequests) + fixture.github.permissionCalls; got != pendingForgeReadLimit {
		t.Fatalf("actual pending forge reads = %d (views=%d timelines=%d permissions=%d), want %d", got, len(fixture.github.viewRequests), len(fixture.github.timelineRequests), fixture.github.permissionCalls, pendingForgeReadLimit)
	}
	if len(fixture.planner.inputs) != 6 {
		t.Fatalf("planner calls = %d, want 6 sources completed before exact-call budget exhausted", len(fixture.planner.inputs))
	}
}

func TestDiscoverIssuesChargesEveryConfirmationPermissionLookup(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	fixture.github.listEmpty = true
	detail, report := pendingReportFixture(fixture.now, 300, ActionAwaitHuman)
	for index := 0; index < 20; index++ {
		detail.Comments = append(detail.Comments, githubinfra.CommentInfo{
			ID: int64(index + 1), Author: fmt.Sprintf("reader-%d", index),
			Body: confirmationCommand(report.ConfirmationToken), CreatedAt: fixture.now.Format(time.RFC3339Nano),
		})
	}
	fixture.github.details = map[int64]githubinfra.IssueDetail{detail.Number: detail}
	fixture.github.permission = "read"
	runner := fixture.runner()
	if err := runner.persistReport(context.Background(), report); err != nil {
		t.Fatal(err)
	}

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if result.PendingForgeReads != pendingForgeReadLimit || fixture.github.permissionCalls != pendingForgeReadLimit-2 || result.PendingSourcesDeferred != 1 {
		t.Fatalf("result/permission calls = %#v/%d, want every permission lookup charged", result, fixture.github.permissionCalls)
	}
}

func pendingReportFixture(now time.Time, number int64, action Action) (githubinfra.IssueDetail, Report) {
	createdAt := now.Add(-time.Hour).Add(time.Duration(number) * time.Millisecond).Format(time.RFC3339Nano)
	detail := githubinfra.IssueDetail{Number: number, Title: fmt.Sprintf("Pending issue %d", number), State: "open", URL: fmt.Sprintf("https://github.com/acme/looper/issues/%d", number), CreatedAt: createdAt}
	source := SourceEvent{Kind: sourceEventNew, OccurredAt: createdAt}
	report := Report{
		Version: 2, IdempotencyKey: buildIdempotencyKey("project_1", "acme/looper", number, source),
		ProjectID: "project_1", Repo: "acme/looper", IssueNumber: number, Source: source,
		Policy: PolicyDecision{Action: action}, ConfirmationToken: fmt.Sprintf("triage-confirm-%032d", number), CreatedAt: createdAt,
	}
	return detail, report
}
