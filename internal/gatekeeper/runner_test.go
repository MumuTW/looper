package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

func TestEvaluatePullRequestPersistsEligibleReportBoundToHead(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID:       "project_1",
		Repo:            "acme/looper",
		PRNumber:        42,
		ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || report.Status != StatusEligible || len(report.Reasons) != 0 {
		t.Fatalf("report = %#v, want eligible without reasons", report)
	}
	if report.ObservedHeadSHA != "head-1" || !report.RequiresFreshRevalidation || report.Mode != string(config.GatekeeperTrustObserve) {
		t.Fatalf("report binding = %#v, want observe-only head-bound report", report)
	}

	events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	gateReports := make([]storage.EventLogRecord, 0, len(events))
	for _, event := range events {
		if event.EventType == GateReportEventType {
			gateReports = append(gateReports, event)
		}
	}
	// Persist writes a crash-boundary pending projection (empty discovery
	// fingerprint) before the routing projection, then the final report with the
	// real fingerprint after the projection succeeds. The final report is the
	// newest record and must be the one latestGateReports resolves to.
	// Persist writes a crash-boundary pending projection (empty discovery
	// fingerprint) before the routing projection, then the final report after
	// the projection succeeds. A direct EvaluatePullRequest carries no discovery
	// fingerprint, so both records here have empty fingerprints; the contract
	// under test is the two-append ordering, not the fingerprint value.
	if len(gateReports) != 2 {
		t.Fatalf("gate report events = %d, want 2 (pending projection plus final report)", len(gateReports))
	}
	var persisted Report
	if err := json.Unmarshal([]byte(gateReports[len(gateReports)-1].PayloadJSON), &persisted); err != nil {
		t.Fatalf("decode persisted report: %v", err)
	}
	if !persisted.Eligible || persisted.ObservedHeadSHA != "head-1" {
		t.Fatalf("persisted report = %#v, want eligible at head-1", persisted)
	}
}

func TestEvaluatePullRequestPreservesUnrecognizedMergeabilityEvidence(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.mergeable.MergeableState = "future_state"

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !slices.Equal(reasonCodes(report.Reasons), []ReasonCode{ReasonProviderStateAmbiguous}) {
		t.Fatalf("report = %#v, want an ambiguous provider-state block", report)
	}
	if report.Evidence.MergeableState != "future_state" {
		t.Fatalf("MergeableState evidence = %q, want future_state", report.Evidence.MergeableState)
	}
}

func TestEvaluatePullRequestUsesProjectHoldNamespace(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	metadata := `{"labelNamespace":"team.looper:"}`
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = %#v, %v", project, err)
	}
	project.MetadataJSON = &metadata
	if err := fixture.repos.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	fixture.github.detail.Labels = []string{"team.looper:hold"}

	report, err := fixture.runner().EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonHold || report.Reasons[0].Subject != "team.looper:hold" {
		t.Fatalf("report = %#v, want custom namespace hold block", report)
	}
	if len(report.Evidence.HoldLabels) != 1 || report.Evidence.HoldLabels[0] != "team.looper:hold" {
		t.Fatalf("hold evidence = %#v, want custom namespace label", report.Evidence.HoldLabels)
	}
}

func TestEvaluatePullRequestUsesConfiguredProjectHoldNamespace(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.Labels = []string{"team.looper:hold"}
	runner := New(Options{
		Repos:  fixture.repos,
		GitHub: fixture.github,
		Now:    func() time.Time { return fixture.now },
		LabelNamespaceForProject: func(projectID string) (labels.Namespace, bool) {
			if projectID != "project_1" {
				return labels.Namespace{}, false
			}
			return labels.NewNamespace("team.looper:"), true
		},
	})

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonHold {
		t.Fatalf("report = %#v, want configured namespace hold block", report)
	}
}

