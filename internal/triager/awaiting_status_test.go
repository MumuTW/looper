package triager

import (
	"context"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/planner"
	"github.com/MumuTW/looper/internal/projects"
	"github.com/MumuTW/looper/internal/storage"
)

// Status must project the existing triage lifecycle rather than recording a
// second "awaiting" state. A confirmation, route, or retirement resolves the
// ask even if the original report remains in the event log.
func TestAwaitingConfirmationStatusProjectsOnlyUnresolvedReports(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	now := fixture.now
	runner := fixture.runner()

	pending := statusAwaitingReport("pending", 41, now.Add(-3*time.Hour))
	confirmed := statusAwaitingReport("confirmed", 42, now.Add(-2*time.Hour))
	routed := statusAwaitingReport("routed", 43, now.Add(-time.Hour))
	retired := statusAwaitingReport("retired", 44, now.Add(-30*time.Minute))
	archived := Report{
		Version: 2, IdempotencyKey: "archived", ProjectID: "project_archived", Repo: "acme/archived", IssueNumber: 46,
		Policy: PolicyDecision{Action: ActionAwaitHuman}, CreatedAt: now.Add(-45 * time.Minute).Format(time.RFC3339Nano),
	}
	nonAwaiting := Report{
		Version: 2, IdempotencyKey: "routable", ProjectID: "project_1", Repo: "acme/looper", IssueNumber: 45,
		Policy: PolicyDecision{Action: ActionRoutePlanner}, CreatedAt: now.Add(-4 * time.Hour).Format(time.RFC3339Nano),
	}
	if err := fixture.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: archived.ProjectID, Name: "Archived", RepoPath: t.TempDir(), Archived: true, CreatedAt: archived.CreatedAt, UpdatedAt: archived.CreatedAt}); err != nil {
		t.Fatalf("upsert archived project: %v", err)
	}
	for _, report := range []Report{pending, confirmed, routed, retired, archived, nonAwaiting} {
		if err := runner.persistReport(ctx, report); err != nil {
			t.Fatalf("persist report %q: %v", report.IdempotencyKey, err)
		}
	}
	if err := runner.persistConfirmation(ctx, confirmed, Confirmation{ReportKey: confirmed.IdempotencyKey, CommentID: 9, Author: "maintainer", ConfirmedAt: now.Add(-time.Hour).Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("persist confirmation: %v", err)
	}
	if err := runner.persistProjection(ctx, routed, planner.DiscoveryResult{}); err != nil {
		t.Fatalf("persist projection: %v", err)
	}
	if err := runner.persistRetirement(ctx, Enrollment{IdempotencyKey: retired.IdempotencyKey, ProjectID: retired.ProjectID, Repo: retired.Repo, IssueNumber: retired.IssueNumber}, Retirement{EnrollmentKey: retired.IdempotencyKey, Reason: "confirmation_timeout", RetiredAt: now.Format(time.RFC3339Nano)}); err != nil {
		t.Fatalf("persist retirement: %v", err)
	}

	before, err := fixture.repos.Events.List(ctx, 100)
	if err != nil {
		t.Fatalf("list events before status projection: %v", err)
	}
	summary, err := AwaitingConfirmationStatus(ctx, fixture.repos, now)
	if err != nil {
		t.Fatalf("AwaitingConfirmationStatus() error = %v", err)
	}
	after, err := fixture.repos.Events.List(ctx, 100)
	if err != nil {
		t.Fatalf("list events after status projection: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("status projection wrote events: before=%d after=%d", len(before), len(after))
	}
	if summary.Count != 1 || len(summary.Sources) != 1 {
		t.Fatalf("summary = %#v, want one unresolved awaiting source", summary)
	}
	source := summary.Sources[0]
	if source.ProjectID != "project_1" || source.Repo != "acme/looper" || source.IssueNumber != 41 {
		t.Fatalf("source = %#v, want project_1 acme/looper#41", source)
	}
	if source.CreatedAt != pending.CreatedAt || source.AgeSeconds != int64((3*time.Hour).Seconds()) {
		t.Fatalf("source = %#v, want createdAt=%q ageSeconds=%d", source, pending.CreatedAt, int64((3 * time.Hour).Seconds()))
	}
}

// RemoveProject archives its record but retains lifecycle events. The status
// projection must therefore omit an unresolved report as soon as the project
// is removed, without adding a separate triage retirement event.
func TestAwaitingConfirmationStatusExcludesRemovedProject(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	report := statusAwaitingReport("removed", 47, fixture.now.Add(-time.Hour))
	if err := fixture.runner().persistReport(ctx, report); err != nil {
		t.Fatalf("persist report: %v", err)
	}

	service := &projects.Service{
		DB:    fixture.coordinator.DB(),
		Repos: fixture.repos,
		Now:   func() time.Time { return fixture.now },
	}
	if _, err := service.RemoveProject(ctx, report.ProjectID); err != nil {
		t.Fatalf("RemoveProject() error = %v", err)
	}

	summary, err := AwaitingConfirmationStatus(ctx, fixture.repos, fixture.now)
	if err != nil {
		t.Fatalf("AwaitingConfirmationStatus() error = %v", err)
	}
	if summary.Count != 0 || len(summary.Sources) != 0 {
		t.Fatalf("summary = %#v, want no sources after project removal", summary)
	}
}

func statusAwaitingReport(key string, issueNumber int64, createdAt time.Time) Report {
	return Report{
		Version: 2, IdempotencyKey: key, ProjectID: "project_1", Repo: "acme/looper", IssueNumber: issueNumber,
		Policy: PolicyDecision{Action: ActionAwaitHuman}, ConfirmationToken: "triage-confirm-" + key,
		CreatedAt: createdAt.Format(time.RFC3339Nano),
	}
}

// Contract: the projection carries the comment that resolves the ask. The
// confirmation token is minted per report and appears in no other operator
// surface, so a roster without it reports a wait nobody can end (#255).
func TestAwaitingConfirmationStatusCarriesTheConfirmationCommand(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	runner := fixture.runner()

	tokenless := statusAwaitingReport("tokenless", 51, fixture.now.Add(-time.Hour))
	tokenless.ConfirmationToken = ""
	for _, report := range []Report{statusAwaitingReport("tokened", 50, fixture.now.Add(-2*time.Hour)), tokenless} {
		if err := runner.persistReport(ctx, report); err != nil {
			t.Fatalf("persist report %q: %v", report.IdempotencyKey, err)
		}
	}

	summary, err := AwaitingConfirmationStatus(ctx, fixture.repos, fixture.now)
	if err != nil {
		t.Fatalf("AwaitingConfirmationStatus() error = %v", err)
	}
	if summary.Count != 2 {
		t.Fatalf("summary = %#v, want both awaiting sources", summary)
	}
	if got, want := summary.Sources[0].Command, "/plan triage-confirm-tokened"; got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
	// A report persisted without a token cannot be confirmed by anyone, so the
	// roster reports no command rather than one that would never be accepted.
	if got := summary.Sources[1].Command; got != "" {
		t.Fatalf("command = %q, want empty for a tokenless report", got)
	}
}
