// Package gatekeeper evaluates pull requests against merge policy and writes
// durable, observe-only gate reports. It never reviews, repairs, or merges.
package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

const (
	// reportVersion is 2 because Report.Mode changed meaning: it now carries the
	// project's configured trust level, spelled as in configuration, where version 1
	// always wrote the literal "observe_only". Readers must branch on Version rather
	// than assume a spelling — that no consumer reads Mode today does not make the
	// persisted format free to redefine in place.
	//
	// GateReportEventType is the Gate report: the durable event written by
	// Merge Gatekeeper recording eligible or blocked, stable reasons and
	// evidence, and the observed head SHA. It is audit evidence, not merge
	// authority — a future merge path must rerun every gate immediately
	// before merging, because holds, reviews, threads, and Project policy can
	// change without moving the head.
	GateReportEventType = "pull_request.merge_gate.evaluated"
	StatusEligible      = "eligible"
	StatusBlocked       = "blocked"
	reportVersion       = 2

	// DefaultDiscoveryPullRequestLimit is how many open pull requests this lane
	// lists when the caller sets no limit. Exported because the scheduler sizes its
	// shared discovery snapshot from it: a snapshot page smaller than this always
	// fails the snapshot's truncation check, so the lane discards the page and
	// issues its own raw query, paying for both.
	DefaultDiscoveryPullRequestLimit = 100
)

type ReasonCode string

const (
	ReasonHeadStale                ReasonCode = "head_stale"
	ReasonBaseStale                ReasonCode = "base_stale"
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
	ReasonCodexReviewMissing       ReasonCode = "codex_review_missing"
	ReasonUnresolvedReviewThread   ReasonCode = "unresolved_review_thread"
	ReasonReviewerConvergence      ReasonCode = "reviewer_convergence_blocked"
	ReasonCodexReviewRequired      ReasonCode = "codex_review_required"
	ReasonCodexReviewBlocked       ReasonCode = "codex_review_blocked"
	ReasonGatekeeperCheckRequired  ReasonCode = "gatekeeper_check_not_required"
	ReasonProjectPolicyDenied      ReasonCode = "project_policy_denied"
	ReasonHold                     ReasonCode = "hold"
	ReasonDoNotMerge               ReasonCode = "do_not_merge"
	ReasonDiffBudgetExceeded       ReasonCode = "diff_budget_exceeded"
	ReasonProviderStateUnavailable ReasonCode = "provider_state_unavailable"
	ReasonProviderStateAmbiguous   ReasonCode = "provider_state_ambiguous"
	ReasonProtectedPathTouched     ReasonCode = "protected_path_touched"
)

type Reason struct {
	Code    ReasonCode `json:"code"`
	Subject string     `json:"subject,omitempty"`
}

type CheckEvidence struct {
	Name       string `json:"name"`
	AppID      int64  `json:"appId,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

// DiffBudgetEvidence records the provider-observed change size and the
// configured bounds that were applied. See config.GatekeeperDiffBudget for the
// gate's semantics and the blind spots it does not catch (no additions or
// per-file bound, generated files not excluded, whole-PR totals against the
// current merge base, and the merge action binding only the head — see
// MergePullRequest — so a base advance between the final revalidation read and
// the merge is not atomically refused).
type DiffBudgetEvidence struct {
	ChangedFiles    int `json:"changedFiles"`
	Deletions       int `json:"deletions"`
	MaxChangedFiles int `json:"maxChangedFiles"`
	MaxDeletions    int `json:"maxDeletions"`
}

// CodexReviewEvidence is the durable Reviewer review signal considered by the
// current-head gate. The event log is the authority because Reviewer appends
// it only after its structured review marker has been verified.
type CodexReviewEvidence struct {
	RequiredHeadSHA  string `json:"requiredHeadSha"`
	ReviewedHeadSHA  string `json:"reviewedHeadSha,omitempty"`
	Event            string `json:"event,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	RecordedAt       string `json:"recordedAt,omitempty"`
	CurrentHeadValid bool   `json:"currentHeadValid"`
}

type Evidence struct {
	PullRequestState             string                       `json:"pullRequestState,omitempty"`
	ClosedAt                     string                       `json:"closedAt,omitempty"`
	MergedAt                     string                       `json:"mergedAt,omitempty"`
	Draft                        bool                         `json:"draft"`
	BaseRefName                  string                       `json:"baseRefName,omitempty"`
	BaseSHA                      string                       `json:"baseSha,omitempty"`
	Mergeable                    *bool                        `json:"mergeable,omitempty"`
	MergeableState               string                       `json:"mergeableState,omitempty"`
	RequiredChecks               []string                     `json:"requiredChecks"`
	Checks                       []CheckEvidence              `json:"checks"`
	RequiredApprovingReviewCount int                          `json:"requiredApprovingReviewCount"`
	ReviewDecision               string                       `json:"reviewDecision,omitempty"`
	CodexReview                  *CodexReviewEvidence         `json:"codexReview,omitempty"`
	UnresolvedReviewThreadIDs    []string                     `json:"unresolvedReviewThreadIds"`
	HoldLabels                   []string                     `json:"holdLabels"`
	DiffBudget                   *DiffBudgetEvidence          `json:"diffBudget,omitempty"`
	ProtectedPaths               []string                     `json:"protectedPaths,omitempty"`
	ReviewerConvergence          *ReviewerConvergenceEvidence `json:"reviewerConvergence,omitempty"`
	ProjectPolicyPermitsTarget   bool                         `json:"projectPolicyPermitsTarget"`
	FinalObservedHeadSHA         string                       `json:"finalObservedHeadSha,omitempty"`
	FinalObservedBaseSHA         string                       `json:"finalObservedBaseSha,omitempty"`
	CodexReviewOutcome           string                       `json:"codexReviewOutcome,omitempty"`
	ReviewProvenance             ReviewProvenance             `json:"reviewProvenance,omitempty"`
	Additions                    int                          `json:"additions,omitempty"`
	Deletions                    int                          `json:"deletions,omitempty"`
	ChangedLines                 int                          `json:"changedLines,omitempty"`
	ReviewRequiredByPolicy       bool                         `json:"reviewRequiredByPolicy"`
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
	// SourceFingerprint is everything the shared discovery list page could observe
	// about the pull request when this report was produced. The next tick compares
	// it to decide whether re-evaluating would reach the same conclusion.
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
}

type EvaluationInput struct {
	ProjectID       string
	Repo            string
	PRNumber        int64
	CWD             string
	ExpectedHeadSHA string
	ExpectedBaseSHA string
	// SourceFingerprint is recorded on the resulting report. Empty means the caller
	// had no list-page observation, in which case the report can never be skipped.
	SourceFingerprint string
	// Confirming marks the second evaluation performed immediately before a merge.
	// It publishes no verdict and triggers no merge of its own: it exists only to
	// re-establish that the gates still pass.
	Confirming bool
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
	// Skipped counts pull requests that reused their previous report because
	// nothing the list page can observe had changed.
	Skipped int
	// Reconciled counts pull requests that left the open set since the last tick
	// and were given a final report. It is separate from Evaluated because it is
	// the count that must sit at zero in steady state.
	Reconciled int
	Reports    []Report
	// UnreviewedMerged counts new durable negative review-capacity records from
	// the bounded recent merged-PR reconciliation.
	UnreviewedMerged int
}

type GitHubGateway interface {
	ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error)
	ListMergedPullRequests(context.Context, githubinfra.ListMergedPullRequestsInput) ([]githubinfra.PullRequestSummary, error)
	ViewPullRequestForGatekeeper(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
	ViewPullRequestMergeWatch(context.Context, githubinfra.ViewPullRequestInput) (githubinfra.PullRequestDetail, error)
	GetBranchProtection(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error)
	ListPullRequestCheckRuns(context.Context, githubinfra.PullRequestCheckRunsInput) (githubinfra.PullRequestCheckRuns, error)
	ListReviewThreads(context.Context, githubinfra.ListReviewThreadsInput) ([]githubinfra.ReviewThread, error)
	ListPullRequestReviews(context.Context, githubinfra.ViewPullRequestInput) ([]githubinfra.ReviewSummary, error)
	// GetPullRequestHeadAndBaseSHA is the final revalidation read. It returns
	// both the head and base ref OIDs so a gate whose verdict depends on the
	// merge base (the diff budget) can detect a base advance that the head
	// revalidation alone cannot.
	GetPullRequestHeadAndBaseSHA(context.Context, githubinfra.ViewPullRequestInput) (string, string, error)
	GetPullRequestBaseSHA(context.Context, githubinfra.ViewPullRequestInput) (string, error)
	ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error)
	ListIssueCommentsContaining(context.Context, githubinfra.ViewIssueInput, []string) ([]githubinfra.CommentInfo, error)
	CreateIssueComment(context.Context, githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error)
	UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error
	GetCurrentUserLoginForRepo(context.Context, string, string) (string, error)
	DeleteIssueComment(context.Context, githubinfra.DeleteIssueCommentInput) error
	FindReviewMarker(context.Context, githubinfra.VerifyReviewMarkerInput) (githubinfra.ReviewMarkerResult, error)
	SetCommitStatus(context.Context, githubinfra.CommitStatusInput) error
	MergePullRequest(context.Context, githubinfra.PullRequestMergeInput) error
}

