package runtime

import (
	"context"
	"encoding/json"
	"errors"
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
	head             string
	checks           githubinfra.PullRequestCheckRuns
	annotations      map[int64][]githubinfra.CheckRunAnnotation
	annotationErrors map[int64]error
}

func (f fakeAuditorGateway) GetBranchHeadSHA(context.Context, githubinfra.BranchHeadInput) (string, error) {
	return f.head, nil
}

func (f fakeAuditorGateway) ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error) {
	return f.checks, nil
}

func (f fakeAuditorGateway) RerequestCheckSuite(context.Context, githubinfra.RerequestCheckSuiteInput) error {
	return nil
}

func (f fakeAuditorGateway) ListCheckRunAnnotations(_ context.Context, input githubinfra.CheckRunAnnotationsInput) ([]githubinfra.CheckRunAnnotation, error) {
	if err := f.annotationErrors[input.CheckRunID]; err != nil {
		return nil, err
	}
	return f.annotations[input.CheckRunID], nil
}

func (fakeAuditorGateway) ListPullRequestFiles(context.Context, githubinfra.ViewPullRequestInput) ([]string, error) {
	return nil, errors.New("files unavailable in fake")
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
	entityType, entityID := "branch_head", repo+"@"+head
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{EventType: auditor.BaselineEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: auditor.BaselineObservation{Version: 1, ProjectID: projectID, Repo: repo, HeadSHA: head, ObservedAt: eventlog.FormatJavaScriptISOString(now.Add(-time.Minute))}, CreatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("Append baseline error = %v", err)
	}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	gateway := fakeAuditorGateway{head: head, checks: githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{ID: 99, Name: "ci", Status: "completed", Conclusion: "failure", CheckSuiteID: 7654}}}, annotations: map[int64][]githubinfra.CheckRunAnnotation{99: {{Path: "internal/runtime/auditor.go", Level: "failure"}}}}
	if err := observePostMergeFailure(ctx, repos, gateway, project, repo, base, role, func() time.Time { return now }); err != nil {
		t.Fatalf("observePostMergeFailure() error = %v", err)
	}
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil || countAuditorEvents(events, auditor.ObservedFailureEventType) != 1 {
		t.Fatalf("ListByEntity() = %#v, %v", events, err)
	}
	var observation auditor.FailureObservation
	for _, event := range events {
		if event.EventType == auditor.ObservedFailureEventType {
			if err := json.Unmarshal([]byte(event.PayloadJSON), &observation); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if observation.Version != 5 || !observation.BaselineKnown || !observation.FailingPathEvidenceComplete || observation.ProjectID != projectID || observation.HeadSHA != head || len(observation.CandidatePRs) != 1 || observation.CandidatePRs[0] != 42 || len(observation.FailedChecks) != 1 || observation.FailedChecks[0] != "ci" || len(observation.FailingPaths) != 1 || observation.FailingPaths[0] != "internal/runtime/auditor.go" || len(observation.CheckSuiteIDs) != 1 || observation.CheckSuiteIDs[0] != 7654 {
		t.Fatalf("observation = %#v", observation)
	}
	if err := observePostMergeFailure(ctx, repos, gateway, project, repo, base, role, func() time.Time { return now }); err != nil {
		t.Fatalf("second observePostMergeFailure() error = %v", err)
	}
	events, _ = repos.Events.ListByEntity(ctx, entityType, entityID)
	if countAuditorEvents(events, auditor.ObservedFailureEventType) != 1 {
		t.Fatalf("observation events = %#v, want one idempotent record", events)
	}
}

func TestObservePostMergeFailureUsesCoordinatorMergeWatchEvidence(t *testing.T) {
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
	payload := eventlog.CoordinatorPullRequestMerged{Version: 1, ProjectID: projectID, Repo: repo, PRNumber: 42, HeadSHA: "pr-sha", MergedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano)}
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{EventType: eventlog.CoordinatorPullRequestMergedEventType, ProjectID: &projectID, Payload: payload, CreatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("Append coordinator merge event error = %v", err)
	}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	gateway := fakeAuditorGateway{head: head, checks: githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}}}}
	if err := observePostMergeFailure(ctx, repos, gateway, project, repo, base, role, func() time.Time { return now }); err != nil {
		t.Fatalf("observePostMergeFailure() error = %v", err)
	}
	entityType, entityID := "branch_head", repo+"@"+head
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil || len(events) != 1 {
		t.Fatalf("ListByEntity() = %#v, %v", events, err)
	}
	var observation auditor.FailureObservation
	if err := json.Unmarshal([]byte(events[0].PayloadJSON), &observation); err != nil {
		t.Fatal(err)
	}
	if len(observation.CandidatePRs) != 1 || observation.CandidatePRs[0] != 42 {
		t.Fatalf("observation candidate PRs = %#v, want Mergify merge-watch PR", observation.CandidatePRs)
	}
}