func TestEvaluatePullRequestBlocksEachSafetyCondition(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gatekeeperFixture)
		input  EvaluationInput
		want   []ReasonCode
	}{
		{
			name:  "head changed since trigger",
			input: EvaluationInput{ExpectedHeadSHA: "old-head"},
			want:  []ReasonCode{ReasonHeadStale},
		},
		{
			name: "head changes during evaluation",
			mutate: func(f *gatekeeperFixture) {
				f.github.finalHeadSHA = "head-2"
			},
			want: []ReasonCode{ReasonHeadStale},
		},
		{
			name: "pending required check",
			mutate: func(f *gatekeeperFixture) {
				f.github.checks.CheckRuns[0] = githubinfra.PullRequestCheckRun{Name: "ci", Status: "in_progress", AppID: 15368}
			},
			want: []ReasonCode{ReasonCheckPending},
		},
		{
			name: "cancelled required check",
			mutate: func(f *gatekeeperFixture) {
				f.github.checks.CheckRuns[0] = githubinfra.PullRequestCheckRun{Name: "ci", Status: "completed", Conclusion: "cancelled", AppID: 15368}
			},
			want: []ReasonCode{ReasonCheckCancelled},
		},
		{
			name: "failed required check",
			mutate: func(f *gatekeeperFixture) {
				f.github.checks.CheckRuns[0] = githubinfra.PullRequestCheckRun{Name: "ci", Status: "completed", Conclusion: "failure", AppID: 15368}
			},
			want: []ReasonCode{ReasonCheckFailed},
		},
		{
			name: "missing required check",
			mutate: func(f *gatekeeperFixture) {
				f.github.checks.CheckRuns = nil
				f.github.checks.TotalCount = 0
			},
			want: []ReasonCode{ReasonCheckMissing},
		},
		{
			name: "same named check from wrong app does not satisfy protection",
			mutate: func(f *gatekeeperFixture) {
				f.github.checks.CheckRuns[0].AppID = 99999
			},
			want: []ReasonCode{ReasonCheckMissing},
		},
		{
			name: "truncated check state is ambiguous",
			mutate: func(f *gatekeeperFixture) {
				f.github.checks.TotalCount = 2
			},
			want: []ReasonCode{ReasonProviderStateAmbiguous},
		},
		{
			name: "unresolved review thread",
			mutate: func(f *gatekeeperFixture) {
				f.github.threads = []githubinfra.ReviewThread{{ID: "thread-1", IsResolved: false}}
			},
			want: []ReasonCode{ReasonUnresolvedReviewThread},
		},
		{
			name: "required review missing",
			mutate: func(f *gatekeeperFixture) {
				f.github.detail.ReviewDecision = "REVIEW_REQUIRED"
			},
			want: []ReasonCode{ReasonReviewRequired},
		},
		{
			name: "merge conflict",
			mutate: func(f *gatekeeperFixture) {
				mergeable := false
				f.github.mergeable.Mergeable = &mergeable
				f.github.mergeable.MergeableState = "dirty"
			},
			want: []ReasonCode{ReasonMergeConflict},
		},
		{
			name: "mergeability is not clean",
			mutate: func(f *gatekeeperFixture) {
				f.github.mergeable.MergeableState = "behind"
			},
			want: []ReasonCode{ReasonMergeabilityNotClean},
		},
		{
			name: "provider policy blocker is ambiguous",
			mutate: func(f *gatekeeperFixture) {
				f.github.mergeable.MergeableState = "blocked"
			},
			want: []ReasonCode{ReasonProviderStateAmbiguous},
		},
		{
			name: "global hold",
			mutate: func(f *gatekeeperFixture) {
				f.github.detail.Labels = []string{labels.HoldGlobal}
			},
			want: []ReasonCode{ReasonHold},
		},
		{
			// A Gate report that omitted this would record the PR as eligible
			// while a human veto is in force.
			name: "global hold despite case and padding",
			mutate: func(f *gatekeeperFixture) {
				f.github.detail.Labels = []string{" " + strings.Title(labels.HoldGlobal) + " "}
			},
			want: []ReasonCode{ReasonHold},
		},
		{
			name: "provider mergeability is ambiguous",
			mutate: func(f *gatekeeperFixture) {
				f.github.mergeable.Mergeable = nil
				f.github.mergeable.MergeableState = "unknown"
			},
			want: []ReasonCode{ReasonProviderStateAmbiguous},
		},
		{
			name: "provider state is unavailable",
			mutate: func(f *gatekeeperFixture) {
				f.github.protectionErr = errors.New("provider unavailable")
			},
			want: []ReasonCode{ReasonProviderStateUnavailable},
		},
		{
			name: "draft pull request",
			mutate: func(f *gatekeeperFixture) {
				f.github.detail.IsDraft = true
			},
			want: []ReasonCode{ReasonPullRequestDraft},
		},
		{
			name: "closed pull request",
			mutate: func(f *gatekeeperFixture) {
				f.github.detail.State = "CLOSED"
			},
			want: []ReasonCode{ReasonPullRequestNotOpen},
		},
		{
			name: "project policy denies target",
			mutate: func(f *gatekeeperFixture) {
				f.policyPermits = false
			},
			want: []ReasonCode{ReasonProjectPolicyDenied},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGatekeeperFixture(t)
			if tc.mutate != nil {
				tc.mutate(fixture)
			}
			input := tc.input
			input.ProjectID = "project_1"
			input.Repo = "acme/looper"
			input.PRNumber = 42
			report, err := fixture.runner().EvaluatePullRequest(context.Background(), input)
			if err != nil {
				t.Fatalf("EvaluatePullRequest() error = %v", err)
			}
			if report.Eligible || report.Status != StatusBlocked {
				t.Fatalf("report = %#v, want blocked", report)
			}
			got := reasonCodes(report.Reasons)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("reason codes = %v, want %v", got, tc.want)
			}
		})
	}
}

