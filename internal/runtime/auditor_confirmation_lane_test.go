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

type confirmationAuditorGateway struct {
	annotations map[int64][]githubinfra.CheckRunAnnotation
	head        string
	checks      githubinfra.PullRequestCheckRuns
	rerequests  []int64
}

func (g *confirmationAuditorGateway) GetBranchHeadSHA(context.Context, githubinfra.BranchHeadInput) (string, error) {
	return g.head, nil
}

func (g *confirmationAuditorGateway) ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error) {
	return g.checks, nil
}

func (g *confirmationAuditorGateway) RerequestCheckSuite(_ context.Context, input githubinfra.RerequestCheckSuiteInput) error {
	g.rerequests = append(g.rerequests, input.CheckSuiteID)
	return nil
}

func (g *confirmationAuditorGateway) ListCheckRunAnnotations(_ context.Context, input githubinfra.CheckRunAnnotationsInput) ([]githubinfra.CheckRunAnnotation, error) {
	return g.annotations[input.CheckRunID], nil
}

func (g *confirmationAuditorGateway) ListPullRequestFiles(context.Context, githubinfra.ViewPullRequestInput) ([]string, error) {
	return []string{"internal/runtime/auditor.go"}, nil
}

func TestProgressAuditorConfirmationRerequestsOnceThenRecordsMatchingFailure(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorConfirmationFixture(t, []int64{7654})
	gateway := &confirmationAuditorGateway{head: head, annotations: map[int64][]githubinfra.CheckRunAnnotation{99: {{Path: "internal/runtime/auditor.go", Level: "failure"}}}}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}

	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatalf("first progressAuditorConfirmation() error = %v", err)
	}
	if len(gateway.rerequests) != 1 || gateway.rerequests[0] != 7654 {
		t.Fatalf("rerequests = %#v, want one observed suite", gateway.rerequests)
	}

	entityType, entityID := "branch_head", repo+"@"+head
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil || countAuditorEvents(events, auditor.RerunRequestedEventType) != 1 {
		t.Fatalf("rerequest events = %#v, %v", events, err)
	}

	gateway.checks = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{ID: 99, Name: "ci", Status: "completed", Conclusion: "failure", CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(now)}}}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatalf("second progressAuditorConfirmation() error = %v", err)
	}
	if len(gateway.rerequests) != 1 {
		t.Fatalf("rerequests = %#v, want no duplicate request", gateway.rerequests)
	}

	events, _ = repos.Events.ListByEntity(ctx, entityType, entityID)
	confirmation := onlyAuditorConfirmation(t, events)
	if confirmation.Version != 2 || confirmation.Outcome != auditor.ConfirmationConfirmed || confirmation.Decision != auditor.ActionEscalate || confirmation.Candidate != nil || len(confirmation.ConfirmedChecks) != 1 || confirmation.ConfirmedChecks[0] != "ci" {
		t.Fatalf("confirmation = %#v, want confirmed escalation without attribution evidence", confirmation)
	}

	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatalf("replayed progressAuditorConfirmation() error = %v", err)
	}
	events, _ = repos.Events.ListByEntity(ctx, entityType, entityID)
	if len(gateway.rerequests) != 1 || countAuditorEvents(events, auditor.ConfirmationEventType) != 1 {
		t.Fatalf("replayed events = %#v, rerequests = %#v", events, gateway.rerequests)
	}
}

func TestProgressAuditorConfirmationRecordsFlakeWithoutRerequestingAgain(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorConfirmationFixture(t, []int64{7654})
	gateway := &confirmationAuditorGateway{head: head, annotations: map[int64][]githubinfra.CheckRunAnnotation{99: {{Path: "internal/runtime/auditor.go", Level: "failure"}}}}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	gateway.checks = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{ID: 99, Name: "ci", Status: "completed", Conclusion: "success", CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(now)}}}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	entityType, entityID := "branch_head", repo+"@"+head
	confirmation := onlyAuditorConfirmation(t, mustListAuditorEvents(t, ctx, repos, entityType, entityID))
	if confirmation.Outcome != auditor.ConfirmationSuspectedFlake || confirmation.Decision != auditor.ActionRecordFlake {
		t.Fatalf("confirmation = %#v, want durable flake record", confirmation)
	}
}