// RequiredStatusContext is the GitHub status context an operator adds to branch
// protection when promoting Gatekeeper to auto. GitHub branch protection is the
// merge authority; this runner reports policy for one commit.
const RequiredStatusContext = "Looper Gatekeeper"

// DefaultRequiredReviewChangedLines is the deliberate capacity policy when an
// operator has not set a project override. An explicit zero disables the
// threshold after configuration normalization.
const DefaultRequiredReviewChangedLines = 200

type Options struct {
	Repos  *storage.Repositories
	GitHub GitHubGateway
	Now    func() time.Time
	// LabelNamespaceForProject resolves config-managed projects. API-managed
	// projects fall back to their persisted catalog metadata in the runner.
	// The boolean distinguishes an unconfigured project from an explicit
	// default namespace so persisted metadata is not accidentally shadowed.
	LabelNamespaceForProject func(projectID string) (labels.Namespace, bool)
	PolicyPermitsTarget      func(projectID, repo, baseRefName string) bool
	// TrustForProject reports a project's merge-authority level. Nil means every
	// project stays at observe, which is also the configured default.
	TrustForProject func(projectID string) config.GatekeeperTrustLevel
	// DiffBudgetForProject returns the effective boolean change-size limits. Zero
	// bounds are unlimited; nil means every project has no configured limit.
	DiffBudgetForProject func(projectID string) config.GatekeeperDiffBudget
	// ConfiguredTargetBranch reports the project policy's configured base branch.
	// It is folded into the discovery fingerprint so a policy generation change
	// invalidates reused success within the skip window.
	ConfiguredTargetBranch func(projectID string) string
	// MergeStrategyForProject selects the strategy used at the auto trust level.
	MergeStrategyForProject func(projectID string) config.MergeStrategy
	// RequiredReviewChangedLinesForProject returns the effective review-capacity
	// threshold. Zero explicitly disables the threshold; a normalized config
	// default supplies the ordinary 200-line policy when omitted.
	RequiredReviewChangedLinesForProject func(projectID string) int
	// ProtectedPathsForProject returns repository-relative globs that require
	// human review before an auto-trust merge.
	ProtectedPathsForProject func(projectID string) []string
	LogWarn                  func(msg string, fields map[string]any)
}

// Runner is the Merge Gatekeeper: a reactive, agent-free policy Role that
// re-fetches current Pull Request state and writes an observe-only Gate
// report. It never reviews code, repairs a Pull Request, resolves comments,
// or merges.
// Stateful: agent-free but not database-free — it persists Gate reports in
// the local SQLite event log.
type Runner struct {
	repos                                *storage.Repositories
	github                               GitHubGateway
	now                                  func() time.Time
	labelNamespaceForProject             func(projectID string) (labels.Namespace, bool)
	policyPermitsTarget                  func(projectID, repo, baseRefName string) bool
	trustForProject                      func(projectID string) config.GatekeeperTrustLevel
	mergeStrategyForProject              func(projectID string) config.MergeStrategy
	diffBudgetForProject                 func(projectID string) config.GatekeeperDiffBudget
	requiredReviewChangedLinesForProject func(projectID string) int
	reviewEvidenceLookup                 reviewEvidenceLookup
	configuredTargetBranch               func(projectID string) string
	protectedPathsForProject             func(projectID string) []string
	logWarn                              func(msg string, fields map[string]any)
	lastPublishedStatusMu                sync.Mutex
	lastPublishedStatus                  map[string]string
}

// reviewEvidenceLookup is kept as a narrow seam around the local event-log
// read. Production always uses reviewerReviewEvidenceAppearedSince; the seam
// makes the transient-read failure path deterministic in lifecycle tests without
// replacing the storage repository or sleeping around a scheduler tick.
type reviewEvidenceLookup func(context.Context, *storage.Repositories, string, string, int64, string, string, string) (bool, error)

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	policy := options.PolicyPermitsTarget
	if policy == nil {
		policy = func(string, string, string) bool { return true }
	}
	return &Runner{
		repos: options.Repos, github: options.GitHub, now: now, policyPermitsTarget: policy,
		trustForProject: options.TrustForProject, mergeStrategyForProject: options.MergeStrategyForProject,
		diffBudgetForProject:                 options.DiffBudgetForProject,
		requiredReviewChangedLinesForProject: options.RequiredReviewChangedLinesForProject,
		reviewEvidenceLookup:                 reviewerReviewEvidenceAppearedSince,
		labelNamespaceForProject:             options.LabelNamespaceForProject,
		configuredTargetBranch:               options.ConfiguredTargetBranch,
		protectedPathsForProject:             options.ProtectedPathsForProject,
		logWarn:                              options.LogWarn,
	}
}