type gatekeeperFixture struct {
	repos         *storage.Repositories
	github        *fakeGatekeeperGitHub
	now           time.Time
	policyPermits bool
	// closeDB closes the underlying SQLite coordinator so a test can force a
	// persistence failure mid-discovery.
	closeDB func() error
}

func newGatekeeperFixture(t *testing.T) *gatekeeperFixture {
	return newGatekeeperFixtureWithReview(t, true)
}

func newGatekeeperFixtureWithoutReview(t *testing.T) *gatekeeperFixture {
	return newGatekeeperFixtureWithReview(t, false)
}

func newGatekeeperFixtureWithReview(t *testing.T, seedReview bool) *gatekeeperFixture {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "gatekeeper.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	nowISO := now.Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	mergeable := true
	fixture := &gatekeeperFixture{
		repos: repos,
		now:   now,
		github: &fakeGatekeeperGitHub{
			detail:    githubinfra.PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", BaseRefName: "main", BaseSHA: "base-1", ReviewDecision: "APPROVED", AdditionsKnown: true, DeletionsKnown: true},
			mergeable: githubinfra.PullRequestDetail{Number: 42, HeadSHA: "head-1", BaseSHA: "base-1", Mergeable: &mergeable, MergeableState: "clean", AdditionsKnown: true, DeletionsKnown: true},
			protection: githubinfra.BranchProtection{
				Enabled: true, HasRequiredChecks: true, RequiredChecks: []string{"ci", RequiredStatusContext},
				RequiredCheckRules: []githubinfra.RequiredCheckRule{{Context: "ci", AppID: 15368}},
				HasRequiredReviews: true, RequiredApprovingReviewCount: 1,
			},
			checks:       githubinfra.PullRequestCheckRuns{TotalCount: 1, CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "success", AppID: 15368}}},
			finalHeadSHA: "head-1", finalBaseSHA: "base-1",
			reviewMarker: githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"},
		},
		policyPermits: true,
		closeDB:       coordinator.Close,
	}
	if seedReview {
		seedReviewerReviewEvent(t, fixture, "head-1", "APPROVE", "reviewer-loop", 0)
	}
	return fixture
}

func (f *gatekeeperFixture) runner() *Runner {
	return New(Options{
		Repos:  f.repos,
		GitHub: f.github,
		Now:    func() time.Time { return f.now },
		PolicyPermitsTarget: func(string, string, string) bool {
			return f.policyPermits
		},
	})
}

func (f *gatekeeperFixture) autoRunner() *Runner {
	return New(Options{
		Repos: f.repos, GitHub: f.github, Now: func() time.Time { return f.now },
		PolicyPermitsTarget: func(string, string, string) bool { return f.policyPermits },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
	})
}

func (f *gatekeeperFixture) requireReviewBySize() {
	f.github.detail.Additions = DefaultRequiredReviewChangedLines
}

