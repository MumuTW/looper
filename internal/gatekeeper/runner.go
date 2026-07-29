// Package gatekeeper evaluates pull requests against merge policy and writes
// durable, observe-only gate reports. It never reviews, repairs, or merges.
package gatekeeper

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	GateReportEventType = "pull_request.merge_gate.evaluated"
	ModeObserveOnly     = "observe_only"
	StatusEligible      = "eligible"
	StatusBlocked       = "blocked"
	reportVersion       = 1
)

type ReasonCode string

const (
	ReasonHeadStale                ReasonCode = "head_stale"
	ReasonPullRequestNotOpen       ReasonCode = "pull_request_not_open"
	ReasonPullRequestDraft         ReasonCode = "pull_request_draft"
	ReasonMergeConflict            ReasonCode = "merge_conflict"
	ReasonMergeabilityNotClean     ReasonCode = "mergeability_not_clean"
	ReasonCheckMissing             ReasonCode = "required_check_missing"
	ReasonCheckPending             ReasonCode = "required_check_pending"
	ReasonCheckFailed              ReasonCode = "required_check_failed"
	ReasonCheckCancelled           ReasonCode = "required_check_cancelled"
	ReasonReviewRequired           ReasonCode = "required_review_missing"
	ReasonReviewChangesRequested   ReasonCode = "review_changes_requested"
	ReasonUnresolvedReviewThread   ReasonCode = "unresolved_review_thread"
	ReasonProjectPolicyDenied      ReasonCode = "project_policy_denied"
	ReasonHold                     ReasonCode = "hold"
	ReasonProviderStateUnavailable ReasonCode = "provider_state_unavailable"
	ReasonProviderStateAmbiguous   ReasonCode = "provider_state_ambiguous"
)

type Reason struct {
	Code    ReasonCode `json:"code"`
	Subject string     `json:"subject,omitempty"`
}

