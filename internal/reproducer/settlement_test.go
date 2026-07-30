package reproducer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/triager"
)

// seedSupersedingReport mints a second accepted report for the same Issue, as
// Triager does when a new comment or edit supersedes the triage source.
func (f *fixture) seedSupersedingReport(key string) triager.Report {
	f.t.Helper()
	report := triager.Report{
		Version: 1, IdempotencyKey: key, ProjectID: testProjectID, Repo: testRepo,
		IssueNumber: testIssue, Decision: triager.Decision{Classification: triager.ClassificationBug},
		Policy: triager.PolicyDecision{Action: triager.ActionRoutePlanner},
		// Later than the seeded report, so it sorts last and is the current one.
		CreatedAt: f.now.Add(time.Hour).Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(report)
	if err != nil {
		f.t.Fatalf("marshal report: %v", err)
	}
	projectID, entityType := testProjectID, "github_issue"
	entityID := reproduction.EntityID(testProjectID, testRepo, testIssue)
	if err := f.repos.Events.Append(context.Background(), storage.EventLogRecord{
		ID: "event_triage_" + key, EventType: triager.ReportEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, PayloadJSON: string(payload),
		CreatedAt: report.CreatedAt,
	}); err != nil {
		f.t.Fatalf("Events.Append() error = %v", err)
	}
	return report
}

// Settlement was keyed by Issue number, so a cannot-reproduce recorded against
// one triage report skipped the attempt for every later report on the same
// Issue. The superseding report is exactly the case where new information
// arrived, which is the one time re-attempting matters most.
func TestASupersedingReportIsNotSettledByTheOldDecision(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{
		Attempted: []string{"ran the reported command"}, ObservedInstead: "no failure",
	})

	first := fixture.discover()
	if first.Unreproducible != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the first report parked", first)
	}

	// A comment supersedes the triage source; Triager mints a new report, and the
	// agent can now reproduce. The sentinel is already gone: parking cleaned the
	// worktree before exposing the waiver.
	fixture.seedSupersedingReport("triage:superseded")
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{
		"pkg/crash_test.go": "func TestCrash(t *testing.T) { panic(1) }",
	})

	second := fixture.discover()
	if second.Reproduced != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the superseding report attempted afresh", second)
	}
}

// The mirror image: a record or waiver minted against a superseded report must
// not authorize Planner for the new one, or the gate would open on evidence
// that predates the information the new report was created from.
func TestAnOldRecordDoesNotAuthorizePlannerForANewReport(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeAgentDraft("go test ./pkg -run TestCrash", map[string]string{
		"pkg/crash_test.go": "func TestCrash(t *testing.T) { panic(1) }",
	})
	if result := fixture.discover(); result.Reproduced != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the first report reproduced", result)
	}

	superseding := fixture.seedSupersedingReport("triage:superseded")
	status := fixture.status()
	if status.Record == nil {
		t.Fatalf("status = %#v, want the first report's record persisted", status)
	}
	supersededKey := reproduction.IdempotencyKey(testProjectID, testRepo, testIssue, superseding.IdempotencyKey)
	if status.PlannerAllowed(supersededKey) {
		t.Fatalf("status.PlannerAllowed(%q) = true, want the old record to authorize only its own report", supersededKey)
	}
	if !status.PlannerAllowed(testCandidateKey()) {
		t.Fatalf("status.PlannerAllowed(original) = false, want the record to still authorize the report it was made for")
	}
}

// The waiver is a decision about the evidence a human was shown. Carrying it
// forward to a report minted afterwards would authorize planning for something
// nobody looked at.
func TestAWaiverAuthorizesOnlyTheReportItAnswered(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{
		Attempted: []string{"ran the reported command"}, ObservedInstead: "no failure",
	})
	fixture.answerParkedLoop(AnswerProceed)

	fixture.discover()
	if second := fixture.discover(); second.Waived != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the waiver recorded", second)
	}

	superseding := fixture.seedSupersedingReport("triage:superseded")
	supersededKey := reproduction.IdempotencyKey(testProjectID, testRepo, testIssue, superseding.IdempotencyKey)
	status := fixture.status()
	if status.Waived == nil {
		t.Fatalf("status = %#v, want a waiver recorded", status)
	}
	if status.PlannerAllowed(supersededKey) || status.Settled(supersededKey) {
		t.Fatalf("status = %#v, want the waiver scoped to the report it answered", status)
	}
}