func TestAutoGatekeeperRequiresCurrentHeadCodexReviewAndPublishesPendingStatus(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.requireReviewBySize()
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.checks.Statuses = []githubinfra.PullRequestStatus{{Context: RequiredStatusContext, State: "success"}}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !slices.Contains(reasonCodes(report.Reasons), ReasonCodexReviewRequired) {
		t.Fatalf("report = %#v, want current-head review required", report)
	}
	if got := fixture.github.labelAdds; len(got) != 0 {
		t.Fatalf("label adds = %#v, want no auto-merge route while review is required", got)
	}
	if got := fixture.github.reviewMarkerCalls; len(got) != 1 || got[0].Marker != "looper:review id_prefix=reviewer: head=head-1" || got[0].AuthorLogin != "looper-bot" {
		t.Fatalf("review marker calls = %#v, want exact current-head marker and Looper author", got)
	}
}

func TestAutoGatekeeperAcceptsCleanCurrentHeadCodexReview(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.requireReviewBySize()
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || report.Evidence.CodexReviewOutcome != "clean" {
		t.Fatalf("report = %#v, want eligible clean current-head review", report)
	}
	if got := fixture.github.statusCalls; len(got) != 1 || got[0].State != "success" {
		t.Fatalf("status calls = %#v, want success", got)
	}
}

func TestAutoGatekeeperDoesNotDuplicateMissingReviewReason(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.requireReviewBySize()
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	missingCount := 0
	for _, reason := range report.Reasons {
		if reason.Code == ReasonCodexReviewMissing {
			missingCount++
		}
	}
	if missingCount != 1 {
		t.Fatalf("reasons = %#v, want exactly one codex_review_missing entry", report.Reasons)
	}
}

func TestAutoGatekeeperAllowsSmallChangeWithoutCleanReview(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.github.detail.Additions = DefaultRequiredReviewChangedLines - 1
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || report.Evidence.ReviewRequiredByPolicy || report.Evidence.ChangedLines != DefaultRequiredReviewChangedLines-1 {
		t.Fatalf("report = %#v, want eligible below threshold", report)
	}
	if len(fixture.github.reviewMarkerCalls) != 1 {
		t.Fatalf("review marker calls = %#v, want one blocking-marker check", fixture.github.reviewMarkerCalls)
	}
	if got := fixture.github.statusCalls; len(got) != 1 || got[0].State != "success" {
		t.Fatalf("status calls = %#v, want success", got)
	}
}

func TestAutoGatekeeperFailsClosedWhenReviewStatsAreOmitted(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.Additions = 0
	fixture.github.detail.Deletions = 0
	fixture.github.detail.AdditionsKnown = false
	fixture.github.detail.DeletionsKnown = false
	fixture.github.mergeable.AdditionsKnown = false
	fixture.github.mergeable.DeletionsKnown = false

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || len(report.Reasons) != 1 || report.Reasons[0].Code != ReasonProviderStateUnavailable || report.Reasons[0].Subject != "review_capacity_stats" {
		t.Fatalf("report = %#v, want fail-closed review capacity stats block", report)
	}
}

func TestAutoGatekeeperBlocksBlockingReviewBelowThreshold(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.github.detail.Additions = DefaultRequiredReviewChangedLines - 1
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "blocking", Event: "REQUEST_CHANGES", AuthorLogin: "looper-bot"}
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !hasReason(report, ReasonCodexReviewBlocked) || report.Evidence.ReviewRequiredByPolicy {
		t.Fatalf("report = %#v, want blocking marker to veto below-threshold change", report)
	}
}

func TestAutoGatekeeperExplicitZeroDisablesReviewThreshold(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.github.detail.Additions = DefaultRequiredReviewChangedLines + 100
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget:                  func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:                      func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		RequiredReviewChangedLinesForProject: func(string) int { return 0 },
	})

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible || report.Evidence.ReviewRequiredByPolicy || hasReason(report, ReasonCodexReviewMissing) {
		t.Fatalf("report = %#v, want explicit zero to disable review requirement", report)
	}
}