type CheckEvidence struct {
	Name       string `json:"name"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

type Evidence struct {
	PullRequestState             string          `json:"pullRequestState,omitempty"`
	Draft                        bool            `json:"draft"`
	BaseRefName                  string          `json:"baseRefName,omitempty"`
	Mergeable                    *bool           `json:"mergeable,omitempty"`
	MergeableState               string          `json:"mergeableState,omitempty"`
	RequiredChecks               []string        `json:"requiredChecks"`
	Checks                       []CheckEvidence `json:"checks"`
	RequiredApprovingReviewCount int             `json:"requiredApprovingReviewCount"`
	ReviewDecision               string          `json:"reviewDecision,omitempty"`
	UnresolvedReviewThreadIDs    []string        `json:"unresolvedReviewThreadIds"`
	HoldLabels                   []string        `json:"holdLabels"`
	ProjectPolicyPermitsTarget   bool            `json:"projectPolicyPermitsTarget"`
	FinalObservedHeadSHA         string          `json:"finalObservedHeadSha,omitempty"`
}

type Report struct {
	Version         int    `json:"version"`
	Mode            string `json:"mode"`
	Status          string `json:"status"`
	Eligible        bool   `json:"eligible"`
	ProjectID       string `json:"projectId"`
	Repo            string `json:"repo"`
	PRNumber        int64  `json:"prNumber"`
	ExpectedHeadSHA string `json:"expectedHeadSha,omitempty"`
	ObservedHeadSHA string `json:"observedHeadSha,omitempty"`
	// RequiresFreshRevalidation means a future merge path must rerun the full
	// evaluation immediately before merging. Comparing ObservedHeadSHA alone
	// is insufficient because holds, reviews, threads, and project policy can
	// change without moving the head.
	RequiresFreshRevalidation bool     `json:"requiresFreshRevalidation"`
	Reasons                   []Reason `json:"reasons"`
	Evidence                  Evidence `json:"evidence"`
	EvaluatedAt               string   `json:"evaluatedAt"`
}

type EvaluationInput struct {
	ProjectID       string
	Repo            string
	PRNumber        int64
	CWD             string
	ExpectedHeadSHA string
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	CWD       string
	Limit     int
	Snapshot  *githubinfra.DiscoverySnapshot
}

type DiscoveryResult struct {
	Evaluated int
	Reports   []Report
}

type GitHubGateway interface {
	ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error)
	ViewPullRequestForGatekeeper(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
	ViewPullRequestMergeWatch(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
	GetBranchProtection(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error)
	ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error)
	ListReviewThreads(context.Context, githubinfra.ListReviewThreadsInput) ([]githubinfra.ReviewThread, error)
	GetPullRequestHeadSHA(context.Context, githubinfra.ViewPullRequestInput) (string, error)
}

type Options struct {
	Repos               *storage.Repositories
	GitHub              GitHubGateway
	Now                 func() time.Time
	PolicyPermitsTarget func(projectID, repo, baseRefName string) bool
}

type Runner struct {
	repos               *storage.Repositories
	github              GitHubGateway
	now                 func() time.Time
	policyPermitsTarget func(projectID, repo, baseRefName string) bool
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	policy := options.PolicyPermitsTarget
	if policy == nil {
		policy = func(string, string, string) bool { return true }
	}
	return &Runner{repos: options.Repos, github: options.GitHub, now: now, policyPermitsTarget: policy}
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if r == nil || r.github == nil {
		return DiscoveryResult{}, fmt.Errorf("gatekeeper GitHub gateway is not configured")
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 100
	}
	if strings.TrimSpace(input.CWD) == "" {
		input.CWD = r.projectCWD(ctx, input.ProjectID)
	}
	listCtx := ctx
	if input.Snapshot != nil {
		listCtx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	}
	pullRequests, err := r.github.ListOpenPullRequests(listCtx, githubinfra.ListOpenPullRequestsInput{
		Repo: input.Repo, CWD: input.CWD, Limit: limit,
	})
	if err != nil {
		return DiscoveryResult{}, fmt.Errorf("list gatekeeper pull requests: %w", err)
	}
	result := DiscoveryResult{Reports: make([]Report, 0, len(pullRequests))}
	for _, pullRequest := range pullRequests {
		report, err := r.EvaluatePullRequest(ctx, EvaluationInput{
			ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: pullRequest.Number,
			CWD: input.CWD, ExpectedHeadSHA: pullRequest.HeadSHA,
		})
		if err != nil {
			return result, err
		}
		result.Evaluated++
		result.Reports = append(result.Reports, report)
	}
	return result, nil
}

func (r *Runner) EvaluatePullRequest(ctx context.Context, input EvaluationInput) (Report, error) {
	if r == nil || r.github == nil {
		return Report{}, fmt.Errorf("gatekeeper GitHub gateway is not configured")
	}
	if r.repos == nil || r.repos.Events == nil {
		return Report{}, fmt.Errorf("gatekeeper event repository is not configured")
	}
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.Repo = strings.TrimSpace(input.Repo)
	input.ExpectedHeadSHA = strings.TrimSpace(input.ExpectedHeadSHA)
	if input.ProjectID == "" || input.Repo == "" || input.PRNumber <= 0 {
		return Report{}, fmt.Errorf("gatekeeper project, repository, and pull request number are required")
	}
	if strings.TrimSpace(input.CWD) == "" {
		input.CWD = r.projectCWD(ctx, input.ProjectID)
	}

	report := Report{
		Version: reportVersion, Mode: ModeObserveOnly, Status: StatusBlocked,
		ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber,
		ExpectedHeadSHA: input.ExpectedHeadSHA, RequiresFreshRevalidation: true,
		Reasons: []Reason{}, Evidence: Evidence{RequiredChecks: []string{}, Checks: []CheckEvidence{}, UnresolvedReviewThreadIDs: []string{}, HoldLabels: []string{}},
		EvaluatedAt: r.now().UTC().Format(time.RFC3339Nano),
	}
	viewInput := githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD}
	detail, err := r.github.ViewPullRequestForGatekeeper(ctx, viewInput)
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "pull_request")
	}
	report.ObservedHeadSHA = strings.TrimSpace(detail.HeadSHA)
	report.Evidence.PullRequestState = strings.ToUpper(strings.TrimSpace(detail.State))
	report.Evidence.Draft = detail.IsDraft
	report.Evidence.BaseRefName = strings.TrimSpace(detail.BaseRefName)
	report.Evidence.ReviewDecision = strings.ToUpper(strings.TrimSpace(detail.ReviewDecision))
	if input.ExpectedHeadSHA != "" && report.ObservedHeadSHA != input.ExpectedHeadSHA {
		report.Reasons = []Reason{{Code: ReasonHeadStale}}
		return r.persist(ctx, report)
	}
	if detail.Number != input.PRNumber || report.ObservedHeadSHA == "" || report.Evidence.BaseRefName == "" {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "pull_request")
	}

	if report.Evidence.PullRequestState != "OPEN" {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonPullRequestNotOpen})
	}
	if detail.IsDraft {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonPullRequestDraft})
	}
	for _, label := range detail.Labels {
		if strings.TrimSpace(label) == domain.HoldLabelGlobal {
			report.Evidence.HoldLabels = append(report.Evidence.HoldLabels, domain.HoldLabelGlobal)
			report.Reasons = append(report.Reasons, Reason{Code: ReasonHold, Subject: domain.HoldLabelGlobal})
		}
	}
	report.Evidence.ProjectPolicyPermitsTarget = r.policyPermitsTarget(input.ProjectID, input.Repo, report.Evidence.BaseRefName)
	if !report.Evidence.ProjectPolicyPermitsTarget {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonProjectPolicyDenied})
	}

	mergeability, err := r.github.ViewPullRequestMergeWatch(ctx, viewInput)
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "mergeability")
	}
	if strings.TrimSpace(mergeability.HeadSHA) != report.ObservedHeadSHA {
		report.Reasons = []Reason{{Code: ReasonHeadStale}}
		report.Evidence.FinalObservedHeadSHA = strings.TrimSpace(mergeability.HeadSHA)
		return r.persist(ctx, report)
	}
	report.Evidence.Mergeable = mergeability.Mergeable
	report.Evidence.MergeableState = strings.ToLower(strings.TrimSpace(mergeability.MergeableState))
	if mergeability.Mergeable == nil || report.Evidence.MergeableState == "" || report.Evidence.MergeableState == "unknown" {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "mergeability")
	}
	if !*mergeability.Mergeable || report.Evidence.MergeableState == "dirty" {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonMergeConflict})
	} else if providerPolicyStateIsAmbiguous(report.Evidence.MergeableState) {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonProviderStateAmbiguous, Subject: "mergeability:" + report.Evidence.MergeableState})
	} else if report.Evidence.MergeableState != "clean" {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonMergeabilityNotClean, Subject: report.Evidence.MergeableState})
	}

	protection, err := r.github.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: input.Repo, Branch: report.Evidence.BaseRefName, CWD: input.CWD})
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "branch_protection")
	}
	report.Evidence.RequiredChecks = sortedUnique(protection.RequiredChecks)
	report.Evidence.RequiredApprovingReviewCount = protection.RequiredApprovingReviewCount

	checks, err := r.github.ListPullRequestCheckRuns(ctx, githubinfra.PullRequestCheckRunsInput{Repo: input.Repo, Ref: report.ObservedHeadSHA, CWD: input.CWD})
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "checks")
	}
	checkReasons, checkEvidence := evaluateRequiredChecks(report.Evidence.RequiredChecks, checks)
	report.Reasons = append(report.Reasons, checkReasons...)
	report.Evidence.Checks = checkEvidence

	switch report.Evidence.ReviewDecision {
	case "CHANGES_REQUESTED":
		report.Reasons = append(report.Reasons, Reason{Code: ReasonReviewChangesRequested})
	case "REVIEW_REQUIRED":
		report.Reasons = append(report.Reasons, Reason{Code: ReasonReviewRequired})
	default:
		if (protection.HasRequiredReviews || protection.RequiredApprovingReviewCount > 0) && report.Evidence.ReviewDecision != "APPROVED" {
			report.Reasons = append(report.Reasons, Reason{Code: ReasonReviewRequired})
		}
	}

	threads, err := r.github.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, Limit: 1000})
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "review_threads")
	}
	if len(threads) == 1000 {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "review_threads")
	}
	for _, thread := range threads {
		if !thread.IsResolved {
			report.Evidence.UnresolvedReviewThreadIDs = append(report.Evidence.UnresolvedReviewThreadIDs, thread.ID)
			report.Reasons = append(report.Reasons, Reason{Code: ReasonUnresolvedReviewThread, Subject: thread.ID})
		}
	}
	sort.Strings(report.Evidence.UnresolvedReviewThreadIDs)

	finalHead, err := r.github.GetPullRequestHeadSHA(ctx, viewInput)
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "head_revalidation")
	}
	report.Evidence.FinalObservedHeadSHA = strings.TrimSpace(finalHead)
	if report.Evidence.FinalObservedHeadSHA == "" {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "head_revalidation")
	}
	if report.Evidence.FinalObservedHeadSHA != report.ObservedHeadSHA {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonHeadStale})
	}
	return r.persist(ctx, report)
}

func (r *Runner) projectCWD(ctx context.Context, projectID string) string {
	if r == nil || r.repos == nil || r.repos.Projects == nil || strings.TrimSpace(projectID) == "" {
		return ""
	}
	project, err := r.repos.Projects.GetByID(ctx, strings.TrimSpace(projectID))
	if err != nil || project == nil {
		return ""
	}
	return project.RepoPath
}

func (r *Runner) persistProviderBlock(ctx context.Context, report Report, code ReasonCode, subject string) (Report, error) {
	report.Reasons = []Reason{{Code: code, Subject: subject}}
	return r.persist(ctx, report)
}

func (r *Runner) persist(ctx context.Context, report Report) (Report, error) {
	sortReasons(report.Reasons)
	report.Eligible = len(report.Reasons) == 0
	if report.Eligible {
		report.Status = StatusEligible
	} else {
		report.Status = StatusBlocked
	}
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", report.Repo, report.PRNumber)
	projectID := report.ProjectID
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: report, CreatedAt: r.now(),
	}); err != nil {
		return Report{}, fmt.Errorf("persist gate report: %w", err)
	}
	return report, nil
}

func evaluateRequiredChecks(required []string, checks githubinfra.PullRequestCheckRuns) ([]Reason, []CheckEvidence) {
	runs := make(map[string]githubinfra.PullRequestCheckRun, len(checks.CheckRuns))
	for _, check := range checks.CheckRuns {
		runs[strings.ToLower(strings.TrimSpace(check.Name))] = check
	}
	statuses := make(map[string]githubinfra.PullRequestStatus, len(checks.Statuses))
	for _, status := range checks.Statuses {
		statuses[strings.ToLower(strings.TrimSpace(status.Context))] = status
	}
	reasons := make([]Reason, 0)
	evidence := make([]CheckEvidence, 0, len(required))
	for _, name := range required {
		key := strings.ToLower(strings.TrimSpace(name))
		if check, ok := runs[key]; ok {
			status := strings.ToLower(strings.TrimSpace(check.Status))
			conclusion := strings.ToLower(strings.TrimSpace(check.Conclusion))
			evidence = append(evidence, CheckEvidence{Name: name, Status: status, Conclusion: conclusion})
			switch {
			case status != "completed":
				reasons = append(reasons, Reason{Code: ReasonCheckPending, Subject: name})
			case conclusion == "success":
			case conclusion == "cancelled":
				reasons = append(reasons, Reason{Code: ReasonCheckCancelled, Subject: name})
			default:
				reasons = append(reasons, Reason{Code: ReasonCheckFailed, Subject: name})
			}
			continue
		}
		if status, ok := statuses[key]; ok {
			state := strings.ToLower(strings.TrimSpace(status.State))
			evidence = append(evidence, CheckEvidence{Name: name, Status: state})
			switch state {
			case "success":
			case "pending":
				reasons = append(reasons, Reason{Code: ReasonCheckPending, Subject: name})
			default:
				reasons = append(reasons, Reason{Code: ReasonCheckFailed, Subject: name})
			}
			continue
		}
		evidence = append(evidence, CheckEvidence{Name: name})
		reasons = append(reasons, Reason{Code: ReasonCheckMissing, Subject: name})
	}
	return reasons, evidence
}

func sortedUnique(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortReasons(reasons []Reason) {
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i].Code != reasons[j].Code {
			return reasons[i].Code < reasons[j].Code
		}
		return reasons[i].Subject < reasons[j].Subject
	})
}

func providerPolicyStateIsAmbiguous(state string) bool {
	switch state {
	case "blocked", "unstable", "has_hooks":
		return true
	default:
		return false
	}
}