// A cannot-reproduce leaves the sentinel untracked and the agent's exploratory
// edits dirty. The human's other option is "plan without a reproduction", and
// Planner restores this same worktree and commits whatever it finds — so the
// branch has to be clean before the waiver is reachable.
func TestCannotReproduceCleansTheWorktreeBeforeExposingTheWaiver(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{
		Attempted: []string{"ran the reported command"}, ObservedInstead: "no failure",
	})
	// An exploratory edit the agent left behind while investigating.
	experiment := filepath.Join(fixture.worktree, "src", "experiment.go")
	if err := os.MkdirAll(filepath.Dir(experiment), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(experiment, []byte("// half-finished"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if result := fixture.discover(); result.Unreproducible != 1 {
		t.Fatalf("DiscoverIssues() = %#v, want the Issue parked", result)
	}
	if len(fixture.git.discarded) != 1 {
		t.Fatalf("discarded = %#v, want the worktree returned to its committed state exactly once", fixture.git.discarded)
	}
	if _, err := os.Stat(filepath.Join(fixture.worktree, filepath.FromSlash(CannotReproduceRelPath))); !os.IsNotExist(err) {
		t.Fatalf("sentinel Stat() error = %v, want the sentinel removed rather than left for Planner to commit", err)
	}
	// The cleanup must precede the park: once the loop is awaiting a human, the
	// waiver is reachable and Planner can restore the branch.
	if len(fixture.planner.parked) != 1 {
		t.Fatalf("parked = %#v, want the Issue parked after the cleanup", fixture.planner.parked)
	}
}

// A decodable but empty or wrong-version record parks the Issue permanently with
// none of the attempted/observed detail the escalation exists to provide, and a
// future schema would be silently half-honoured.
func TestAnUnusableCannotReproduceRecordStillEscalatesWithAnExplanation(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]any{
		"empty shape":   {"version": reproduction.ManifestVersion, "attempted": []string{}, "observedInstead": ""},
		"wrong version": {"version": 2, "attempted": []string{"tried"}, "observedInstead": "nothing"},
		"blank strings": {"version": reproduction.ManifestVersion, "attempted": []string{"   "}, "observedInstead": "  "},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newFixture(t)
			fixture.seedTriageReport(triager.ClassificationBug)
			fixture.agent.onStart = func() { fixture.writeWorktreeJSON(CannotReproduceRelPath, payload) }

			if result := fixture.discover(); result.Unreproducible != 1 {
				t.Fatalf("DiscoverIssues() = %#v, want the decline still escalated", result)
			}
			record := fixture.status().Unreproducible
			if record == nil {
				t.Fatalf("status.Unreproducible = nil, want the decline recorded")
			}
			if !strings.Contains(record.Summary, "cannot-reproduce record") {
				t.Fatalf("summary = %q, want the human told the record was unusable", record.Summary)
			}
			if len(record.Attempted) == 0 || strings.TrimSpace(record.ObservedInstead) == "" {
				t.Fatalf("record = %#v, want the escalation to carry actionable fields", record)
			}
		})
	}
}

// The rejection option is documented as leaving the Issue stopped. It must be
// declared non-resuming on the ask itself, because the shared respond path
// transitions every awaiting_human loop to running and queues Planner work
// before Reproducer ever sees the answer.
func TestTheRejectionOptionIsDeclaredNonResuming(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.seedTriageReport(triager.ClassificationBug)
	fixture.writeCannotReproduce(CannotReproduce{
		Attempted: []string{"ran the reported command"}, ObservedInstead: "no failure",
	})
	fixture.discover()

	ask := fixture.planner.parked[0].Ask
	if ask.AnswerResumes(AnswerReject) {
		t.Fatalf("ask = %#v, want rejection routed through a non-resuming path", ask)
	}
	if !ask.AnswerResumes(AnswerProceed) {
		t.Fatalf("ask = %#v, want proceeding to resume the loop as before", ask)
	}
}