func TestAutoGatekeeperUsesEffectiveReviewThreshold(t *testing.T) {
	fixture := newGatekeeperFixtureWithoutReview(t)
	fixture.github.detail.Additions = 120
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
	fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget:                  func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:                      func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		RequiredReviewChangedLinesForProject: func(string) int { return 100 },
	})

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !report.Evidence.ReviewRequiredByPolicy || report.Evidence.ChangedLines != 120 || !hasReason(report, ReasonCodexReviewRequired) {
		t.Fatalf("report = %#v, want effective threshold 100 to require a review", report)
	}
}

func TestAutoGatekeeperSkipsMergedBacklogWhenReviewThresholdDisabled(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = nil
	fixture.github.mergedPullRequests = []githubinfra.PullRequestSummary{{Number: 91, HeadSHA: "merged-head", Additions: 200}}
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget:                  func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:                      func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		RequiredReviewChangedLinesForProject: func(string) int { return 0 },
	})

	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if result.UnreviewedMerged != 0 || fixture.github.mergedListCalls != 0 {
		t.Fatalf("result = %#v, merged list calls = %d, want disabled backlog scan with no provider request", result, fixture.github.mergedListCalls)
	}
}

func TestAutoGatekeeperPublishesStatusesBeforeMergedReviewBacklog(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{{
		Number: 42, HeadSHA: "head-1", State: "OPEN", UpdatedAt: "2026-07-30T10:00:00Z", BaseRefName: "main", ReviewDecision: "APPROVED",
	}}
	fixture.github.mergedPullRequests = []githubinfra.PullRequestSummary{{
		Number: 91, HeadSHA: "merged-head", MergedAt: "2026-07-30T12:00:00Z", Additions: 220,
	}}
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	if _, err := fixture.autoRunner().DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(fixture.github.callSequence) < 2 || fixture.github.callSequence[0] != "commit-status" {
		t.Fatalf("call sequence = %#v, want current status before historical backlog scan", fixture.github.callSequence)
	}
}

func TestAutoGatekeeperRecordsUnreviewedMergedPullRequestOnce(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = nil
	fixture.github.mergedPullRequests = []githubinfra.PullRequestSummary{{
		Number: 91, HeadSHA: "merged-head", MergedAt: "2026-07-30T12:00:00Z", Additions: 180, Deletions: 40,
	}}
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	runner := fixture.autoRunner()

	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.UnreviewedMerged != 1 {
		t.Fatalf("first discovery = %#v, want one unreviewed merged PR", first)
	}
	events, err := fixture.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#91")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 1 || events[0].EventType != "pr.review.unreviewed" || !strings.Contains(events[0].PayloadJSON, `"changedLines":220`) {
		t.Fatalf("events = %#v, want one durable unreviewed event", events)
	}
	markerCalls := len(fixture.github.reviewMarkerCalls)
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.UnreviewedMerged != 0 || len(fixture.github.reviewMarkerCalls) != markerCalls {
		t.Fatalf("second discovery = %#v, marker calls=%d, want no duplicate lookup", second, len(fixture.github.reviewMarkerCalls))
	}
}

func TestAutoGatekeeperBoundsRefusedMergedReviewReconciliation(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = nil
	fixture.github.mergedPullRequests = []githubinfra.PullRequestSummary{{
		Number: 92, HeadSHA: "refused-head", MergedAt: "2026-07-30T12:00:00Z", Additions: 180, Deletions: 40,
	}}
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{}
	projectID, entityType, entityID, actorType, actorID := "project_1", "pull_request", "acme/looper#92", "system", "reviewer"
	if err := eventlog.Append(context.Background(), fixture.repos, eventlog.AppendInput{
		ID: "refused-review-92", EventType: "pr.review.refused", ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		ActorType: &actorType, ActorID: &actorID, Payload: map[string]any{"headSha": "refused-head", "reason": "rate_limit"}, CreatedAt: fixture.now,
	}); err != nil {
		t.Fatalf("append refusal evidence: %v", err)
	}
	runner := fixture.autoRunner()
	first, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("first DiscoverPullRequests() error = %v", err)
	}
	if first.UnreviewedMerged != 1 || len(fixture.github.reviewMarkerCalls) != 1 {
		t.Fatalf("first discovery = %#v, marker calls = %d, want one refusal reconciliation and durable negative evidence", first, len(fixture.github.reviewMarkerCalls))
	}
	second, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: projectID, Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("second DiscoverPullRequests() error = %v", err)
	}
	if second.UnreviewedMerged != 0 || len(fixture.github.reviewMarkerCalls) != 1 {
		t.Fatalf("second discovery = %#v, marker calls = %d, want bounded reconciliation", second, len(fixture.github.reviewMarkerCalls))
	}
	events, err := fixture.repos.Events.ListByEntity(context.Background(), entityType, entityID)
	if err != nil {
		t.Fatalf("list review evidence: %v", err)
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.EventType]++
	}
	if counts["pr.review.refused"] != 1 || counts["pr.review.unreviewed"] != 1 {
		t.Fatalf("review evidence counts = %#v, want one refusal and one settled negative row", counts)
	}
}