func (r *Runner) DiscoverPullRequests(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if r == nil || r.github == nil {
		return DiscoveryResult{}, fmt.Errorf("gatekeeper GitHub gateway is not configured")
	}
	limit := input.Limit
	if limit <= 0 {
		limit = DefaultDiscoveryPullRequestLimit
	}
	if strings.TrimSpace(input.CWD) == "" {
		input.CWD = r.projectCWD(ctx, input.ProjectID)
	}
	// Reconcile any pre-forge merge marker before listing open PRs.  A PR that
	// merged just before the final SQLite append is no longer on that list, so
	// recovery must be driven by the durable event log instead.
	if err := r.reconcilePendingMergeOutcomes(ctx, input.ProjectID, input.Repo, input.CWD); err != nil {
		return DiscoveryResult{}, err
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
	// Evaluating a pull request costs branch protection, check runs, head SHA, and a
	// review-thread query each — so this lane was O(open PRs) in forge round trips
	// every tick, and grew with the repo. Most of those pull requests are unchanged
	// since their last evaluation, and the list call already made above can prove it.
	previousReports, err := latestGateReports(ctx, r.repos, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	// Convergence state is durable Reviewer metadata in SQLite, so a blocked
	// report can be reused unless that metadata advanced. Reading the current
	// convergence revision per pull request in one local query lets the
	// unchanged path detect Reviewer progress without re-polling the forge on
	// every tick.
	convergenceRevisions, err := latestConvergenceRevisions(ctx, r.repos, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	ctx = withDeferCommitStatus(ctx)
	trust := r.trustFor(input.ProjectID)
	result := DiscoveryResult{Reports: make([]Report, 0, len(pullRequests))}
	// Deferred commit statuses must still publish on abort: a blocked report that
	// was already evaluated this tick is durable in result.Reports, and leaving
	// without publishing would keep a prior success visible to branch protection.
	publishDeferredStatuses := func() {
		if trust == config.GatekeeperTrustAuto {
			r.publishDiscoveryCommitStatuses(ctx, input.ProjectID, input.Repo, input.CWD, result.Reports)
		}
	}
	diffBudget := r.diffBudget(input.ProjectID)
	budgetEnabled := diffBudget.MaxChangedFiles > 0 || diffBudget.MaxDeletions > 0
	configuredTarget := r.configuredTarget(input.ProjectID)
	reviewThreshold := r.requiredReviewChangedLinesFor(input.ProjectID)
	reviewPolicyEnabled := trust == config.GatekeeperTrustAuto && reviewThreshold > 0
	protectedPaths := r.protectedPaths(input.ProjectID)
	stillOpen := make(map[string]struct{}, len(pullRequests))
	for _, pullRequest := range pullRequests {
		// A budget change is a gate-input change even when the PR list page is
		// otherwise unchanged; include it so enabling or disabling the gate does
		// not wait for the periodic maxSkipAge backstop. BaseSHA is folded into
		// the fingerprint only while the budget is enabled, so a base advance on
		// a repo with no configured limit does not invalidate every open report.
		policyPermits := r.policyPermitsTarget(input.ProjectID, input.Repo, pullRequest.BaseRefName)
		fingerprint := sourceFingerprint(pullRequest, budgetEnabled, protectedPaths, reviewPolicyEnabled) + fmt.Sprintf("\x1fdiff-budget=%d,%d", diffBudget.MaxChangedFiles, diffBudget.MaxDeletions) + fmt.Sprintf("\x1fgatekeeper-trust=%s", trust) + fmt.Sprintf("\x1fconfigured-target=%s", configuredTarget) + fmt.Sprintf("\x1fpolicy-permits=%t", policyPermits) + fmt.Sprintf("\x1freview-threshold=%d", reviewThreshold)
		entityID := fmt.Sprintf("%s#%d", input.Repo, pullRequest.Number)
		stillOpen[entityID] = struct{}{}
		previous, hasPrevious := previousReports[entityID]
		if hasPrevious && strings.ToLower(strings.TrimSpace(previous.Mode)) != strings.ToLower(strings.TrimSpace(string(r.trustFor(input.ProjectID)))) {
			// Trust promotion is a gate input even when the forge-visible PR
			// fingerprint is unchanged; do not reuse an observe/advise report
			// after the project policy moves up the ladder.
			hasPrevious = false
		}
		// Check the local event log cheaply before deciding to skip. This avoids a
		// full forge evaluation every tick for PRs that may never receive a
		// Reviewer review (unrequested, self-authored), while also invalidating a
		// below-threshold auto success if a later verified review arrives.
		reviewEvidenceRefreshRequired := false
		if hasPrevious && previous.SourceFingerprint == fingerprint && (reportAwaitsCurrentHeadReview(previous) || trust == config.GatekeeperTrustAuto) {
			previousRecordedAt := ""
			if previous.Evidence.CodexReview != nil {
				previousRecordedAt = previous.Evidence.CodexReview.RecordedAt
			}
			lookup := r.reviewEvidenceLookup
			if lookup == nil {
				lookup = reviewerReviewEvidenceAppearedSince
			}
			if appeared, err := lookup(ctx, r.repos, input.ProjectID, input.Repo, pullRequest.Number, pullRequest.HeadSHA, previous.EvaluatedAt, previousRecordedAt); err == nil {
				reviewEvidenceRefreshRequired = appeared
			} else {
				// An unavailable local event log cannot prove that no new blocking
				// review arrived. Re-evaluate so a stale success is not reused while
				// the evidence lookup is failing.
				reviewEvidenceRefreshRequired = true
				if r.logWarn != nil {
					r.logWarn("gatekeeper review evidence lookup failed", map[string]any{"projectId": input.ProjectID, "repo": input.Repo, "prNumber": pullRequest.Number, "error": err.Error()})
				}
			}
		}
		// Auto trust is an acting authority: every tick must run the complete
		// confirming path, even when the list-page fingerprint is unchanged. A
		// trust transition also invalidates the prior mode's report so a
		// demotion cannot reuse an auto-mode verdict (and its owned comment).
		canReuse := trust != config.GatekeeperTrustAuto && hasPrevious && (previous.Mode == "" || previous.Mode == string(trust))
		if canReuse {
			if reused, ok := skipUnchanged(previous, hasPrevious, fingerprint, r.now(), convergenceRevisions[entityID], reviewEvidenceRefreshRequired); ok {
				result.Skipped++
				result.Reports = append(result.Reports, reused)
				continue
			}
		}
		report, err := r.EvaluatePullRequest(ctx, EvaluationInput{
			ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: pullRequest.Number,
			CWD: input.CWD, ExpectedHeadSHA: pullRequest.HeadSHA, SourceFingerprint: fingerprint,
		})
		if err != nil {
			publishDeferredStatuses()
			return result, err
		}
		result.Evaluated++
		result.Reports = append(result.Reports, report)
	}

	for _, entityID := range r.departedFromOpenSet(input.Repo, pullRequests, limit, previousReports, stillOpen) {
		report, err := r.EvaluatePullRequest(ctx, EvaluationInput{
			ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: previousReports[entityID].PRNumber, CWD: input.CWD,
		})
		if err != nil {
			publishDeferredStatuses()
			return result, err
		}
		result.Reconciled++
		result.Reports = append(result.Reports, report)
	}
	// Publish the current open-PR verdicts before the best-effort historical
	// scan. Marker lookups for up to one page of merged PRs may be slow or
	// rate-limited; branch protection must not keep an older success visible
	// while the freshly evaluated report waits behind that audit work.
	publishDeferredStatuses()
	if trust == config.GatekeeperTrustAuto && reviewThreshold > 0 {
		unreviewed, err := r.recordRecentMergedReviewBacklog(ctx, input)
		if err != nil {
			if r.logWarn != nil {
				r.logWarn("gatekeeper merged review backlog scan failed", map[string]any{"projectId": input.ProjectID, "repo": input.Repo, "error": err.Error()})
			}
		} else {
			result.UnreviewedMerged = unreviewed
		}
	}
	publishDeferredStatuses()
	return result, nil
}

// recordRecentMergedReviewBacklog makes the bounded recent merged-PR review
// capacity observation durable. It is deliberately best-effort at the caller:
// open-PR gate evaluation and status publication are merge-policy work, while
// this scan is audit evidence about already merged changes.
func (r *Runner) recordRecentMergedReviewBacklog(ctx context.Context, input DiscoveryInput) (int, error) {
	threshold := r.requiredReviewChangedLinesFor(input.ProjectID)
	if threshold <= 0 {
		return 0, nil
	}
	merged, err := r.github.ListMergedPullRequests(ctx, githubinfra.ListMergedPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: DefaultDiscoveryPullRequestLimit})
	if err != nil {
		return 0, fmt.Errorf("list merged pull requests for review backlog: %w", err)
	}
	count := 0
	reviewerLogin := ""
	loginLoaded := false
	for _, pullRequest := range merged {
		changedLines := maxInt(pullRequest.Additions, 0) + maxInt(pullRequest.Deletions, 0)
		if pullRequest.Number <= 0 || strings.TrimSpace(pullRequest.HeadSHA) == "" || threshold <= 0 || changedLines < threshold {
			continue
		}
		entityID := fmt.Sprintf("%s#%d", input.Repo, pullRequest.Number)
		events, err := r.repos.Events.ListByEntity(ctx, "pull_request", entityID)
		if err != nil {
			return count, fmt.Errorf("list review evidence for %s: %w", entityID, err)
		}
		completed, _, unreviewed := mergedReviewEvidenceForHead(events, pullRequest.HeadSHA)
		if completed {
			continue
		}
		// A negative backlog row is durable evidence that this merged PR was
		// observed without a review. It must not trigger a forge lookup forever.
		// A prior Reviewer refusal gets exactly one reconciliation opportunity;
		// a later successful Reviewer run appends completed evidence and remains
		// authoritative without requiring this historical scan to poll forever.
		if unreviewed {
			continue
		}
		if !loginLoaded {
			login, loginErr := r.github.GetCurrentUserLoginForRepo(ctx, input.Repo, input.CWD)
			if loginErr != nil || strings.TrimSpace(login) == "" {
				if loginErr == nil {
					loginErr = fmt.Errorf("empty reviewer login")
				}
				return count, fmt.Errorf("resolve reviewer identity for merged review backlog: %w", loginErr)
			}
			reviewerLogin = strings.TrimSpace(login)
			loginLoaded = true
		}
		marker, markerErr := r.github.FindReviewMarker(ctx, githubinfra.VerifyReviewMarkerInput{
			Repo: input.Repo, PRNumber: pullRequest.Number,
			Marker:              "looper:review id_prefix=reviewer: head=" + strings.TrimSpace(pullRequest.HeadSHA),
			AllowedReviewEvents: []string{"COMMENT", "APPROVE", "REQUEST_CHANGES"},
			AuthorLogin:         reviewerLogin, AllowCleanComment: true, SkipInlineComments: true, CWD: input.CWD,
		})
		if markerErr != nil {
			return count, fmt.Errorf("verify review evidence for %s: %w", entityID, markerErr)
		}
		if marker.Found {
			if err := r.appendMergedReviewEvidence(ctx, input, pullRequest, "pr.review.completed", map[string]any{
				"source": "gatekeeper-backlog-reconciliation", "outcome": marker.Outcome,
				"event": marker.Event, "reviewerLogin": marker.AuthorLogin, "reviewId": marker.ReviewID,
			}); err != nil {
				return count, err
			}
			continue
		}
		if err := r.appendMergedReviewEvidence(ctx, input, pullRequest, "pr.review.unreviewed", map[string]any{"threshold": threshold}); err != nil {
			return count, fmt.Errorf("record unreviewed merged pull request %s: %w", entityID, err)
		}
		count++
	}
	return count, nil
}

func (r *Runner) appendMergedReviewEvidence(ctx context.Context, input DiscoveryInput, pullRequest githubinfra.PullRequestSummary, eventType string, extra map[string]any) error {
	projectID := input.ProjectID
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", input.Repo, pullRequest.Number)
	payload := map[string]any{
		"repo": input.Repo, "prNumber": pullRequest.Number, "headSha": pullRequest.HeadSHA,
		"mergedAt": pullRequest.MergedAt, "additions": maxInt(pullRequest.Additions, 0),
		"deletions": maxInt(pullRequest.Deletions, 0), "changedLines": maxInt(pullRequest.Additions, 0) + maxInt(pullRequest.Deletions, 0),
	}
	for key, value := range extra {
		payload[key] = value
	}
	reviewID, _ := extra["reviewId"].(string)
	eventID := eventlog.StableReviewEvidenceID(eventType, input.Repo, pullRequest.Number, pullRequest.HeadSHA, reviewID, "")
	existing, err := r.repos.Events.ListByEntityAndEventTypes(ctx, "pull_request", entityID, []string{eventType})
	if err != nil {
		return err
	}
	for _, event := range existing {
		if event.ID == eventID {
			return nil
		}
	}
	err = eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		ID: eventID, EventType: eventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		ActorType: stringPtr("system"), ActorID: stringPtr("gatekeeper"), ActorDisplayName: stringPtr("gatekeeper"),
		Payload: payload, CreatedAt: r.now(),
	})
	if err == nil {
		return nil
	}
	// A concurrent discovery tick may have inserted the same deterministic row.
	// Re-read before surfacing the error so retries remain idempotent.
	if events, lookupErr := r.repos.Events.ListByEntityAndEventTypes(ctx, "pull_request", entityID, []string{eventType}); lookupErr == nil {
		for _, event := range events {
			if event.ID == eventID {
				return nil
			}
		}
	}
	return err
}

func mergedReviewEvidenceForHead(events []storage.EventLogRecord, headSHA string) (completed bool, refused bool, unreviewed bool) {
	for _, event := range events {
		if event.EventType != "pr.review.completed" && event.EventType != "pr.review.refused" && event.EventType != "pr.review.unreviewed" {
			continue
		}
		var payload struct {
			HeadSHA string `json:"headSha"`
		}
		if json.Unmarshal([]byte(event.PayloadJSON), &payload) != nil || !strings.EqualFold(strings.TrimSpace(payload.HeadSHA), strings.TrimSpace(headSHA)) {
			continue
		}
		switch event.EventType {
		case "pr.review.completed":
			completed = true
		case "pr.review.refused":
			refused = true
		case "pr.review.unreviewed":
			unreviewed = true
		}
	}
	return completed, refused, unreviewed
}

