package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/auditor"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
)

type fakeAuditorGateway struct {
	head   string
	checks githubinfra.PullRequestCheckRuns
}

func (f fakeAuditorGateway) GetBranchHeadSHA(context.Context, githubinfra.BranchHeadInput) (string, error) {
	return f.head, nil
}

func (f fakeAuditorGateway) ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error) {
	return f.checks, nil
}

func TestObservePostMergeFailureRecordsOneOptInDefaultBranchObservation(t *testing.T) {
	ctx := context.Background()
	workdir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workdir, "auditor.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	projectID, repo, head, base := "project_1", "acme/looper", "base-sha", "main"
	nowISO := eventlog.FormatJavaScriptISOString(now)
	metadata := `{"repo":"acme/looper"}`
	project := storage.ProjectRecord{ID: projectID, Name: "Demo", RepoPath: workdir, BaseBranch: &base, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatalf("Upsert project error = %v", err)
	}
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{EventType: gatekeeper.MergeOutcomeEventType, ProjectID: &projectID, Payload: gatekeeper.MergeOutcome{Version: 1, ProjectID: projectID, Repo: repo, PRNumber: 42, HeadSHA: "pr-sha", Merged: true}, CreatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("Append merge outcome error = %v", err)
	}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	gateway := fakeAuditorGateway{head: head, checks: githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}}}}
	if err := observePostMergeFailure(ctx, repos, gateway, project, repo, base, role, func() time.Time { return now }); err != nil {
		t.Fatalf("observePostMergeFailure() error = %v", err)
	}
	entityType, entityID := "branch_head", repo+"@"+head
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil || len(events) != 1 || events[0].EventType != auditor.ObservedFailureEventType {
		t.Fatalf("ListByEntity() = %#v, %v", events, err)
	}
	var observation auditor.FailureObservation
	if err := json.Unmarshal([]byte(events[0].PayloadJSON), &observation); err != nil {
		t.Fatal(err)
	}
	if observation.ProjectID != projectID || observation.HeadSHA != head || len(observation.CandidatePRs) != 1 || observation.CandidatePRs[0] != 42 || len(observation.FailedChecks) != 1 || observation.FailedChecks[0] != "ci" {
		t.Fatalf("observation = %#v", observation)
	}
	if err := observePostMergeFailure(ctx, repos, gateway, project, repo, base, role, func() time.Time { return now }); err != nil {
		t.Fatalf("second observePostMergeFailure() error = %v", err)
	}
	events, _ = repos.Events.ListByEntity(ctx, entityType, entityID)
	if len(events) != 1 {
		t.Fatalf("observation events = %#v, want one idempotent record", events)
	}
}

func TestObservePostMergeFailureSkipsDisabledProject(t *testing.T) {
	ctx := context.Background()
	coordinator := openMigratedCoordinator(t, filepath.Join(t.TempDir(), "auditor.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	project := storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}
	gateway := fakeAuditorGateway{head: "base", checks: githubinfra.PullRequestCheckRuns{}}
	if err := observePostMergeFailure(ctx, repos, gateway, project, "acme/looper", "main", config.AuditorRoleConfig{}, time.Now); err != nil {
		t.Fatalf("disabled observe error = %v", err)
	}
	events, err := repos.Events.List(ctx, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after disabled observer = %#v, %v", events, err)
	}
}

func TestFailedAuditorChecksKeepsOnlyCompletedFailures(t *testing.T) {
	checks := githubinfra.PullRequestCheckRuns{
		CheckRuns: []githubinfra.PullRequestCheckRun{
			{Name: "unit", Status: "completed", Conclusion: "failure"},
			{Name: "pending", Status: "in_progress", Conclusion: "failure"},
			{Name: "lint", Status: "completed", Conclusion: "success"},
			{Name: "duplicate", Status: "completed", Conclusion: "timed_out"},
			{Name: "duplicate", Status: "completed", Conclusion: "cancelled"},
		},
		Statuses: []githubinfra.PullRequestStatus{
			{Context: "legacy", State: "error"},
			{Context: "passing", State: "success"},
		},
	}

	if got, want := failedAuditorChecks(checks), []string{"duplicate", "legacy", "unit"}; !equalStringSlices(got, want) {
		t.Fatalf("failedAuditorChecks() = %#v, want %#v", got, want)
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