func TestAutoGatekeeperRejectsStaleOrBlockingCodexReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker githubinfra.ReviewMarkerResult
		want   ReasonCode
	}{
		{name: "stale review is not found for new head", marker: githubinfra.ReviewMarkerResult{}, want: ReasonCodexReviewRequired},
		{name: "blocking review", marker: githubinfra.ReviewMarkerResult{Found: true, Outcome: "blocking", Event: "REQUEST_CHANGES", AuthorLogin: "looper-bot"}, want: ReasonCodexReviewBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGatekeeperFixture(t)
			fixture.requireReviewBySize()
			fixture.github.protection.RequiredChecks = []string{"ci", RequiredStatusContext}
			fixture.github.protection.RequiredCheckRules = append(fixture.github.protection.RequiredCheckRules, githubinfra.RequiredCheckRule{Context: RequiredStatusContext})
			fixture.github.reviewMarker = tc.marker
			report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
			if err != nil {
				t.Fatalf("EvaluatePullRequest() error = %v", err)
			}
			if !slices.Contains(reasonCodes(report.Reasons), tc.want) {
				t.Fatalf("report = %#v, want %s", report, tc.want)
			}
			if got := fixture.github.labelAdds; len(got) != 0 {
				t.Fatalf("label adds = %#v, want no auto-merge route for rejected review", got)
			}
		})
	}
}

func TestAutoGatekeeperRefusesToReportSuccessWithoutProtectedContext(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.requireReviewBySize()
	fixture.github.protection.RequiredChecks = []string{"ci"}
	fixture.github.protection.RequiredCheckRules = []githubinfra.RequiredCheckRule{{Context: "ci", AppID: 15368}}
	fixture.github.reviewMarker = githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"}

	report, err := fixture.autoRunner().EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible || !slices.Contains(reasonCodes(report.Reasons), ReasonGatekeeperCheckRequired) {
		t.Fatalf("report = %#v, want missing protected context", report)
	}
	if got := fixture.github.labelAdds; len(got) != 0 {
		t.Fatalf("label adds = %#v, want no auto-merge route without protected context", got)
	}
}

type fakeGatekeeperGitHub struct {
	openPullRequests   []githubinfra.PullRequestSummary
	mergedPullRequests []githubinfra.PullRequestSummary
	mergedListCalls    int
	detail             githubinfra.PullRequestDetail
	mergeable          githubinfra.PullRequestDetail
	// mergeWatch is returned by ViewPullRequestMergeWatch when its state or
	// merge timestamp is set; the default detail keeps the pre-reconcile
	// behavior of returning the mergeable view.
	mergeWatch       githubinfra.PullRequestDetail
	mergeWatchInputs []githubinfra.ViewPullRequestInput
	protection       githubinfra.BranchProtection
	checks           githubinfra.PullRequestCheckRuns
	threads          []githubinfra.ReviewThread
	reviews          []githubinfra.ReviewSummary
	reviewsErr       error
	finalHeadSHA     string
	headSHAResponses []string
	finalBaseSHA     string
	protectionErr    error
	commentsErr      error
	// perPullRequestCalls counts the forge round trips that only a full evaluation
	// makes, so a test can prove a pull request was skipped rather than evaluated.
	perPullRequestCalls int

	currentLogin          string
	commentErr            error
	deletedIDs            []int64
	labelAdds             []githubinfra.PullRequestLabelsInput
	labelRemoves          []githubinfra.PullRequestLabelsInput
	labelErr              error
	commitStatuses        []githubinfra.CommitStatusInput
	statusErr             error
	validateMergifyErr    error
	validateMergifyCalls  int
	validateMergifyInputs []githubinfra.ValidateMergifyRoutingInput
	listCalls             int
	loginCalls            int
	comments              []githubinfra.CommentInfo
	createdBodies         []string
	updatedBodies         []string
	reviewMarker          githubinfra.ReviewMarkerResult
	reviewMarkerErr       error
	reviewMarkerCalls     []githubinfra.VerifyReviewMarkerInput
	statusCalls           []githubinfra.CommitStatusInput
	callSequence          []string
	viewErr               error
	mergedPullRequestsErr error
	listReviewThreadsHook func(*fakeGatekeeperGitHub) error
	// beforeView, when set, runs before each pull-request read, so a test can
	// inject a state change between the first evaluation and a later one.
	beforeView    func(*fakeGatekeeperGitHub)
	beforeThreads func(*fakeGatekeeperGitHub)
}