func maxInt(value, floor int) int {
	if value < floor {
		return floor
	}
	return value
}

func reviewCapacityStatsKnown(additions, deletions int, additionsKnown, deletionsKnown bool) bool {
	// Non-zero values preserve compatibility with hand-built gateway fakes and
	// older adapters that predate the explicit presence bits. Zero is only
	// trusted when the provider confirmed that the field was present.
	return (additionsKnown || additions != 0) && (deletionsKnown || deletions != 0)
}

func stringPtr(value string) *string { return &value }

// maxReconciledDepartures bounds how many departed pull requests one tick gives
// a final report. A burst — a release day that merges sixty pull requests, a
// project whose discovery scope was just narrowed — must not turn one tick into
// an unbounded run of forge reads. The remainder is picked up by later ticks,
// because a pull request that is still recorded OPEN and still absent from the
// open list stays a candidate until it is reconciled.
const maxReconciledDepartures = 20

// departedFromOpenSet names the pull requests whose latest report still says
// OPEN but which are no longer in the open list — they merged or were closed
// since the last tick.
//
// This is the lifecycle path that makes the merged state durable with webhooks
// off, which is the default. Polling discovery lists only open pull requests, so
// without this a pull request's last report is forever the one written while it
// was still open, and `state=merged` answers empty on a default install.
//
// It is not a poller over merged history, and three things bound it:
//
//   - Only pull requests Looper itself last recorded as OPEN are candidates.
//     Anything that merged before this ran, or that Looper never saw, is never
//     revisited — there is no backfill.
//   - The transition is one-way and self-clearing. Reconciling rewrites the
//     state to MERGED or CLOSED, so the same pull request is never a candidate
//     twice and the steady-state count is zero.
//   - A truncated open list is not evidence of departure. When the list came
//     back at the limit, a pull request may simply have fallen off the page, so
//     nothing is reconciled at all rather than churning the overflow every tick.
func (r *Runner) departedFromOpenSet(
	repo string,
	pullRequests []githubinfra.PullRequestSummary,
	limit int,
	previousReports map[string]Report,
	stillOpen map[string]struct{},
) []string {
	if len(pullRequests) >= limit {
		return nil
	}
	departed := make([]string, 0)
	for entityID, previous := range previousReports {
		if previous.Repo != repo || previous.PRNumber <= 0 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(previous.Evidence.PullRequestState), "OPEN") {
			continue
		}
		if _, open := stillOpen[entityID]; open {
			continue
		}
		departed = append(departed, entityID)
	}
	// Sorted so the cap takes a deterministic prefix rather than whichever
	// entries Go's map iteration happened to yield first.
	sort.Slice(departed, func(i, j int) bool {
		return previousReports[departed[i]].PRNumber < previousReports[departed[j]].PRNumber
	})
	if len(departed) > maxReconciledDepartures {
		departed = departed[:maxReconciledDepartures]
	}
	return departed
}