func TestProgressAuditorConfirmationContinuesAfterHeadAdvances(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorConfirmationFixture(t, []int64{7654})
	gateway := &confirmationAuditorGateway{head: head}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	gateway.head = "new-head"
	gateway.checks = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{ID: 99, Name: "ci", Status: "completed", Conclusion: "success", CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(now)}}}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	events, err := repos.Events.ListByEntity(ctx, "branch_head", repo+"@"+head)
	if err != nil {
		t.Fatal(err)
	}
	confirmation := onlyAuditorConfirmation(t, events)
	if confirmation.Outcome != auditor.ConfirmationSuspectedFlake {
		t.Fatalf("old-head confirmation = %#v, want rerun completion after branch advance", confirmation)
	}
}

func TestProgressAuditorConfirmationMarksUniquePathOverlapAsRevertProposal(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorConfirmationFixture(t, []int64{7654})
	projectID := project.ID
	sourceIssue := &githubinfra.IssueReference{Number: 118, Repo: repo}
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{EventType: gatekeeper.MergeOutcomeEventType, ProjectID: &projectID, Payload: gatekeeper.MergeOutcome{Version: 1, ProjectID: projectID, Repo: repo, PRNumber: 42, HeadSHA: "merged-pr-head", MergeCommitSHA: "merge-commit-1", MergeStrategy: "squash", SourceIssue: sourceIssue, Merged: true, TouchedFiles: []string{"internal/runtime/auditor.go"}, TouchedFilesAvailable: true}, CreatedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	gateway := &confirmationAuditorGateway{head: head, annotations: map[int64][]githubinfra.CheckRunAnnotation{99: {{Path: "internal/runtime/auditor.go", Level: "failure"}}}}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	gateway.checks = githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{ID: 99, Name: "ci", Status: "completed", Conclusion: "failure", CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(now)}}}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	entityType, entityID := "branch_head", repo+"@"+head
	confirmation := onlyAuditorConfirmation(t, mustListAuditorEvents(t, ctx, repos, entityType, entityID))
	if confirmation.Outcome != auditor.ConfirmationConfirmed || confirmation.Decision != auditor.ActionProposeRevert || confirmation.Candidate == nil || confirmation.Candidate.MergeCommitSHA != "merge-commit-1" || confirmation.Candidate.SourceIssueNumber != 118 {
		t.Fatalf("confirmation = %#v, want high-confidence revert proposal decision", confirmation)
	}
}