type fingerprintGatekeeperGitHub struct {
	*fakeGatekeeperGitHub
	fingerprintRepo string
}

func (f *fingerprintGatekeeperGitHub) MergifyRoutingContractFingerprint(_ context.Context, input githubinfra.ValidateMergifyRoutingInput) (string, error) {
	f.fingerprintRepo = input.Repo
	return "contract-digest", nil
}

func (f *fakeGatekeeperGitHub) GetCurrentUserLoginForRepo(context.Context, string, string) (string, error) {
	f.loginCalls++
	if f.currentLogin == "" {
		return "looper-bot", nil
	}
	return f.currentLogin, nil
}

func (f *fakeGatekeeperGitHub) ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error) {
	f.listCalls++
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	return f.comments, nil
}

func (f *fakeGatekeeperGitHub) ListPullRequestReviews(context.Context, githubinfra.ViewPullRequestInput) ([]githubinfra.ReviewSummary, error) {
	f.perPullRequestCalls++
	return f.reviews, f.reviewsErr
}

// ListIssueCommentsContaining mirrors the gateway's projection: only comments
// carrying one of the markers cross the boundary, so no test can accidentally
// rely on the detector seeing the whole conversation.
func (f *fakeGatekeeperGitHub) ListIssueCommentsContaining(_ context.Context, _ githubinfra.ViewIssueInput, markers []string) ([]githubinfra.CommentInfo, error) {
	f.perPullRequestCalls++
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	matched := make([]githubinfra.CommentInfo, 0)
	for _, comment := range f.comments {
		for _, marker := range markers {
			if strings.Contains(comment.Body, marker) {
				matched = append(matched, comment)
				break
			}
		}
	}
	return matched, nil
}

func (f *fakeGatekeeperGitHub) CreateIssueComment(_ context.Context, input githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error) {
	if f.commentErr != nil {
		return githubinfra.IssueCommentResult{}, f.commentErr
	}
	f.createdBodies = append(f.createdBodies, input.Body)
	f.comments = append(f.comments, githubinfra.CommentInfo{ID: int64(900 + len(f.comments)), Author: "looper-bot", Body: input.Body})
	return githubinfra.IssueCommentResult{ID: int64(900 + len(f.comments))}, nil
}

func (f *fakeGatekeeperGitHub) DeleteIssueComment(_ context.Context, input githubinfra.DeleteIssueCommentInput) error {
	f.deletedIDs = append(f.deletedIDs, input.CommentID)
	kept := f.comments[:0]
	for _, comment := range f.comments {
		if comment.ID != input.CommentID {
			kept = append(kept, comment)
		}
	}
	f.comments = kept
	return nil
}

func (f *fakeGatekeeperGitHub) UpdateIssueComment(_ context.Context, input githubinfra.UpdateIssueCommentInput) error {
	f.updatedBodies = append(f.updatedBodies, input.Body)
	for i := range f.comments {
		if f.comments[i].ID == input.CommentID {
			f.comments[i].Body = input.Body
		}
	}
	return nil
}