func (r *Runner) EvaluatePullRequest(ctx context.Context, input EvaluationInput) (Report, error) {
	if r == nil || r.github == nil {
		return Report{}, fmt.Errorf("gatekeeper GitHub gateway is not configured")
	}
	if r.repos == nil || r.repos.Events == nil {
		return Report{}, fmt.Errorf("gatekeeper event repository is not configured")
	}
	// The project ID is matched by exact equality against configured IDs, which
	// validation accepts with surrounding whitespace. Trimming it here would
	// diverge from the discovery path (which fingerprints the project override
	// using the caller's original key), so the report would fall back to the
	// global budget and default trust while the fingerprint claimed the override.
	// Only the emptiness check is trimmed; the original key is preserved.
	input.Repo = strings.TrimSpace(input.Repo)
	input.ExpectedHeadSHA = strings.TrimSpace(input.ExpectedHeadSHA)
	input.ExpectedBaseSHA = strings.TrimSpace(input.ExpectedBaseSHA)
	diffBudget := r.diffBudget(input.ProjectID)
	budgetEnabled := diffBudget.MaxChangedFiles > 0 || diffBudget.MaxDeletions > 0
	if strings.TrimSpace(input.ProjectID) == "" || input.Repo == "" || input.PRNumber <= 0 {
		return Report{}, fmt.Errorf("gatekeeper project, repository, and pull request number are required")
	}
	if strings.TrimSpace(input.CWD) == "" {
		input.CWD = r.projectCWD(ctx, input.ProjectID)
	}

	if input.Confirming {
		ctx = withConfirming(ctx)
	}

	report := Report{
		Version: reportVersion, Status: StatusBlocked,
		ProjectID: input.ProjectID, Repo: input.Repo, PRNumber: input.PRNumber,
		ExpectedHeadSHA: input.ExpectedHeadSHA, RequiresFreshRevalidation: true,
		Reasons: []Reason{}, Evidence: Evidence{
			RequiredChecks: []string{}, Checks: []CheckEvidence{}, UnresolvedReviewThreadIDs: []string{}, HoldLabels: []string{},
			ReviewProvenance: ReviewProvenance{Reviewers: []ReviewerObservation{}, Refusals: []ReviewRefusal{}},
		},
		EvaluatedAt:       r.now().UTC().Format(time.RFC3339Nano),
		SourceFingerprint: input.SourceFingerprint,
	}
	viewInput := githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD, ProtectedPaths: r.protectedPaths(input.ProjectID)}
	detail, err := r.github.ViewPullRequestForGatekeeper(ctx, viewInput)
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "pull_request")
	}
	report.ObservedHeadSHA = strings.TrimSpace(detail.HeadSHA)
	report.Evidence.PullRequestState = strings.ToUpper(strings.TrimSpace(detail.State))
	report.Evidence.ClosedAt = strings.TrimSpace(detail.ClosedAt)
	report.Evidence.MergedAt = strings.TrimSpace(detail.MergedAt)
	report.Evidence.Draft = detail.IsDraft
	report.Evidence.BaseRefName = strings.TrimSpace(detail.BaseRefName)
	report.Evidence.BaseSHA = strings.TrimSpace(detail.BaseSHA)
	report.Evidence.ReviewDecision = strings.ToUpper(strings.TrimSpace(detail.ReviewDecision))
	reviewThreshold := r.requiredReviewChangedLinesFor(input.ProjectID)
	reviewPolicyEnabled := r.trustFor(input.ProjectID) == config.GatekeeperTrustAuto && reviewThreshold > 0
	report.Evidence.Additions = maxInt(detail.Additions, 0)
	report.Evidence.Deletions = maxInt(detail.Deletions, 0)
	report.Evidence.ChangedLines = report.Evidence.Additions + report.Evidence.Deletions
	report.Evidence.ReviewRequiredByPolicy = reviewPolicyEnabled && report.Evidence.ChangedLines >= reviewThreshold
	if input.ExpectedHeadSHA != "" && report.ObservedHeadSHA != input.ExpectedHeadSHA {
		report.Reasons = []Reason{{Code: ReasonHeadStale}}
		return r.persist(ctx, report)
	}
	if input.ExpectedBaseSHA != "" && report.Evidence.BaseSHA != input.ExpectedBaseSHA {
		report.Reasons = []Reason{{Code: ReasonBaseStale}}
		return r.persist(ctx, report)
	}
	if detail.Number != input.PRNumber || report.ObservedHeadSHA == "" || report.Evidence.BaseRefName == "" {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "pull_request")
	}
	codexReview, err := latestCodexReviewForHead(ctx, r.repos, input.ProjectID, input.Repo, input.PRNumber, report.ObservedHeadSHA)
	if err != nil {
		// Persist the invalid projection so reportAwaitsCurrentHeadReview
		// recognizes this report as waiting on review evidence. Without it the
		// report carries only the provider reason and a nil CodexReview
		// projection, which neither signal detects, so unchanged discovery
		// would reuse this transient read failure for up to maxSkipAge even
		// after the event store recovers or the review event appears. The
		// placeholder carries RequiredHeadSHA and CurrentHeadValid=false.
		report.Evidence.CodexReview = &codexReview
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "codex_review")
	}
	report.Evidence.CodexReview = &codexReview
	report.Evidence.CodexReviewOutcome = strings.ToLower(strings.TrimSpace(codexReview.Outcome))
	if !codexReview.CurrentHeadValid {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonCodexReviewMissing, Subject: codexReviewReasonSubject(codexReview)})
	}
	if codexReview.CurrentHeadValid && report.Evidence.CodexReviewOutcome == "blocking" {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonCodexReviewBlocked, Subject: report.Evidence.CodexReviewOutcome})
	}
	if r.trustFor(input.ProjectID) == config.GatekeeperTrustAuto && !report.Evidence.ReviewRequiredByPolicy {
		// Small changes and an explicit zero threshold waive only the clean-review
		// requirement; an explicit blocking marker remains authoritative below.
		report.Reasons = withoutReasonCodes(report.Reasons, ReasonCodexReviewMissing, ReasonCodexReviewRequired)
	}

	// Recorded here, before every gate branch and every early return below,
	// because provenance is an observation about the pull request rather than a
	// step in the eligibility decision — and because the reports that matter most
	// never reach the review branch at all. A merged pull request answers the
	// mergeability read ambiguously, and that returns long before the review
	// branch — yet the closed-webhook evaluation that follows a merge is exactly
	// the report an operator later reads to ask whether the merged change was
	// ever reviewed.
	report.Evidence.ReviewProvenance = r.observeReviewProvenance(ctx, input)

	if protectedPaths := matchedProtectedPaths(detail.ChangedFiles, r.protectedPaths(input.ProjectID)); len(protectedPaths) > 0 {
		report.Evidence.ProtectedPaths = protectedPaths
		report.Reasons = append(report.Reasons, Reason{Code: ReasonProtectedPathTouched, Subject: protectedPathReasonSubject(protectedPaths)})
	}

	if report.Evidence.PullRequestState != "OPEN" {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonPullRequestNotOpen})
	}
	if detail.IsDraft {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonPullRequestDraft})
	}
	// Normalize the effective project namespace before evaluating the human
	// veto. A custom namespace's hold is authoritative for that project; using
	// the global default here would let an explicitly held PR appear eligible.
	holdLabel := r.projectLabelNamespace(ctx, input.ProjectID).HoldGlobal()
	if labels.Has(detail.Labels, holdLabel) {
		report.Evidence.HoldLabels = append(report.Evidence.HoldLabels, holdLabel)
		report.Reasons = append(report.Reasons, Reason{Code: ReasonHold, Subject: holdLabel})
	}
	if labels.Has(detail.Labels, labels.DoNotMerge) {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonDoNotMerge, Subject: labels.DoNotMerge})
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
		report.Evidence.FinalObservedBaseSHA = strings.TrimSpace(mergeability.BaseSHA)
		return r.persist(ctx, report)
	}
	report.Evidence.Mergeable = mergeability.Mergeable
	mergeabilityState := mergeability.MergeableState
	report.Evidence.MergeableState = mergeabilityState.Raw()
	if mergeability.Mergeable == nil || mergeabilityState.IsUnknown() {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "mergeability")
	}
	if !*mergeability.Mergeable || mergeabilityState.HasConflict() {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonMergeConflict})
	} else if mergeabilityState.IsAmbiguousPolicyState() {
		// GitHub reports "blocked" while required statuses (including this
		// runner's own) are absent or pending. Treating that as a gate input in
		// auto mode would publish error and keep mergeability blocked forever.
		if r.trustFor(input.ProjectID) != config.GatekeeperTrustAuto || mergeabilityState != githubinfra.MergeabilityStateBlocked {
			report.Reasons = append(report.Reasons, Reason{Code: ReasonProviderStateAmbiguous, Subject: "mergeability:" + mergeabilityState.Raw()})
		}
	} else if !mergeabilityState.IsClean() {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonMergeabilityNotClean, Subject: mergeabilityState.Raw()})
	}

	// Review-capacity counts are merge-base dependent just like the optional
	// diff budget. Use the merge-watch observation when its base differs from
	// the initial detail read, then revalidate that base before publishing.
	reviewCapacityBaseSHA := ""
	reviewCapacityBaseMissing := false
	if reviewPolicyEnabled {
		reviewCapacityBaseSHA = strings.TrimSpace(detail.BaseSHA)
		additions, deletions := detail.Additions, detail.Deletions
		statsKnown := reviewCapacityStatsKnown(detail.Additions, detail.Deletions, detail.AdditionsKnown, detail.DeletionsKnown)
		if strings.TrimSpace(mergeability.BaseSHA) != strings.TrimSpace(detail.BaseSHA) {
			reviewCapacityBaseSHA = strings.TrimSpace(mergeability.BaseSHA)
			additions, deletions = mergeability.Additions, mergeability.Deletions
			statsKnown = reviewCapacityStatsKnown(mergeability.Additions, mergeability.Deletions, mergeability.AdditionsKnown, mergeability.DeletionsKnown)
		}
		if reviewCapacityBaseSHA == "" {
			reviewCapacityBaseMissing = true
		} else if !statsKnown {
			// A zero count is ambiguous when the provider omitted the field: it
			// may describe a genuinely empty diff, or a large change whose
			// additions/deletions were not decoded. Auto trust must not waive a
			// current-head review on that uncertainty.
			return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "review_capacity_stats")
		}
		if additions < 0 {
			additions = 0
		}
		if deletions < 0 {
			deletions = 0
		}
		report.Evidence.Additions = additions
		report.Evidence.Deletions = deletions
		report.Evidence.ChangedLines = additions + deletions
		report.Evidence.ReviewRequiredByPolicy = report.Evidence.ChangedLines >= reviewThreshold
		if !report.Evidence.ReviewRequiredByPolicy {
			// The capacity policy waives the requirement for a review on small
			// changes. Blocking provider-authored review markers are still checked
			// below, so a waiver cannot turn an explicit veto into success.
			report.Reasons = withoutReasonCodes(report.Reasons, ReasonCodexReviewMissing, ReasonCodexReviewRequired)
		}
		if report.Evidence.ReviewRequiredByPolicy && !codexReview.CurrentHeadValid && !hasReasonCode(report.Reasons, ReasonCodexReviewMissing) {
			report.Reasons = append(report.Reasons, Reason{Code: ReasonCodexReviewMissing, Subject: codexReviewReasonSubject(codexReview)})
		}
	}

	// The diff statistics were observed against detail.BaseSHA. If the base
	// branch advanced between the detail read and the merge-watch read, GitHub
	// recomputes changedFiles and deletions against a new merge base without
	// moving the head, so the original counts no longer describe the diff this
	// merge would produce. Revalidate against the merge-watch's current base
	// before accepting the budget verdict; fail closed when those stats are
	// unavailable. diffBudgetBaseSHA records the base the accepted counts were
	// observed against so the final revalidation can detect a later advance.
	diffBudgetBaseSHA := ""
	if budgetEnabled {
		stats := detail.DiffStats
		diffBudgetBaseSHA = strings.TrimSpace(detail.BaseSHA)
		if strings.TrimSpace(mergeability.BaseSHA) != strings.TrimSpace(detail.BaseSHA) {
			stats = mergeability.DiffStats
			diffBudgetBaseSHA = strings.TrimSpace(mergeability.BaseSHA)
		}
		if stats == nil {
			return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "diff_stats")
		}
		// The accepted counts are only meaningful against the base they were
		// observed with. A provider that returns diff statistics but omits the
		// base SHA leaves no way to establish which merge base produced them, so
		// the final revalidation below could not detect a later advance. Fail
		// closed rather than recording a verdict the gate cannot anchor.
		if diffBudgetBaseSHA == "" {
			return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "diff_budget_base")
		}
		report.Evidence.DiffBudget = &DiffBudgetEvidence{
			ChangedFiles:    stats.ChangedFiles,
			Deletions:       stats.Deletions,
			MaxChangedFiles: diffBudget.MaxChangedFiles,
			MaxDeletions:    diffBudget.MaxDeletions,
		}
		if subject := diffBudgetExceededSubject(*stats, diffBudget); subject != "" {
			report.Reasons = append(report.Reasons, Reason{Code: ReasonDiffBudgetExceeded, Subject: subject})
		}
	}
	if reviewCapacityBaseMissing {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "review_capacity_base")
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
	if checks.TotalCount > len(checks.CheckRuns) {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "checks_truncated")
	}
	requiredCheckRules := protection.RequiredCheckRules
	if len(requiredCheckRules) == 0 {
		requiredCheckRules = make([]githubinfra.RequiredCheckRule, 0, len(report.Evidence.RequiredChecks))
		for _, name := range report.Evidence.RequiredChecks {
			requiredCheckRules = append(requiredCheckRules, githubinfra.RequiredCheckRule{Context: name})
		}
	}
	if r.trustFor(input.ProjectID) == config.GatekeeperTrustAuto {
		if !containsRequiredCheck(report.Evidence.RequiredChecks, RequiredStatusContext) {
			report.Reasons = append(report.Reasons, Reason{Code: ReasonGatekeeperCheckRequired, Subject: RequiredStatusContext})
		}
		for _, rule := range requiredCheckRules {
			if strings.EqualFold(strings.TrimSpace(rule.Context), RequiredStatusContext) && rule.AppID != 0 {
				report.Reasons = append(report.Reasons, Reason{Code: ReasonGatekeeperCheckRequired, Subject: requiredCheckSubject(rule.Context, rule.AppID)})
			}
		}
		// The Gatekeeper status is this evaluation's output, not an input. Reading
		// its prior value would turn the first run on a new SHA into a permanent
		// self-failure instead of letting this run publish the replacement. Only
		// strip the unscoped name match; app-bound rules remain so misconfigured
		// protection fails closed instead of being silently ignored.
		requiredCheckRules = withoutRequiredCheck(requiredCheckRules, RequiredStatusContext)
	}
	checkReasons, checkEvidence := evaluateRequiredChecks(requiredCheckRules, checks)
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

	convergenceEvidence, hasConvergence, err := latestReviewerConvergence(ctx, r.repos, input.ProjectID, input.Repo, input.PRNumber, report.ObservedHeadSHA)
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "reviewer_convergence")
	}
	if hasConvergence {
		report.Evidence.ReviewerConvergence = &convergenceEvidence
		if reviewerConvergenceBlocks(convergenceEvidence) {
			report.Reasons = append(report.Reasons, Reason{Code: ReasonReviewerConvergence, Subject: reviewerConvergenceReasonSubject(convergenceEvidence)})
		}
	}

	if r.trustFor(input.ProjectID) == config.GatekeeperTrustAuto {
		if !reviewPolicyEnabled {
			// An explicit zero disables the clean-review capacity requirement. A
			// blocking marker remains authoritative and is still checked below.
			report.Reasons = withoutReasonCodes(report.Reasons, ReasonCodexReviewMissing, ReasonCodexReviewRequired)
		}
		login, loginErr := r.github.GetCurrentUserLoginForRepo(ctx, input.Repo, input.CWD)
		if loginErr != nil || strings.TrimSpace(login) == "" {
			return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "codex_reviewer_identity")
		}
		marker, markerErr := r.github.FindReviewMarker(ctx, githubinfra.VerifyReviewMarkerInput{
			Repo: input.Repo, PRNumber: input.PRNumber,
			Marker:              "looper:review id_prefix=reviewer: head=" + report.ObservedHeadSHA,
			AllowedReviewEvents: []string{"COMMENT", "APPROVE", "REQUEST_CHANGES"},
			AuthorLogin:         login, AllowCleanComment: true, SkipInlineComments: true, CWD: input.CWD,
		})
		if markerErr != nil {
			return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "codex_review")
		}
		if marker.Found {
			report.Reasons = withoutReasonCodes(report.Reasons, ReasonCodexReviewMissing)
			report.Evidence.CodexReviewOutcome = strings.ToLower(strings.TrimSpace(marker.Outcome))
			if report.Evidence.CodexReviewOutcome == "blocking" {
				report.Reasons = append(report.Reasons, Reason{Code: ReasonCodexReviewBlocked, Subject: report.Evidence.CodexReviewOutcome})
			}
		} else if report.Evidence.CodexReview != nil && report.Evidence.CodexReview.CurrentHeadValid && report.Evidence.CodexReview.Event == "COMMENT" && strings.EqualFold(strings.TrimSpace(report.Evidence.CodexReview.Outcome), "clean") {
			// Markerless clean COMMENT policy records pr.review.posted with
			// markerVerified=true and outcome=clean, but leaves no forge marker
			// for FindReviewMarker. An empty or blocking outcome must fail closed.
			report.Evidence.CodexReviewOutcome = "clean"
		} else if report.Evidence.ReviewRequiredByPolicy && !strings.EqualFold(strings.TrimSpace(report.Evidence.CodexReview.Outcome), "blocking") {
			report.Reasons = append(report.Reasons, Reason{Code: ReasonCodexReviewRequired, Subject: "current_head"})
		}
	}

	finalHead, finalBase, err := r.github.GetPullRequestHeadAndBaseSHA(ctx, viewInput)
	if err != nil {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateUnavailable, "head_revalidation")
	}
	report.Evidence.FinalObservedHeadSHA = strings.TrimSpace(finalHead)
	report.Evidence.FinalObservedBaseSHA = strings.TrimSpace(finalBase)
	if report.Evidence.FinalObservedHeadSHA == "" {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "head_revalidation")
	}
	if report.Evidence.FinalObservedBaseSHA == "" {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "base_revalidation")
	}
	if report.Evidence.FinalObservedHeadSHA != report.ObservedHeadSHA {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonHeadStale})
	}
	// The diff-budget verdict was captured against diffBudgetBaseSHA at the
	// merge-watch read. The base branch can advance between that read and this
	// final observation without moving the head, so the head revalidation above
	// cannot catch it: GitHub would recompute the change size against a new
	// merge base while the recorded counts still describe the previous one, and
	// an eligible verdict would proceed toward auto-merge on stale evidence. Fail
	// closed so the next tick (whose fingerprint folds in BaseSHA while the budget
	// is enabled) re-evaluates against the current base. diffBudgetBaseSHA is
	// guaranteed non-empty here because the budget block above fails closed when
	// the provider omits it.
	if budgetEnabled && strings.TrimSpace(finalBase) != diffBudgetBaseSHA {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "diff_budget_base")
	}
	if reviewPolicyEnabled && strings.TrimSpace(finalBase) != reviewCapacityBaseSHA {
		return r.persistProviderBlock(ctx, report, ReasonProviderStateAmbiguous, "review_capacity_base")
	}
	if !budgetEnabled && len(r.protectedPaths(input.ProjectID)) > 0 && report.Evidence.FinalObservedBaseSHA != report.Evidence.BaseSHA {
		report.Reasons = append(report.Reasons, Reason{Code: ReasonBaseStale})
	}
	persisted, err := r.persist(ctx, report)
	if err != nil {
		return Report{}, err
	}
	// Merging is the last thing, and only on the primary pass: the confirming
	// evaluation exists to serve this decision, not to make another one.
	if !input.Confirming && persisted.Eligible && r.trustFor(persisted.ProjectID) == config.GatekeeperTrustAuto {
		confirmed, err := r.confirmAndMerge(ctx, input, persisted)
		if err != nil {
			return persisted, err
		}
		return confirmed, nil
	}
	return persisted, nil
}

