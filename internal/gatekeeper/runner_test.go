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
	if len(gateReports) != 1 {
		t.Fatalf("events = %#v, want one durable gate report", events)
	}
	var persisted Report
	if err := json.Unmarshal([]byte(gateReports[0].PayloadJSON), &persisted); err != nil {
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
			detail:    githubinfra.PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", BaseRefName: "main", ReviewDecision: "APPROVED"},
			mergeable: githubinfra.PullRequestDetail{Number: 42, HeadSHA: "head-1", Mergeable: &mergeable, MergeableState: "clean"},
			protection: githubinfra.BranchProtection{
				Enabled: true, HasRequiredChecks: true, RequiredChecks: []string{"ci", RequiredStatusContext},
				RequiredCheckRules: []githubinfra.RequiredCheckRule{{Context: "ci", AppID: 15368}},
				HasRequiredReviews: true, RequiredApprovingReviewCount: 1,
			},
			checks:       githubinfra.PullRequestCheckRuns{TotalCount: 1, CheckRuns: []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "success", AppID: 15368}}},
			finalHeadSHA: "head-1",
			reviewMarker: githubinfra.ReviewMarkerResult{Found: true, Outcome: "clean", Event: "APPROVE", AuthorLogin: "looper-bot"},
		},
		policyPermits: true,
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

func TestAutoGatekeeperRequiresCurrentHeadCodexReviewAndPublishesPendingStatus(t *testing.T) {
	fixture := newGatekeeperFixture(t)
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
	if got := fixture.github.statusCalls; len(got) != 1 || got[0].SHA != "head-1" || got[0].Context != RequiredStatusContext || got[0].State != "pending" {
		t.Fatalf("status calls = %#v, want pending status for head-1", got)
	}
	if got := fixture.github.reviewMarkerCalls; len(got) != 1 || got[0].Marker != "looper:review id_prefix=reviewer: head=head-1" || got[0].AuthorLogin != "looper-bot" {
		t.Fatalf("review marker calls = %#v, want exact current-head marker and Looper author", got)
	}
}

func TestAutoGatekeeperAcceptsCleanCurrentHeadCodexReview(t *testing.T) {
	fixture := newGatekeeperFixture(t)
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

func TestAutoGatekeeperRejectsStaleOrBlockingCodexReview(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker githubinfra.ReviewMarkerResult
		want   ReasonCode
		state  string
	}{
		{name: "stale review is not found for new head", marker: githubinfra.ReviewMarkerResult{}, want: ReasonCodexReviewRequired, state: "pending"},
		{name: "blocking review", marker: githubinfra.ReviewMarkerResult{Found: true, Outcome: "blocking", Event: "REQUEST_CHANGES", AuthorLogin: "looper-bot"}, want: ReasonCodexReviewBlocked, state: "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGatekeeperFixture(t)
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
			if got := fixture.github.statusCalls; len(got) != 1 || got[0].State != tc.state {
				t.Fatalf("status calls = %#v, want %s", got, tc.state)
			}
		})
	}
}

func TestAutoGatekeeperRefusesToReportSuccessWithoutProtectedContext(t *testing.T) {
	fixture := newGatekeeperFixture(t)
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
	if got := fixture.github.statusCalls; len(got) != 1 || got[0].State != "failure" {
		t.Fatalf("status calls = %#v, want failure", got)
	}
}

type fakeGatekeeperGitHub struct {
	openPullRequests []githubinfra.PullRequestSummary
	detail           githubinfra.PullRequestDetail
	mergeable        githubinfra.PullRequestDetail
	protection       githubinfra.BranchProtection
	checks           githubinfra.PullRequestCheckRuns
	threads          []githubinfra.ReviewThread
	reviews          []githubinfra.ReviewSummary
	reviewsErr       error
	finalHeadSHA     string
	finalBaseSHA     string
	protectionErr    error
	commentsErr      error
	// perPullRequestCalls counts the forge round trips that only a full evaluation
	// makes, so a test can prove a pull request was skipped rather than evaluated.
	perPullRequestCalls int

	currentLogin      string
	commentErr        error
	deletedIDs        []int64
	listCalls         int
	loginCalls        int
	comments          []githubinfra.CommentInfo
	createdBodies     []string
	updatedBodies     []string
	reviewMarker      githubinfra.ReviewMarkerResult
	reviewMarkerErr   error
	reviewMarkerCalls []githubinfra.VerifyReviewMarkerInput
	statusCalls       []githubinfra.CommitStatusInput
	statusErr         error
	viewErr           error
	// beforeView, when set, runs before each pull-request read, so a test can
	// inject a state change between the first evaluation and a later one.
	beforeView func(*fakeGatekeeperGitHub)
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
func (f *fakeGatekeeperGitHub) ViewPullRequestMergeWatch(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error) {
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
	return f.threads, nil
}
func (f *fakeGatekeeperGitHub) GetPullRequestHeadAndBaseSHA(context.Context, githubinfra.ViewPullRequestInput) (string, string, error) {
	return f.finalHeadSHA, f.finalBaseSHA, nil
}
func (f *fakeGatekeeperGitHub) FindReviewMarker(_ context.Context, input githubinfra.VerifyReviewMarkerInput) (githubinfra.ReviewMarkerResult, error) {
	f.reviewMarkerCalls = append(f.reviewMarkerCalls, input)
	return f.reviewMarker, f.reviewMarkerErr
}
func (f *fakeGatekeeperGitHub) SetCommitStatus(_ context.Context, input githubinfra.CommitStatusInput) error {
	f.statusCalls = append(f.statusCalls, input)
	return f.statusErr
}

func reasonCodes(reasons []Reason) []ReasonCode {
	out := make([]ReasonCode, 0, len(reasons))
	for _, reason := range reasons {
		out = append(out, reason.Code)
	}
	return out
}