func TestObservePostMergeFailureFiltersOutOfWindowMergeByForgeTime(t *testing.T) {
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
	// The event was observed recently (event CreatedAt inside the window) but
	// the merge itself happened long before the window started. The audit
	// window is selected by event CreatedAt, so the event is listed; the
	// candidate filter must drop it by the authoritative forge MergedAt so a
	// late-observed out-of-window merge never corrupts attribution.
	payload := eventlog.CoordinatorPullRequestMerged{Version: 1, ProjectID: projectID, Repo: repo, PRNumber: 42, HeadSHA: "pr-sha", MergedAt: now.Add(-24 * time.Hour).Format(time.RFC3339Nano)}
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{EventType: eventlog.CoordinatorPullRequestMergedEventType, ProjectID: &projectID, Payload: payload, CreatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("Append coordinator merge event error = %v", err)
	}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	gateway := fakeAuditorGateway{head: head, checks: githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}}}}
	if err := observePostMergeFailure(ctx, repos, gateway, project, repo, base, role, func() time.Time { return now }); err != nil {
		t.Fatalf("observePostMergeFailure() error = %v", err)
	}
	entityType, entityID := "branch_head", repo+"@"+head
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil {
		t.Fatalf("ListByEntity() error = %v", err)
	}
	for _, event := range events {
		if event.EventType == auditor.ObservedFailureEventType {
			t.Fatalf("out-of-window merge became a failure observation: %#v", event)
		}
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
			{Name: "unit", Status: "completed", Conclusion: "failure", CheckSuiteID: 8},
			{Name: "pending", Status: "in_progress", Conclusion: "failure"},
			{Name: "lint", Status: "completed", Conclusion: "success"},
			{Name: "duplicate", Status: "completed", Conclusion: "timed_out", CheckSuiteID: 3},
			{Name: "duplicate", Status: "completed", Conclusion: "cancelled", CheckSuiteID: 3},
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

func TestFailedAuditorCheckEvidenceKeepsExactFailedSuiteIDs(t *testing.T) {
	evidence := failedAuditorCheckEvidence(githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{
		{Name: "ignored-pending", Status: "in_progress", Conclusion: "failure", CheckSuiteID: 1},
		{Name: "failed", Status: "completed", Conclusion: "failure", CheckSuiteID: 4},
		{Name: "also-failed", Status: "completed", Conclusion: "timed_out", CheckSuiteID: 2},
		{Name: "missing-suite", Status: "completed", Conclusion: "failure"},
	}})
	if !equalStringSlices(evidence.Names, []string{"also-failed", "failed", "missing-suite"}) {
		t.Fatalf("Names = %#v", evidence.Names)
	}
	if len(evidence.SuiteIDs) != 2 || evidence.SuiteIDs[0] != 2 || evidence.SuiteIDs[1] != 4 {
		t.Fatalf("SuiteIDs = %#v, want exact sorted failed-suite IDs", evidence.SuiteIDs)
	}
}

func TestFailedAuditorCheckPathsFiltersNonFailuresAndReportsReadGaps(t *testing.T) {
	gateway := fakeAuditorGateway{
		annotations: map[int64][]githubinfra.CheckRunAnnotation{
			99: {{Path: "failure.go", Level: "failure"}, {Path: "warning.go", Level: "warning"}},
		},
		annotationErrors: map[int64]error{100: errors.New("annotations unavailable")},
	}
	paths, _, complete := failedAuditorCheckEvidenceWithPaths(context.Background(), gateway, "acme/looper", t.TempDir(), githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{
		{ID: 99, Status: "completed", Conclusion: "failure"},
		{ID: 100, Status: "completed", Conclusion: "failure"},
	}})
	if !equalStringSlices(paths, []string{"failure.go"}) || complete {
		t.Fatalf("failedAuditorCheckEvidenceWithPaths() = (%#v, %v), want only failure-level path and incomplete evidence", paths, complete)
	}
}

func TestCleanAuditorCheckEvidenceRequiresCompleteTerminalSuccess(t *testing.T) {
	tests := []struct {
		name   string
		checks githubinfra.PullRequestCheckRuns
		want   bool
	}{
		{name: "empty", checks: githubinfra.PullRequestCheckRuns{}, want: false},
		{name: "pending", checks: githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Status: "in_progress"}}}, want: false},
		{name: "successful check", checks: githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Status: "completed", Conclusion: "success"}}}, want: true},
		{name: "truncated", checks: githubinfra.PullRequestCheckRuns{TotalCount: 2, CheckRuns: []githubinfra.PullRequestCheckRun{{Status: "completed", Conclusion: "success"}}}, want: false},
		{name: "pending status", checks: githubinfra.PullRequestCheckRuns{Statuses: []githubinfra.PullRequestStatus{{State: "pending"}}}, want: false},
		{name: "successful status", checks: githubinfra.PullRequestCheckRuns{Statuses: []githubinfra.PullRequestStatus{{State: "success"}}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanAuditorCheckEvidence(test.checks); got != test.want {
				t.Fatalf("cleanAuditorCheckEvidence() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRecordAuditorBaselineScopesDeduplicationToProject(t *testing.T) {
	ctx := context.Background()
	coordinator := openMigratedCoordinator(t, filepath.Join(t.TempDir(), "auditor.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	nowISO := eventlog.FormatJavaScriptISOString(now)
	projectA := storage.ProjectRecord{ID: "project_a", Name: "A", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
	projectB := storage.ProjectRecord{ID: "project_b", Name: "B", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, projectA); err != nil {
		t.Fatal(err)
	}
	if err := repos.Projects.Upsert(ctx, projectB); err != nil {
		t.Fatal(err)
	}
	if err := recordAuditorBaseline(ctx, repos, projectA, "acme/looper", "head", now); err != nil {
		t.Fatal(err)
	}
	if err := recordAuditorBaseline(ctx, repos, projectB, "acme/looper", "head", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	events, err := repos.Events.ListByEntity(ctx, "branch_head", "acme/looper@head")
	if err != nil || countAuditorEvents(events, auditor.BaselineEventType) != 2 {
		t.Fatalf("baseline events = %#v, %v; want one per project", events, err)
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