func (f *fakeGatekeeperGitHub) ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error) {
	return f.openPullRequests, nil
}
func (f *fakeGatekeeperGitHub) ListMergedPullRequests(context.Context, githubinfra.ListMergedPullRequestsInput) ([]githubinfra.PullRequestSummary, error) {
	f.mergedListCalls++
	f.callSequence = append(f.callSequence, "merged-review-backlog")
	return f.mergedPullRequests, f.mergedPullRequestsErr
}
func (f *fakeGatekeeperGitHub) ViewPullRequestForGatekeeper(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	f.perPullRequestCalls++
	if f.beforeView != nil {
		f.beforeView(f)
	}
	if f.viewErr != nil {
		return githubinfra.PullRequestDetail{}, f.viewErr
	}
	return f.detail, nil
}
func (f *fakeGatekeeperGitHub) ViewPullRequestMergeWatch(_ context.Context, input githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
	f.mergeWatchInputs = append(f.mergeWatchInputs, input)
	if f.mergeWatch.State != "" || f.mergeWatch.MergedAt != "" {
		return f.mergeWatch, nil
	}
	return f.mergeable, nil
}
func (f *fakeGatekeeperGitHub) GetBranchProtection(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
	f.perPullRequestCalls++
	return f.protection, f.protectionErr
}
func (f *fakeGatekeeperGitHub) ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error) {
	f.perPullRequestCalls++
	return f.checks, nil
}
func (f *fakeGatekeeperGitHub) ListReviewThreads(context.Context, githubinfra.ListReviewThreadsInput) ([]githubinfra.ReviewThread, error) {
	f.perPullRequestCalls++
	if f.beforeThreads != nil {
		f.beforeThreads(f)
	}
	return f.threads, nil
}
func (f *fakeGatekeeperGitHub) GetPullRequestHeadSHA(context.Context, githubinfra.ViewPullRequestInput) (string, error) {
	if len(f.headSHAResponses) > 0 {
		head := f.headSHAResponses[0]
		f.headSHAResponses = f.headSHAResponses[1:]
		return head, nil
	}
	return f.finalHeadSHA, nil
}

func (f *fakeGatekeeperGitHub) GetPullRequestHeadAndBaseSHA(context.Context, githubinfra.ViewPullRequestInput) (string, string, error) {
	if len(f.headSHAResponses) > 0 {
		head := f.headSHAResponses[0]
		f.headSHAResponses = f.headSHAResponses[1:]
		return head, f.finalBaseSHA, nil
	}
	return f.finalHeadSHA, f.finalBaseSHA, nil
}

func (f *fakeGatekeeperGitHub) AddPullRequestLabels(_ context.Context, input githubinfra.PullRequestLabelsInput) error {
	if f.labelErr != nil {
		return f.labelErr
	}
	f.labelAdds = append(f.labelAdds, input)
	return nil
}

func (f *fakeGatekeeperGitHub) RemovePullRequestLabels(_ context.Context, input githubinfra.PullRequestLabelsInput) error {
	if f.labelErr != nil {
		return f.labelErr
	}
	f.labelRemoves = append(f.labelRemoves, input)
	return nil
}
func (f *fakeGatekeeperGitHub) FindReviewMarker(_ context.Context, input githubinfra.VerifyReviewMarkerInput) (githubinfra.ReviewMarkerResult, error) {
	f.reviewMarkerCalls = append(f.reviewMarkerCalls, input)
	return f.reviewMarker, f.reviewMarkerErr
}
func (f *fakeGatekeeperGitHub) SetCommitStatus(_ context.Context, input githubinfra.CommitStatusInput) error {
	if f.statusErr != nil {
		f.callSequence = append(f.callSequence, "commit-status")
		return f.statusErr
	}
	f.statusCalls = append(f.statusCalls, input)
	f.commitStatuses = append(f.commitStatuses, input)
	f.callSequence = append(f.callSequence, "commit-status")
	return nil
}

func (f *fakeGatekeeperGitHub) ValidateMergifyRouting(_ context.Context, input githubinfra.ValidateMergifyRoutingInput) error {
	f.validateMergifyCalls++
	f.validateMergifyInputs = append(f.validateMergifyInputs, input)
	return f.validateMergifyErr
}

func reasonCodes(reasons []Reason) []ReasonCode {
	out := make([]ReasonCode, 0, len(reasons))
	for _, reason := range reasons {
		out = append(out, reason.Code)
	}
	return out
}
