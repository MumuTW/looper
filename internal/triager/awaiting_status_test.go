package triager

import (
	"context"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/planner"
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
	nonAwaiting := Report{
		Version: 2, IdempotencyKey: "routable", ProjectID: "project_1", Repo: "acme/looper", IssueNumber: 45,
		Policy: PolicyDecision{Action: ActionRoutePlanner}, CreatedAt: now.Add(-4 * time.Hour).Format(time.RFC3339Nano),
	}
	for _, report := range []Report{pending, confirmed, routed, retired, nonAwaiting} {
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

func statusAwaitingReport(key string, issueNumber int64, createdAt time.Time) Report {
	return Report{
		Version: 2, IdempotencyKey: key, ProjectID: "project_1", Repo: "acme/looper", IssueNumber: issueNumber,
		Policy: PolicyDecision{Action: ActionAwaitHuman}, CreatedAt: createdAt.Format(time.RFC3339Nano),
	}
}