// observeReviewProvenance reads who reviewed this pull request and who refused
// to.
//
// It never returns an error, and that is deliberate rather than lax: Gatekeeper
// is observe-only, so an observation must not be able to move a pull request
// from eligible to blocked. A forge that will not answer yields
// ReviewProvenanceUnknown, which the enumeration excludes — the record says it
// does not know instead of saying nobody reviewed.
//
// It costs two forge reads per full evaluation. The review list is REST because
// only REST classifies the reviewer's account, and the comment list is where a
// refusal lives, because a refusal is not a review and the forge files it
// nowhere else. Both are skipped entirely for a pull request whose fingerprint
// is unchanged.
//
// The two reads fail differently, because they answer different questions. The
// review list is the whole of `reviewed`; comments only separate `refused` from
// `absent`, and only when nothing was reviewed. So a comment read that fails
// after the reviews came back keeps what those reviews already settled —
// discarding a list of named reviewers because a later call timed out would
// throw away evidence Looper is holding.
func (r *Runner) observeReviewProvenance(ctx context.Context, input EvaluationInput) ReviewProvenance {
	unknown := ReviewProvenance{Status: ReviewProvenanceUnknown, Reviewers: []ReviewerObservation{}, Refusals: []ReviewRefusal{}}
	reviews, err := r.github.ListPullRequestReviews(ctx, githubinfra.ViewPullRequestInput{
		Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD,
	})
	if err != nil {
		r.warnReviewProvenance(input, "reviews", err)
		return unknown
	}
	observed := reviewsObservedFrom(reviews)
	comments, err := r.github.ListIssueCommentsContaining(ctx, githubinfra.ViewIssueInput{
		Repo: input.Repo, IssueNumber: input.PRNumber, CWD: input.CWD,
	}, RefusalCommentMarkers())
	if err != nil {
		r.warnReviewProvenance(input, "comments", err)
		if observed.CompletedReviews > 0 {
			return observed
		}
		return unknown
	}
	return withRefusalsFrom(observed, comments)
}

func (r *Runner) warnReviewProvenance(input EvaluationInput, subject string, err error) {
	if r.logWarn == nil {
		return
	}
	r.logWarn("gatekeeper: could not observe review provenance", map[string]any{
		"repo": input.Repo, "pr": input.PRNumber, "subject": subject, "error": err.Error(),
	})
}

func (r *Runner) projectCWD(ctx context.Context, projectID string) string {
	if r == nil || r.repos == nil || r.repos.Projects == nil || strings.TrimSpace(projectID) == "" {
		return ""
	}
	// Project IDs are matched by exact equality against configured IDs, which
	// validation accepts with surrounding whitespace. Trimming here would diverge
	// from the discovery and evaluation paths (which preserve the caller's
	// original key), so a whitespace-padded configured ID would no longer resolve
	// to its repo path. On GitHub Enterprise that empty CWD strands
	// ListReviewThreads on the wrong provider hostname. Only the emptiness check
	// above is trimmed; the lookup uses the original key.
	project, err := r.repos.Projects.GetByID(ctx, projectID)
	if err != nil || project == nil {
		return ""
	}
	return project.RepoPath
}

func (r *Runner) projectLabelNamespace(ctx context.Context, projectID string) labels.Namespace {
	if r != nil && r.labelNamespaceForProject != nil {
		if namespace, configured := r.labelNamespaceForProject(strings.TrimSpace(projectID)); configured {
			return namespace
		}
	}
	if r != nil && r.repos != nil && r.repos.Projects != nil {
		project, err := r.repos.Projects.GetByID(ctx, strings.TrimSpace(projectID))
		if err == nil && project != nil {
			return config.ProjectLabelNamespaceForMetadata(nil, project.ID, project.MetadataJSON)
		}
	}
	return labels.DefaultNamespace()
}
func (r *Runner) persistProviderBlock(ctx context.Context, report Report, code ReasonCode, subject string) (Report, error) {
	report.Reasons = []Reason{{Code: code, Subject: subject}}
	return r.persist(ctx, report)
}