func TestAuditorConfirmationEscalatesWhenCandidateLacksRevertProvenance(t *testing.T) {
	observation := auditor.FailureObservation{ObservedAt: "2026-07-31T12:00:00.000Z", FailingPaths: []string{"a.go"}, FailingPathsByCheck: map[string][]string{"ci": {"a.go"}}, BaselineKnown: true, FailingPathEvidenceComplete: true, CandidatePRs: []int64{42}}
	confirmation := auditor.ConfirmationResult{Outcome: auditor.ConfirmationConfirmed, ConfirmedChecks: []string{"ci"}}
	events := []storage.EventLogRecord{{EventType: gatekeeper.MergeOutcomeEventType, PayloadJSON: `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"headSha":"head","mergeStrategy":"squash","merged":true,"touchedFiles":["a.go"],"touchedFilesAvailable":true}`, CreatedAt: "2026-07-31T11:59:00.000Z"}}
	coordinator := openMigratedCoordinator(t, filepath.Join(t.TempDir(), "auditor.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	for _, event := range events {
		if err := repos.Events.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	decision, err := auditorConfirmationDecisionWithGateway(context.Background(), repos.Events, nil, "", "project_1", "acme/looper", observation, confirmation, nil, 60)
	if err != nil || decision.Action != auditor.ActionEscalate || decision.Reason != "missing_merge_commit_provenance" {
		t.Fatalf("auditorConfirmationDecision() = %#v, %v", decision, err)
	}
}

func TestAuditorConfirmationRejectsRebaseCandidate(t *testing.T) {
	observation := auditor.FailureObservation{
		ObservedAt:                  "2026-07-31T12:00:00.000Z",
		FailingPaths:                []string{"a.go"},
		FailingPathsByCheck:         map[string][]string{"ci": {"a.go"}},
		BaselineKnown:               true,
		FailingPathEvidenceComplete: true,
		CandidatePRs:                []int64{42},
	}
	confirmation := auditor.ConfirmationResult{Outcome: auditor.ConfirmationConfirmed, ConfirmedChecks: []string{"ci"}}
	events := []storage.EventLogRecord{{
		EventType:   gatekeeper.MergeOutcomeEventType,
		PayloadJSON: `{"projectId":"project_1","repo":"acme/looper","prNumber":42,"headSha":"head","mergeCommitSha":"tip","mergeStrategy":"rebase","merged":true,"touchedFiles":["a.go"],"touchedFilesAvailable":true,"sourceIssue":{"number":118,"repo":"acme/looper"}}`,
		CreatedAt:   "2026-07-31T11:59:00.000Z",
	}}
	coordinator := openMigratedCoordinator(t, filepath.Join(t.TempDir(), "auditor.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	for _, event := range events {
		if err := repos.Events.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	decision, err := auditorConfirmationDecisionWithGateway(context.Background(), repos.Events, nil, "", "project_1", "acme/looper", observation, confirmation, nil, 60)
	if err != nil || decision.Action != auditor.ActionEscalate || decision.Reason != "rebase_merge_not_revertable" {
		t.Fatalf("auditorConfirmationDecision() = %#v, %v; want explicit rebase refusal", decision, err)
	}
}

func TestProgressAuditorConfirmationEscalatesObservationWithoutSuiteIdentity(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorConfirmationFixture(t, nil)
	gateway := &confirmationAuditorGateway{head: head}
	role := config.AuditorRoleConfig{Enabled: true, WindowMinutes: 60}
	if err := progressAuditorConfirmation(ctx, repos, gateway, project, repo, "main", role, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	entityType, entityID := "branch_head", repo+"@"+head
	confirmation := onlyAuditorConfirmation(t, mustListAuditorEvents(t, ctx, repos, entityType, entityID))
	if len(gateway.rerequests) != 0 || confirmation.Outcome != auditor.ConfirmationInconclusive || confirmation.Decision != auditor.ActionEscalate || confirmation.Reason != "missing_failed_check_suite_identity" {
		t.Fatalf("confirmation = %#v, rerequests = %#v", confirmation, gateway.rerequests)
	}
}

func TestCompletedAuditorRerunWaitsForAResultNewerThanRequest(t *testing.T) {
	requestedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	checks := githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure", CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(requestedAt.Add(-time.Second)), CompletedAt: eventlog.FormatJavaScriptISOString(requestedAt.Add(time.Minute))}}}
	if completed, failed := completedAuditorRerun(checks, []int64{7654}, map[int64]time.Time{7654: requestedAt}); completed || failed != nil {
		t.Fatalf("completedAuditorRerun() = (%v, %#v), want pending", completed, failed)
	}
}

func TestCompletedAuditorRerunWaitsForPendingStatus(t *testing.T) {
	requestedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	checks := githubinfra.PullRequestCheckRuns{
		CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "success", CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(requestedAt.Add(time.Second))}},
		Statuses:  []githubinfra.PullRequestStatus{{Context: "legacy", State: "pending"}},
	}
	if completed, _ := completedAuditorRerun(checks, []int64{7654}, map[int64]time.Time{7654: requestedAt}); completed {
		t.Fatal("completedAuditorRerun() = true, want pending while a legacy status is unresolved")
	}
}

func TestCompletedAuditorRerunReportsFailureStatusAsFailure(t *testing.T) {
	requestedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	checks := githubinfra.PullRequestCheckRuns{
		CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "success", CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(requestedAt.Add(time.Second))}},
		Statuses:  []githubinfra.PullRequestStatus{{Context: "legacy", State: "failure"}},
	}
	completed, failed := completedAuditorRerun(checks, []int64{7654}, map[int64]time.Time{7654: requestedAt})
	if !completed || !equalStringSlices(failed, []string{"legacy"}) {
		t.Fatalf("completedAuditorRerun() = (%v, %#v), want terminal failure evidence", completed, failed)
	}
}

func TestIntersectAuditorPathsByCheckUsesOnlyRepeatedPaths(t *testing.T) {
	got := intersectAuditorPathsByCheck(
		map[string][]string{"ci": {"a.go", "b.go"}, "lint": {"lint.go"}},
		map[string][]string{"CI": {"b.go"}, "lint": {"other.go"}},
		[]string{"ci"},
	)
	if len(got) != 1 || len(got["ci"]) != 1 || got["ci"][0] != "b.go" {
		t.Fatalf("intersectAuditorPathsByCheck() = %#v, want only repeated ci path", got)
	}
}

func TestIntersectAuditorPathsByCheckDoesNotFallBackWhenNoPathRepeats(t *testing.T) {
	got := intersectAuditorPathsByCheck(
		map[string][]string{"ci": {"a.go", "b.go"}},
		map[string][]string{"ci": {"c.go"}},
		[]string{"ci"},
	)
	if got == nil {
		t.Fatal("intersectAuditorPathsByCheck() = nil, want empty intersection marker")
	}
	if len(got) != 0 {
		t.Fatalf("intersectAuditorPathsByCheck() = %#v, want no paths", got)
	}
}

func TestAuditorObservedRerunIgnoresRunStartedBeforeObservation(t *testing.T) {
	observedAt := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	checks := githubinfra.PullRequestCheckRuns{CheckRuns: []githubinfra.PullRequestCheckRun{{CheckSuiteID: 7654, StartedAt: eventlog.FormatJavaScriptISOString(observedAt.Add(-time.Second)), CompletedAt: eventlog.FormatJavaScriptISOString(observedAt.Add(time.Second))}}}
	if when, ok := auditorObservedRerun(checks, 7654, observedAt); ok || !when.IsZero() {
		t.Fatalf("auditorObservedRerun() = (%v, %v), want no rerun for pre-observation run", when, ok)
	}
}

func auditorConfirmationFixture(t *testing.T, suiteIDs []int64) (context.Context, *storage.Repositories, storage.ProjectRecord, string, string, time.Time) {
	t.Helper()
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
		t.Fatal(err)
	}
	entityType, entityID, eventID := "branch_head", repo+"@"+head, "observation_1"
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{EventType: auditor.BaselineEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: auditor.BaselineObservation{Version: 1, ProjectID: projectID, Repo: repo, HeadSHA: head, ObservedAt: eventlog.FormatJavaScriptISOString(now.Add(-2 * time.Minute))}, CreatedAt: now.Add(-2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{ID: eventID, EventType: auditor.ObservedFailureEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: auditor.FailureObservation{Version: 5, ProjectID: projectID, Repo: repo, HeadSHA: head, FailedChecks: []string{"ci"}, FailingPaths: []string{"internal/runtime/auditor.go"}, FailingPathsByCheck: map[string][]string{"ci": {"internal/runtime/auditor.go"}}, FailingPathEvidenceComplete: true, BaselineKnown: true, CandidatePRs: []int64{42}, CheckSuiteIDs: suiteIDs, ObservedAt: nowISO}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return ctx, repos, project, repo, head, now
}

func mustListAuditorEvents(t *testing.T, ctx context.Context, repos *storage.Repositories, entityType, entityID string) []storage.EventLogRecord {
	t.Helper()
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func onlyAuditorConfirmation(t *testing.T, events []storage.EventLogRecord) auditor.ConfirmationRecord {
	t.Helper()
	var found []auditor.ConfirmationRecord
	for _, event := range events {
		if event.EventType != auditor.ConfirmationEventType {
			continue
		}
		var confirmation auditor.ConfirmationRecord
		if err := json.Unmarshal([]byte(event.PayloadJSON), &confirmation); err != nil {
			t.Fatal(err)
		}
		found = append(found, confirmation)
	}
	if len(found) != 1 {
		t.Fatalf("confirmation records = %#v, want one", found)
	}
	return found[0]
}

func countAuditorEvents(events []storage.EventLogRecord, eventType string) int {
	count := 0
	for _, event := range events {
		if event.EventType == eventType {
			count++
		}
	}
	return count
}