// trustFor reports a project's configured merge-authority level, defaulting to
// observe when nothing is configured.
func (r *Runner) trustFor(projectID string) config.GatekeeperTrustLevel {
	if r == nil || r.trustForProject == nil {
		return config.GatekeeperTrustObserve
	}
	level := config.GatekeeperTrustLevel(strings.ToLower(strings.TrimSpace(string(r.trustForProject(projectID)))))
	switch level {
	case config.GatekeeperTrustAdvise, config.GatekeeperTrustAuto:
		return level
	default:
		return config.GatekeeperTrustObserve
	}
}

func (r *Runner) diffBudget(projectID string) config.GatekeeperDiffBudget {
	if r == nil || r.diffBudgetForProject == nil {
		return config.GatekeeperDiffBudget{}
	}
	return r.diffBudgetForProject(projectID)
}

func (r *Runner) requiredReviewChangedLinesFor(projectID string) int {
	if r == nil || r.requiredReviewChangedLinesForProject == nil {
		return DefaultRequiredReviewChangedLines
	}
	threshold := r.requiredReviewChangedLinesForProject(projectID)
	if threshold < 0 {
		return DefaultRequiredReviewChangedLines
	}
	return threshold
}

func (r *Runner) configuredTarget(projectID string) string {
	if r == nil || r.configuredTargetBranch == nil {
		return ""
	}
	return strings.TrimSpace(r.configuredTargetBranch(projectID))
}

func diffBudgetExceededSubject(stats githubinfra.PullRequestDiffStats, budget config.GatekeeperDiffBudget) string {
	parts := make([]string, 0, 2)
	if budget.MaxChangedFiles > 0 && stats.ChangedFiles > budget.MaxChangedFiles {
		parts = append(parts, fmt.Sprintf("changed files %d > max %d", stats.ChangedFiles, budget.MaxChangedFiles))
	}
	if budget.MaxDeletions > 0 && stats.Deletions > budget.MaxDeletions {
		parts = append(parts, fmt.Sprintf("deletions %d > max %d", stats.Deletions, budget.MaxDeletions))
	}
	return strings.Join(parts, "; ")
}

func (r *Runner) protectedPaths(projectID string) []string {
	if r == nil || r.protectedPathsForProject == nil {
		return nil
	}
	return append([]string(nil), r.protectedPathsForProject(projectID)...)
}

// confirmingKey marks an evaluation as the pass performed immediately before a
// merge. It travels on the context because it is a property of the whole
// evaluation rather than of any one step, and every persist path would otherwise
// need the same extra parameter.
type confirmingKey struct{}

func withConfirming(ctx context.Context) context.Context {
	return context.WithValue(ctx, confirmingKey{}, true)
}

func isConfirming(ctx context.Context) bool {
	confirming, _ := ctx.Value(confirmingKey{}).(bool)
	return confirming
}

type deferCommitStatusKey struct{}

func withDeferCommitStatus(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferCommitStatusKey{}, true)
}

func deferCommitStatus(ctx context.Context) bool {
	deferred, _ := ctx.Value(deferCommitStatusKey{}).(bool)
	return deferred
}

func reportCommitSHA(report Report) string {
	sha := strings.TrimSpace(report.ObservedHeadSHA)
	if sha == "" {
		sha = strings.TrimSpace(report.ExpectedHeadSHA)
	}
	return sha
}

func reportIsOpenForStatusAggregation(report Report) bool {
	state := strings.ToUpper(strings.TrimSpace(report.Evidence.PullRequestState))
	return state == "" || state == "OPEN"
}

func commitStatusSeverity(state string) int {
	switch state {
	case "failure":
		return 3
	case "error":
		return 2
	case "pending":
		return 1
	default:
		return 0
	}
}

func aggregatedCommitStatus(reports []Report) (string, string) {
	if len(reports) == 0 {
		return "error", "Gatekeeper could not verify current merge state"
	}
	for _, report := range reports {
		if hasReasonCode(report.Reasons, ReasonGatekeeperCheckRequired) {
			return "error", "Looper Gatekeeper is not configured as a required status on the target branch"
		}
	}
	blocked := make([]Report, 0, len(reports))
	for _, report := range reports {
		if !report.Eligible {
			blocked = append(blocked, report)
		}
	}
	if len(blocked) > 0 {
		state, description := gatekeeperCommitStatus(blocked[0])
		for _, report := range blocked[1:] {
			nextState, nextDescription := gatekeeperCommitStatus(report)
			if commitStatusSeverity(nextState) > commitStatusSeverity(state) {
				state, description = nextState, nextDescription
			}
		}
		if len(blocked) < len(reports) {
			description = "Another open pull request on this commit failed Gatekeeper"
		}
		return state, description
	}
	return gatekeeperCommitStatus(reports[0])
}

func (r *Runner) publishDiscoveryCommitStatuses(ctx context.Context, projectID, repo, cwd string, reports []Report) {
	_ = r.publishDiscoveryCommitStatusesE(ctx, projectID, repo, cwd, reports)
}

func (r *Runner) publishDiscoveryCommitStatusesE(ctx context.Context, projectID, repo, cwd string, reports []Report) error {
	bySHA := make(map[string][]Report)
	for _, report := range reports {
		if !reportIsOpenForStatusAggregation(report) {
			continue
		}
		sha := reportCommitSHA(report)
		if sha == "" {
			continue
		}
		bySHA[sha] = append(bySHA[sha], report)
	}
	var firstErr error
	for sha, group := range bySHA {
		state, description := aggregatedCommitStatus(group)
		if err := r.setCommitStatusIfChanged(ctx, repo, cwd, sha, RequiredStatusContext, state, description); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if r.logWarn != nil {
				r.logWarn("gatekeeper: could not publish aggregated commit status", map[string]any{
					"repo": repo, "sha": sha, "error": err.Error(),
				})
			}
			for _, report := range group {
				if strings.TrimSpace(report.SourceFingerprint) == "" {
					continue
				}
				if err := r.clearReportCacheability(ctx, report); err != nil && r.logWarn != nil {
					r.logWarn("gatekeeper: could not mark report non-cacheable after status failure", map[string]any{
						"repo": report.Repo, "pr": report.PRNumber, "error": err.Error(),
					})
				}
			}
		}
	}
	return firstErr
}

func (r *Runner) tryPublishAggregatedCommitStatus(ctx context.Context, report Report) bool {
	if r.trustFor(report.ProjectID) != config.GatekeeperTrustAuto {
		return true
	}
	sha := reportCommitSHA(report)
	if sha == "" {
		return true
	}
	group, err := r.openReportsForHeadSHA(ctx, report.ProjectID, report.Repo, sha, report)
	if err != nil {
		if r.logWarn != nil {
			r.logWarn("gatekeeper: could not load reports for status aggregation", map[string]any{
				"repo": report.Repo, "sha": sha, "error": err.Error(),
			})
		}
		group = []Report{report}
	}
	state, description := aggregatedCommitStatus(group)
	if err := r.setCommitStatusIfChanged(ctx, report.Repo, r.projectCWD(ctx, report.ProjectID), sha, RequiredStatusContext, state, description); err != nil {
		if r.logWarn != nil {
			r.logWarn("gatekeeper: could not publish aggregated commit status", map[string]any{
				"repo": report.Repo, "sha": sha, "error": err.Error(),
			})
		}
		return false
	}
	return true
}

// publishConfirmingCommitStatus publishes the status for the complete set of
// open pull requests sharing report's head. A confirming evaluation is about
// to cross the forge merge boundary, so its status must be visible first; the
// report itself is only one member of the shared-head status authority.
func (r *Runner) publishConfirmingCommitStatus(ctx context.Context, report Report) error {
	sha := reportCommitSHA(report)
	if sha == "" {
		return nil
	}
	group, err := r.openReportsForHeadSHA(ctx, report.ProjectID, report.Repo, sha, report)
	if err != nil {
		return fmt.Errorf("load reports for confirming status: %w", err)
	}
	state, description := aggregatedCommitStatus(group)
	if err := r.setCommitStatusIfChanged(ctx, report.Repo, r.projectCWD(ctx, report.ProjectID), sha, RequiredStatusContext, state, description); err != nil {
		for _, groupReport := range group {
			if strings.TrimSpace(groupReport.SourceFingerprint) == "" {
				continue
			}
			if clearErr := r.clearReportCacheability(ctx, groupReport); clearErr != nil && r.logWarn != nil {
				r.logWarn("gatekeeper: could not mark confirming report non-cacheable after status failure", map[string]any{
					"repo": groupReport.Repo, "pr": groupReport.PRNumber, "error": clearErr.Error(),
				})
			}
		}
		return err
	}
	return nil
}

func (r *Runner) openReportsForHeadSHA(ctx context.Context, projectID, repo, sha string, current Report) ([]Report, error) {
	previousReports, err := latestGateReports(ctx, r.repos, projectID)
	if err != nil {
		return nil, err
	}
	currentEntityID := fmt.Sprintf("%s#%d", current.Repo, current.PRNumber)
	group := make([]Report, 0)
	seenCurrent := false
	for entityID, report := range previousReports {
		if report.Repo != repo || !reportIsOpenForStatusAggregation(report) || reportCommitSHA(report) != sha {
			continue
		}
		if entityID == currentEntityID {
			report = current
			seenCurrent = true
		}
		group = append(group, report)
	}
	if !seenCurrent && reportIsOpenForStatusAggregation(current) {
		group = append(group, current)
	}
	return group, nil
}

func (r *Runner) setCommitStatusIfChanged(ctx context.Context, repo, cwd, sha, contextName, state, description string) error {
	signature := state + "\x1f" + description
	key := repo + "\x1f" + sha + "\x1f" + contextName
	r.lastPublishedStatusMu.Lock()
	if r.lastPublishedStatus == nil {
		r.lastPublishedStatus = make(map[string]string)
	}
	if r.lastPublishedStatus[key] == signature {
		r.lastPublishedStatusMu.Unlock()
		return nil
	}
	r.lastPublishedStatusMu.Unlock()

	if err := r.github.SetCommitStatus(ctx, githubinfra.CommitStatusInput{
		Repo: repo, SHA: sha, Context: contextName,
		State: state, Description: description, CWD: cwd,
	}); err != nil {
		r.lastPublishedStatusMu.Lock()
		delete(r.lastPublishedStatus, key)
		r.lastPublishedStatusMu.Unlock()
		return err
	}
	r.lastPublishedStatusMu.Lock()
	r.lastPublishedStatus[key] = signature
	r.lastPublishedStatusMu.Unlock()
	return nil
}

func (r *Runner) clearReportCacheability(ctx context.Context, report Report) error {
	report.SourceFingerprint = ""
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", report.Repo, report.PRNumber)
	projectID := report.ProjectID
	return eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: GateReportEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID,
		Payload: report, CreatedAt: r.now(),
	})
}

// persist records a report. The confirming pass writes its report — the evidence
// behind a merge has to be durable — but publishes no verdict, because it
// describes the same head as the verdict already on the pull request.
func (r *Runner) persist(ctx context.Context, report Report) (Report, error) {
	confirming := isConfirming(ctx)
	sortReasons(report.Reasons)
	report.Eligible = len(report.Reasons) == 0
	if report.Eligible {
		report.Status = StatusEligible
	} else {
		report.Status = StatusBlocked
	}
	// Mode is stamped here rather than by the caller: it is the input to the
	// comment lifecycle below, so a path that built a report without it would
	// silently strand an owned comment.
	report.Mode = string(r.trustFor(report.ProjectID))

	// Decide what the owned comment needs before this report replaces the previous
	// one in the log, and while no forge call has been made.
	action := verdictActionNone
	previous, err := r.latestReport(ctx, report.Repo, report.PRNumber)
	if err != nil {
		// Not knowing the previous state means not knowing whether an owned comment
		// is stranded, so fail rather than guess.
		return Report{}, err
	}
	if !confirming {
		action = decideVerdictAction(r.trustFor(report.ProjectID), previous, report)
	}

	statusSHA := strings.TrimSpace(report.ObservedHeadSHA)
	if statusSHA == "" {
		statusSHA = strings.TrimSpace(report.ExpectedHeadSHA)
	}
	if !confirming && r.trustFor(report.ProjectID) == config.GatekeeperTrustAuto && statusSHA != "" && !deferCommitStatus(ctx) {
		if !r.tryPublishAggregatedCommitStatus(ctx, report) {
			// A failed publication must not be cacheable: the next tick must
			// re-evaluate and retry rather than reuse a report while branch
			// protection still sees a stale success.
			report.SourceFingerprint = ""
		}
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
	// The owned comment is reconciled after the durable report: the report is the
	// record, the comment is a convenience for whoever is deciding. A forge that
	// refuses the write must not discard an evaluation already stored.
	if err := r.applyVerdict(ctx, action, report); err != nil && r.logWarn != nil {
		r.logWarn("gatekeeper: could not reconcile the verdict comment", map[string]any{
			"repo": report.Repo, "pr": report.PRNumber, "action": string(action), "error": err.Error(),
		})
	}
	return report, nil
}

func gatekeeperCommitStatus(report Report) (string, string) {
	for _, reason := range report.Reasons {
		if reason.Code == ReasonGatekeeperCheckRequired {
			return "error", "Looper Gatekeeper is not configured as a required status on the target branch"
		}
	}
	for _, reason := range report.Reasons {
		if reason.Code == ReasonCodexReviewRequired {
			return "pending", "Waiting for Codex review of this commit"
		}
		if reason.Code == ReasonHeadStale {
			return "pending", "Waiting for Gatekeeper evaluation of the current head"
		}
	}
	if report.Eligible {
		if report.Mode == string(config.GatekeeperTrustAuto) && !report.Evidence.ReviewRequiredByPolicy {
			return "success", "Merge gates passed; review threshold not met"
		}
		return "success", "Current-head Codex review and merge gates passed"
	}
	for _, reason := range report.Reasons {
		if reason.Code == ReasonProviderStateUnavailable || reason.Code == ReasonProviderStateAmbiguous {
			return "error", "Gatekeeper could not verify current merge state"
		}
	}
	return "failure", "Gatekeeper found blocking merge state"
}

func containsRequiredCheck(checks []string, want string) bool {
	for _, check := range checks {
		if strings.EqualFold(strings.TrimSpace(check), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func withoutRequiredCheck(rules []githubinfra.RequiredCheckRule, name string) []githubinfra.RequiredCheckRule {
	out := make([]githubinfra.RequiredCheckRule, 0, len(rules))
	for _, rule := range rules {
		if strings.EqualFold(strings.TrimSpace(rule.Context), strings.TrimSpace(name)) && rule.AppID == 0 {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func evaluateRequiredChecks(required []githubinfra.RequiredCheckRule, checks githubinfra.PullRequestCheckRuns) ([]Reason, []CheckEvidence) {
	runs := make(map[string][]githubinfra.PullRequestCheckRun, len(checks.CheckRuns))
	for _, check := range checks.CheckRuns {
		key := strings.ToLower(strings.TrimSpace(check.Name))
		if key != "" {
			runs[key] = append(runs[key], check)
		}
	}
	statuses := make(map[string]githubinfra.PullRequestStatus, len(checks.Statuses))
	for _, status := range checks.Statuses {
		statuses[strings.ToLower(strings.TrimSpace(status.Context))] = status
	}
	reasons := make([]Reason, 0)
	evidence := make([]CheckEvidence, 0, len(required))
	for _, rule := range required {
		name := strings.TrimSpace(rule.Context)
		key := strings.ToLower(name)
		subject := requiredCheckSubject(name, rule.AppID)
		matchingRuns := matchingCheckRuns(runs[key], rule.AppID)
		if len(matchingRuns) > 0 {
			status, conclusion, code := aggregateCheckRuns(matchingRuns)
			evidence = append(evidence, CheckEvidence{Name: name, AppID: rule.AppID, Status: status, Conclusion: conclusion})
			if code != "" {
				reasons = append(reasons, Reason{Code: code, Subject: subject})
			}
			continue
		}
		if rule.AppID == 0 {
			if status, ok := statuses[key]; ok {
				state := strings.ToLower(strings.TrimSpace(status.State))
				evidence = append(evidence, CheckEvidence{Name: name, Status: state})
				switch state {
				case "success":
				case "pending":
					reasons = append(reasons, Reason{Code: ReasonCheckPending, Subject: subject})
				default:
					reasons = append(reasons, Reason{Code: ReasonCheckFailed, Subject: subject})
				}
				continue
			}
		}
		evidence = append(evidence, CheckEvidence{Name: name, AppID: rule.AppID})
		reasons = append(reasons, Reason{Code: ReasonCheckMissing, Subject: subject})
	}
	return reasons, evidence
}

func matchingCheckRuns(checks []githubinfra.PullRequestCheckRun, appID int64) []githubinfra.PullRequestCheckRun {
	if appID == 0 {
		return checks
	}
	out := make([]githubinfra.PullRequestCheckRun, 0, len(checks))
	for _, check := range checks {
		if check.AppID == appID {
			out = append(out, check)
		}
	}
	return out
}

func aggregateCheckRuns(checks []githubinfra.PullRequestCheckRun) (string, string, ReasonCode) {
	status := "completed"
	conclusion := "success"
	for _, check := range checks {
		checkStatus := strings.ToLower(strings.TrimSpace(check.Status))
		checkConclusion := strings.ToLower(strings.TrimSpace(check.Conclusion))
		if checkStatus != "completed" {
			return checkStatus, checkConclusion, ReasonCheckPending
		}
		if checkConclusion == "cancelled" {
			status, conclusion = checkStatus, checkConclusion
			continue
		}
		if checkConclusion != "success" && conclusion == "success" {
			status, conclusion = checkStatus, checkConclusion
		}
	}
	switch conclusion {
	case "success":
		return status, conclusion, ""
	case "cancelled":
		return status, conclusion, ReasonCheckCancelled
	default:
		return status, conclusion, ReasonCheckFailed
	}
}

func requiredCheckSubject(name string, appID int64) string {
	if appID == 0 {
		return name
	}
	return fmt.Sprintf("%s@app:%d", name, appID)
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

func hasReasonCode(reasons []Reason, code ReasonCode) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func withoutReasonCodes(reasons []Reason, codes ...ReasonCode) []Reason {
	if len(reasons) == 0 || len(codes) == 0 {
		return reasons
	}
	skip := make(map[ReasonCode]struct{}, len(codes))
	for _, code := range codes {
		skip[code] = struct{}{}
	}
	out := make([]Reason, 0, len(reasons))
	for _, reason := range reasons {
		if _, remove := skip[reason.Code]; remove {
			continue
		}
		out = append(out, reason)
	}
	return out
}
