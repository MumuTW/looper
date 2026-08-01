package github

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/diffanchor"
	"github.com/MumuTW/looper/internal/disclosure"
	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/outboundguard"
	"github.com/MumuTW/looper/internal/storage"
)

const javaScriptISOStringLayout = "2006-01-02T15:04:05.000Z"

const (
	defaultGhCommandTimeout = 60 * time.Second
	prListGhCommandTimeout  = 15 * time.Second
	prDiffGhCommandTimeout  = 180 * time.Second
)

var (
	prListJSONFields           = []string{"number", "title", "url", "state", "updatedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "reviews", "mergeStateStatus"}
	prDiscoveryListJSONFields  = []string{"number", "title", "url", "state", "updatedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "mergeStateStatus"}
	prViewMetadataJSONFields   = []string{"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "mergeStateStatus"}
	prViewFixerJSONFields      = []string{"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "statusCheckRollup", "mergeStateStatus"}
	prViewReviewerJSONFields   = []string{"number", "title", "body", "url", "state", "createdAt", "updatedAt", "closedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "reviews", "statusCheckRollup", "mergeStateStatus"}
	prViewGatekeeperJSONFields = []string{"number", "state", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeStateStatus", "changedFiles", "deletions"}
)

var prNumberURLPattern = regexp.MustCompile(`/pull/(\d+)(?:/|$)`)

var (
	fixerReplyEvidencePattern    = regexp.MustCompile(`(?i)<!--\s*looper-fixer-reply\s+thread:\S+\s+commit:([^\s]+)\s*-->`)
	fixerDeclinedEvidencePattern = regexp.MustCompile(`(?i)<!--\s*looper-fixer-reply-declined\s+thread:\S+\s+fingerprint:([^\s]+)\s*-->`)
	fixerDeferredEvidencePattern = regexp.MustCompile(`(?i)<!--\s*looper-fixer-reply-deferred\s+thread:\S+\s+fingerprint:([^\s]+)\s*-->`)
)

var (
	// ErrDiffTooLarge is returned when GitHub itself refuses a PR diff as oversized
	// (for example HTTP 406 too_large / 20k-line limit).
	ErrDiffTooLarge = errors.New("github pull request diff is too large")
	// ErrLocalCaptureTruncated is returned when Looper's local shell buffer truncated
	// otherwise-available command output. This is not a GitHub oversized-diff response.
	ErrLocalCaptureTruncated = errors.New("local shell capture truncated PR diff output")
	// ErrAnchorValidationUnavailable is returned when review submit cannot establish a
	// complete base/head diff authority for inline comment anchors.
	ErrAnchorValidationUnavailable = errors.New("pull request diff anchor validation authority is unavailable")
	// ErrReviewBaseHeadMismatch is returned when local git objects do not match the
	// refreshed PR base/head SHAs required for path-targeted anchor authority.
	ErrReviewBaseHeadMismatch = errors.New("local repository base/head does not match refreshed PR metadata")
	// ErrPullRequestAlreadyMerged distinguishes a concurrent successful merge
	// from an idempotent ordinary close. Callers that regenerate failed PRs must
	// abort rather than create a replacement for work that already landed.
	ErrPullRequestAlreadyMerged = errors.New("pull request already merged")
)

// Diagnostic / snapshot reason codes for diff capture and anchor authority.
const (
	DiffTruncationReasonLocalCapture   = "local_capture_truncated"
	DiffTruncationReasonGitHubTooLarge = "github_diff_too_large"
	AnchorOutsideCompleteDiffReason    = "anchor_outside_complete_diff"
	AnchorValidationUnavailableReason  = "anchor_validation_unavailable"
)

type Options struct {
	GHPath  string
	GitPath string
	CWD     string
	// Env carries the credential variables (see config.DaemonGitHubCredentialEnv)
	// that every `gh` child of this gateway must receive. Entries are merged over
	// the parent process environment, so the child keeps PATH/HOME and only the
	// named keys are overridden. Nil means inherit the parent environment
	// unchanged — which, in a detached daemon, means anonymous GitHub calls.
	Env                    map[string]string
	Now                    func() time.Time
	DiscoveryCacheTTL      time.Duration
	GHRun                  func(context.Context, shell.Options) (shell.Result, error)
	GitRun                 func(context.Context, shell.Options) (shell.Result, error)
	ReviewSubmitDiagnostic func(event string, fields map[string]any)
}

type Gateway struct {
	ghPath  string
	gitPath string
	cwd     string
	// ghEnv is the fully materialized child environment for gh invocations
	// (parent environment plus Options.Env), or nil to inherit unchanged.
	ghEnv                  map[string]string
	now                    func() time.Time
	discoveryCacheTTL      time.Duration
	discoveryCacheMu       sync.Mutex
	discoveryPRCache       map[string]discoveryPullRequestListCacheEntry
	discoveryReviewPRCache map[string]discoveryPullRequestListCacheEntry
	discoveryIssueCache    map[string]discoveryIssueListCacheEntry
	ghRun                  func(context.Context, shell.Options) (shell.Result, error)
	gitRun                 func(context.Context, shell.Options) (shell.Result, error)
	reviewSubmitDiagnostic func(event string, fields map[string]any)
}

type discoveryPullRequestListCacheEntry struct {
	expiresAt time.Time
	items     []PullRequestSummary
}

type discoveryIssueListCacheEntry struct {
	expiresAt time.Time
	items     []IssueSummary
}

type PullRequestSummary struct {
	Number             int64
	Title              string
	URL                string
	State              string
	UpdatedAt          string
	IsDraft            bool
	ReviewDecision     string
	Labels             []string
	HeadRefName        string
	BaseRefName        string
	HeadSHA            string
	BaseSHA            string
	HasConflicts       bool
	Author             string
	AuthorAssociation  string
	ReviewRequests     []string
	ReviewRequestUsers []GitHubUser
	Reviews            []map[string]any
}

type PullRequestDetail struct {
	Number             int64
	Title              string
	Body               string
	URL                string
	State              string
	CreatedAt          string
	UpdatedAt          string
	ClosedAt           string
	IsDraft            bool
	ReviewDecision     string
	Labels             []string
	HeadRefName        string
	BaseRefName        string
	HeadSHA            string
	BaseSHA            string
	Author             string
	AuthorAssociation  string
	CommentCount       int
	ReviewRequests     []string
	ReviewRequestUsers []GitHubUser
	HasConflicts       bool
	Comments           []map[string]any
	IssueComments      []CommentInfo
	Reviews            []map[string]any
	Checks             []map[string]any
	DiffStats          *PullRequestDiffStats `json:"diffStats,omitempty"`
	Mergeable          *bool
	MergeableState     MergeabilityState
	MergedAt           string
	AutoMerge          *PullRequestAutoMerge
}

// PullRequestDiffStats is the provider-observed change size for a pull request.
// It is fetched with the same head metadata as Gatekeeper's other gates.
type PullRequestDiffStats struct {
	ChangedFiles int `json:"changedFiles"`
	Deletions    int `json:"deletions"`
}

type PullRequestAutoMerge struct {
	EnabledBy   string
	MergeMethod string
}

type PullRequestCheckRuns struct {
	TotalCount         int
	CheckRuns          []PullRequestCheckRun
	StatusesTotalCount int
	Statuses           []PullRequestStatus
}

type PullRequestCheckRun struct {
	Name         string
	Status       string
	Conclusion   string
	AppID        int64
	CheckSuiteID int64
}

type PullRequestStatus struct {
	Context string
	State   string
}

type CommentInfo struct {
	ID     int64
	Author string
	// IsBot is GitHub's own classification of the comment author's account,
	// taken from the account `type` the API returns. It is read here rather than
	// derived from the login for the same reason as ReviewSummary.IsBot: the
	// login cannot answer it, because gh's GraphQL projections spell the
	// CodeRabbit account `coderabbitai`, with no `[bot]` suffix.
	//
	// A caller that needs it must use a reader that projects the account type; a
	// projection that omits it leaves this false.
	IsBot             bool
	AuthorAssociation string
	Body              string
	CreatedAt         string
	UpdatedAt         string
	URL               string
}

type CurrentUserIdentity struct {
	Login     string
	NumericID int64
}

type IssueTimelineInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}
type IssueReactionInput struct {
	Repo        string
	IssueNumber int64
	CommentID   int64
	CWD         string
}
type CreateIssueReactionInput struct {
	Repo        string
	IssueNumber int64
	CommentID   int64
	Content     string
	CWD         string
}
type RepositoryPermissionInput struct {
	Repo string
	User string
	CWD  string
}
type ListIssueBlockedByInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}
type IssueDependency struct {
	Number int64
	Repo   string
}
type IssueState struct {
	State       string
	StateReason string
}
type LinkedPullRequestsInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}
type PullRequestReviewStateInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type IssueReaction struct {
	ID        int64
	Content   string
	UserLogin string
}
type LinkedPullRequest struct {
	Number         int64
	State          string
	Repo           string
	URL            string
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
}
type PullRequestReviewState struct {
	RequestedReviewers  []string
	LatestReviewPerUser map[string]string
	LastReviewAt        string
}

type GitHubUser struct {
	Login string
	ID    int64
}

type PullRequestHeadAndAuthor struct {
	HeadSHA string
	Author  string
}

type RepositorySettingsInput struct {
	Repo string
	CWD  string
}

type RepositorySettings struct {
	AllowSquashMerge bool
	AllowMergeCommit bool
	AllowRebaseMerge bool
	AllowAutoMerge   bool
	Visibility       string
}

type BranchProtectionInput struct {
	Repo   string
	Branch string
	CWD    string
}

type BranchProtection struct {
	Enabled                      bool
	HasRequiredChecks            bool
	RequiredChecks               []string
	RequiredCheckRules           []RequiredCheckRule
	HasRequiredReviews           bool
	RequiredApprovingReviewCount int
}

// RequiredCheckRule preserves GitHub's optional App binding. AppID zero means
// the legacy context is not bound to a particular GitHub App.
type RequiredCheckRule struct {
	Context string
	AppID   int64
}

type IssueSummary struct {
	Number            int64
	Title             string
	Body              string
	URL               string
	State             string
	UpdatedAt         string
	Author            string
	AuthorAssociation string
	Assignees         []string
	AssigneeUsers     []GitHubUser
	Labels            []string
	IsPullRequest     bool
}

type IssueDetail struct {
	Number            int64
	Title             string
	Body              string
	URL               string
	State             string
	StateReason       string
	CreatedAt         string
	UpdatedAt         string
	ClosedAt          string
	Author            string
	AuthorAssociation string
	AuthorType        string
	Assignees         []string
	AssigneeUsers     []GitHubUser
	Labels            []string
	IsPullRequest     bool
	CommentCount      int
	Comments          []CommentInfo
}

type IssueRepository struct {
	Name     string
	FullName string
	URL      string
	HTMLURL  string
}

type DependencyIssue struct {
	ID            int64
	Number        int64
	Title         string
	URL           string
	HTMLURL       string
	RepositoryURL string
	State         string
	StateReason   string
	Repository    IssueRepository
}

type IssueCommentInput struct {
	Repo        string
	IssueNumber int64
	Body        string
	CWD         string
}

type IssueAssigneesInput struct {
	Repo        string
	IssueNumber int64
	Assignees   []string
	CWD         string
}

type IssueLabelsInput struct {
	Repo        string
	IssueNumber int64
	Labels      []string
	CWD         string
}

type IssueCommentResult struct {
	ID  int64
	URL string
}

type CreateIssueInput struct {
	Repo   string
	Title  string
	Body   string
	Labels []string
	CWD    string
}

type CreateIssueResult struct {
	Number int64
	URL    string
}

type UpdateIssueCommentInput struct {
	Repo      string
	CommentID int64
	Body      string
	CWD       string
}

type DeleteIssueCommentInput struct {
	Repo      string
	CommentID int64
	CWD       string
}

type CloseIssueInput struct {
	Repo        string
	IssueNumber int64
	StateReason string
	CWD         string
}

type ClosePullRequestInput struct {
	Repo         string
	PRNumber     int64
	DeleteBranch bool
	CWD          string
}

type EnableAutoMergeInput struct {
	Repo     string
	PRNumber int64
	Strategy config.ReviewerAutoMergeStrategy
	HeadSHA  string
	CWD      string
}

type MarkPullRequestReadyInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type ListPullRequestCommitsInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type PullRequestDraftEventsInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

// PullRequestDraftEvent is one draft-lifecycle event on a Pull Request's
// timeline: who took it out of draft, or who put it back, and when.
//
// It is the only durable record of that distinction. A draft the machine
// opened and a published Pull Request a human converted back to draft look
// identical on the Pull Request itself; only the timeline says which one is in
// front of you, and it says so across daemon restarts.
type PullRequestDraftEvent struct {
	Event     string
	Actor     string
	CreatedAt string
}

const (
	// DraftEventConvertToDraft and DraftEventReadyForReview are GitHub's own
	// names for the two timeline events that move a Pull Request across the
	// draft boundary.
	DraftEventConvertToDraft = "convert_to_draft"
	DraftEventReadyForReview = "ready_for_review"
)

// PullRequestCommit is one commit on a Pull Request branch together with the
// forge accounts GitHub attributes to its author and committer. CommitterKnown
// distinguishes a current REST response (where a missing committer is
// authoritative unattribution) from an older gateway implementation that only
// supplied Authors.
type PullRequestCommit struct {
	OID            string
	Authors        []string
	Committers     []string
	CommitterKnown bool
}

// CommitStatusInput is a policy result attached to one immutable commit. GitHub
// branch protection, not Looper's local event log, enforces this result before
// a merge or merge-queue progression.
type CommitStatusInput struct {
	Repo        string
	SHA         string
	Context     string
	State       string
	Description string
	CWD         string
}

type PullRequestCheckRunsInput struct {
	Repo string
	Ref  string
	CWD  string
}

// RerequestCheckSuiteInput identifies one GitHub check suite to rerun. The
// suite identifier is read from the check-runs response for the audited ref.
type RerequestCheckSuiteInput struct {
	Repo         string
	CheckSuiteID int64
	CWD          string
}

type SubmitReviewInput struct {
	Repo            string
	PRNumber        int64
	Event           string
	Body            string
	CommitID        string
	Comments        []ReviewComment
	Anchors         *diffanchor.Index
	Disclosure      config.DisclosureConfig
	DisclosureAgent string
	DisclosureModel string
	CWD             string
}

type ReviewComment struct {
	Body            string
	Path            string
	Line            int64
	Side            string
	StartLine       int64
	StartSide       string
	DiagnosticIndex int
}

type VerifyReviewMarkerInput struct {
	Repo                string
	PRNumber            int64
	Marker              string
	AllowedReviewEvents []string
	AuthorLogin         string
	AllowCleanComment   bool
	// SkipInlineComments skips the review-comments fetch when the caller only
	// needs marker presence and outcome (Gatekeeper auto mode).
	SkipInlineComments bool
	CWD                string
}

type ReviewMarkerResult struct {
	Found               bool
	Outcome             string
	Event               string
	AuthorLogin         string
	Body                string
	ReviewID            string
	InlineCommentBodies []string
}

type PullRequestReactionInput struct {
	Repo     string
	PRNumber int64
	Content  string
	CWD      string
}

type PullRequestCommentInput struct {
	Repo     string
	PRNumber int64
	Body     string
	CWD      string
}

type PullRequestLabelsInput struct {
	Repo     string
	PRNumber int64
	Labels   []string
	CWD      string
}

// ValidateMergifyRoutingInput identifies the repository configuration whose
// queue contract Gatekeeper relies on when it publishes the auto-merge label.
type ValidateMergifyRoutingInput struct {
	Repo string
	CWD  string
}

type PullRequestReviewersInput struct {
	Repo      string
	PRNumber  int64
	Reviewers []string
	CWD       string
}

type CreatePullRequestInput struct {
	Repo       string
	HeadBranch string
	BaseBranch string
	Title      string
	Body       string
	Draft      bool
	CWD        string
}

type CreatePullRequestResult struct {
	Number int64
	URL    string
}

type CompareBranchesInput struct {
	Repo       string
	BaseBranch string
	HeadBranch string
	CWD        string
}

type CompareBranchesResult struct {
	AheadBy      int
	BehindBy     int
	Status       string
	TotalCommits int
}

type UpdatePullRequestTitleInput struct {
	Repo     string
	PRNumber int64
	Title    string
	CWD      string
}

type UpdatePullRequestBodyInput struct {
	Repo     string
	PRNumber int64
	Body     string
	CWD      string
}

type ListOpenPullRequestsInput struct {
	Repo        string
	CWD         string
	Limit       int
	Label       string
	Labels      []string
	Author      string
	BaseRefName string
	Timeout     time.Duration
}

type ListReviewRequestedPullRequestsInput struct {
	Repo     string
	CWD      string
	Limit    int
	Reviewer string
	Timeout  time.Duration
}

type ListOpenIssuesInput struct {
	Repo     string
	CWD      string
	Limit    int
	Assignee string
	Label    string
	Labels   []string
	Search   string
}

type ViewIssueInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type ResolveReviewThreadInput struct {
	Repo     string
	ThreadID string
	CWD      string
}

type ListReviewThreadsInput struct {
	Repo     string
	PRNumber int64
	CWD      string
	Limit    int
}

type ViewReviewThreadInput struct {
	ThreadID string
	CWD      string
	Hostname string
}

type ReviewThread struct {
	ID         string
	IsResolved bool
	Path       string
	Line       int64
	URL        string
	Comments   []ReviewThreadComment
}

type ReviewThreadComment struct {
	ID                string
	Body              string
	Author            string
	AuthorAssociation string
	CreatedAt         string
	UpdatedAt         string
	Path              string
	Line              int64
	OriginalCommitOID string
	CommitOID         string
	URL               string
}

type AddReviewThreadReplyInput struct {
	Repo     string
	ThreadID string
	Body     string
	CWD      string
}

type CompareCommitsInput struct {
	Repo string
	Base string
	Head string
	CWD  string
}

type CompareCommitsResult struct {
	Status string
}

type GetPullRequestDiffInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type CapturePullRequestSnapshotInput struct {
	ProjectID string
	// Repo is the logical owner/name identity persisted with the snapshot.
	Repo string
	// TransportRepo may qualify Repo with a configured GHES hostname. It is
	// used only for forge commands and never becomes snapshot identity.
	TransportRepo string
	PRNumber      int64
	CWD           string
	CapturedAt    string
}

type ReviewThreadNotFoundError struct {
	ThreadID string
}

func (e *ReviewThreadNotFoundError) Error() string {
	return fmt.Sprintf("Review thread not found: %s", e.ThreadID)
}

func New(options Options) *Gateway {
	ghPath := strings.TrimSpace(options.GHPath)
	if ghPath == "" {
		ghPath = "gh"
	}
	gitPath := strings.TrimSpace(options.GitPath)
	if gitPath == "" {
		gitPath = "git"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	ghRun := options.GHRun
	if ghRun == nil {
		ghRun = shell.Run
	}
	gitRun := options.GitRun
	if gitRun == nil {
		gitRun = shell.Run
	}
	return &Gateway{
		ghPath:                 ghPath,
		gitPath:                gitPath,
		cwd:                    options.CWD,
		ghEnv:                  mergeIntoProcessEnv(options.Env),
		now:                    now,
		discoveryCacheTTL:      options.DiscoveryCacheTTL,
		discoveryPRCache:       map[string]discoveryPullRequestListCacheEntry{},
		discoveryReviewPRCache: map[string]discoveryPullRequestListCacheEntry{},
		discoveryIssueCache:    map[string]discoveryIssueListCacheEntry{},
		ghRun:                  ghRun,
		gitRun:                 gitRun,
		reviewSubmitDiagnostic: options.ReviewSubmitDiagnostic,
	}
}

func (g *Gateway) ListOpenPullRequests(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.listOpenPullRequests(ctx, input)
	}
	return g.listOpenPullRequestsRaw(ctx, input)
}

func (g *Gateway) ListReviewRequestedPullRequests(ctx context.Context, input ListReviewRequestedPullRequestsInput) ([]PullRequestSummary, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.listReviewRequestedPullRequests(ctx, input)
	}
	return g.listReviewRequestedPullRequestsRaw(ctx, input)
}

func (g *Gateway) listReviewRequestedPullRequestsRaw(ctx context.Context, input ListReviewRequestedPullRequestsInput) ([]PullRequestSummary, error) {
	reviewer := normalizeGitHubLogin(input.Reviewer)
	if reviewer == "" {
		return nil, fmt.Errorf("review requester login is required")
	}
	query := `
query($searchQuery: String!, $first: Int!) {
  search(type: ISSUE, query: $searchQuery, first: $first) {
    nodes {
      ... on PullRequest {
        number
        title
        url
        state
        updatedAt
        isDraft
        reviewDecision
        labels(first: 20) { nodes { name } }
        headRefName
        baseRefName
        headRefOid
        baseRefOid
        mergeStateStatus
        author { login }
      }
    }
  }
}`
	searchQuery := fmt.Sprintf("repo:%s is:pr is:open review-requested:%s", input.Repo, reviewer)
	timeout := input.Timeout
	if timeout <= 0 {
		timeout = defaultGhCommandTimeout
	}
	result, err := g.runGhWithTimeout(ctx, input.CWD, "", timeout, "api", "graphql", "-f", "query="+query, "-F", "searchQuery="+searchQuery, "-F", fmt.Sprintf("first=%d", defaultLimit(input.Limit)))
	if err != nil {
		return nil, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, err
	}
	nodes := digNodes(row, "data", "search")
	out := make([]PullRequestSummary, 0, len(nodes))
	for _, node := range nodes {
		number := asInt64(node["number"])
		if number <= 0 {
			continue
		}
		out = append(out, PullRequestSummary{
			Number:             number,
			Title:              asString(node["title"]),
			URL:                asString(node["url"]),
			State:              asString(node["state"]),
			UpdatedAt:          asString(node["updatedAt"]),
			IsDraft:            asBool(node["isDraft"]),
			ReviewDecision:     asString(node["reviewDecision"]),
			Labels:             extractLabelNamesFromConnection(node["labels"]),
			HeadRefName:        asString(node["headRefName"]),
			BaseRefName:        asString(node["baseRefName"]),
			HeadSHA:            asString(node["headRefOid"]),
			BaseSHA:            asString(node["baseRefOid"]),
			HasConflicts:       asString(node["mergeStateStatus"]) == "DIRTY",
			Author:             extractAuthor(node["author"]),
			ReviewRequests:     []string{reviewer},
			ReviewRequestUsers: []GitHubUser{{Login: reviewer}},
		})
	}
	return out, nil
}

func (g *Gateway) listOpenPullRequestsRaw(ctx context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	return g.listOpenPullRequestsWithFields(ctx, input, prListJSONFields)
}

func (g *Gateway) listOpenPullRequestsWithFields(ctx context.Context, input ListOpenPullRequestsInput, fields []string) ([]PullRequestSummary, error) {
	rows, err := g.listOpenPullRequestRows(ctx, input, fields)
	if err != nil && IsInaccessibleReviewRequestReviewerError(err) {
		rows, err = g.listOpenPullRequestRows(ctx, input, withoutJSONField(fields, "reviewRequests"))
	}
	if err != nil {
		return nil, err
	}
	out := make([]PullRequestSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, PullRequestSummary{
			Number:             asInt64(row["number"]),
			Title:              asString(row["title"]),
			URL:                asString(row["url"]),
			State:              asString(row["state"]),
			UpdatedAt:          asString(row["updatedAt"]),
			IsDraft:            asBool(row["isDraft"]),
			ReviewDecision:     asString(row["reviewDecision"]),
			Labels:             extractLabelNames(row["labels"]),
			HeadRefName:        asString(row["headRefName"]),
			BaseRefName:        asString(row["baseRefName"]),
			HeadSHA:            asString(row["headRefOid"]),
			BaseSHA:            asString(row["baseRefOid"]),
			HasConflicts:       asString(row["mergeStateStatus"]) == "DIRTY",
			Author:             extractAuthor(row["author"]),
			AuthorAssociation:  asString(row["authorAssociation"]),
			ReviewRequests:     extractReviewRequestLogins(row["reviewRequests"]),
			ReviewRequestUsers: extractReviewRequestUsers(row["reviewRequests"]),
			Reviews:            toObjectSlice(row["reviews"]),
		})
	}
	return out, nil
}

func (g *Gateway) listOpenPullRequestRows(ctx context.Context, input ListOpenPullRequestsInput, fields []string) ([]map[string]any, error) {
	args := []string{"pr", "list", "--repo", input.Repo, "--state", "open", "--limit", fmt.Sprintf("%d", defaultLimit(input.Limit))}
	labels := prListLabels(input)
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	if strings.TrimSpace(input.Author) != "" {
		args = append(args, "--author", strings.TrimSpace(input.Author))
	}
	if strings.TrimSpace(input.BaseRefName) != "" {
		args = append(args, "--base", strings.TrimSpace(input.BaseRefName))
	}
	args = append(args, "--json", strings.Join(fields, ","))

	timeout := input.Timeout
	if timeout <= 0 {
		timeout = defaultGhCommandTimeout
	}
	result, err := g.runGhWithTimeout(ctx, input.CWD, "", timeout, args...)
	if err != nil {
		return nil, err
	}
	return decodeJSONArray(result.Stdout)
}

func prListLabels(input ListOpenPullRequestsInput) []string {
	labels := input.Labels
	if len(labels) == 0 && strings.TrimSpace(input.Label) != "" {
		labels = []string{input.Label}
	}
	result := []string{}
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, label)
	}
	return result
}

func (g *Gateway) GetPullRequestAuthor(ctx context.Context, input ViewPullRequestInput) (string, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", "author")
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	return extractAuthor(row["author"]), nil
}

func (g *Gateway) GetPullRequestHeadAndAuthor(ctx context.Context, input ViewPullRequestInput) (PullRequestHeadAndAuthor, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", "headRefOid,author")
	if err != nil {
		return PullRequestHeadAndAuthor{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestHeadAndAuthor{}, err
	}
	return PullRequestHeadAndAuthor{HeadSHA: asString(row["headRefOid"]), Author: extractAuthor(row["author"])}, nil
}

func (g *Gateway) ListOpenIssues(ctx context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.listOpenIssues(ctx, input)
	}
	return g.listOpenIssuesRaw(ctx, input)
}

func (g *Gateway) listOpenIssuesRaw(ctx context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	args := []string{"issue", "list", "--repo", input.Repo, "--state", "open", "--limit", fmt.Sprintf("%d", defaultLimit(input.Limit))}
	if strings.TrimSpace(input.Assignee) != "" {
		args = append(args, "--assignee", input.Assignee)
	}
	for _, label := range issueListLabels(input) {
		args = append(args, "--label", label)
	}
	if strings.TrimSpace(input.Search) != "" {
		args = append(args, "--search", strings.TrimSpace(input.Search))
	}
	args = append(args, "--json", strings.Join([]string{"number", "title", "body", "url", "state", "updatedAt", "author", "assignees", "labels"}, ","))

	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]IssueSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, IssueSummary{
			Number:            asInt64(row["number"]),
			Title:             asString(row["title"]),
			Body:              asString(row["body"]),
			URL:               asString(row["url"]),
			State:             asString(row["state"]),
			UpdatedAt:         asString(row["updatedAt"]),
			Author:            extractAuthor(row["author"]),
			AuthorAssociation: asString(row["authorAssociation"]),
			Assignees:         extractActorLogins(row["assignees"]),
			AssigneeUsers:     extractActorUsers(row["assignees"]),
			Labels:            extractLabelNames(row["labels"]),
		})
	}
	return out, nil
}

func issueListLabels(input ListOpenIssuesInput) []string {
	labels := input.Labels
	if len(labels) == 0 && strings.TrimSpace(input.Label) != "" {
		labels = []string{input.Label}
	}
	result := []string{}
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, label)
	}
	return result
}

func (g *Gateway) ViewIssue(ctx context.Context, input ViewIssueInput) (IssueDetail, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return IssueDetail{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return IssueDetail{}, err
	}
	commentArgs := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber)}
	if hostname != "" {
		commentArgs = append(commentArgs, "--hostname", hostname)
	}
	commentsResult, err := g.runGh(ctx, input.CWD, "", commentArgs...)
	if err != nil {
		return IssueDetail{}, err
	}
	commentRows, err := decodeJSONArrayOrPages(commentsResult.Stdout)
	if err != nil {
		return IssueDetail{}, err
	}
	return IssueDetail{
		Number:            asInt64(row["number"]),
		Title:             asString(row["title"]),
		Body:              asString(row["body"]),
		URL:               firstNonEmpty(asString(row["html_url"]), asString(row["url"])),
		State:             asString(row["state"]),
		StateReason:       firstNonEmpty(asString(row["state_reason"]), asString(row["stateReason"])),
		CreatedAt:         firstNonEmpty(asString(row["created_at"]), asString(row["createdAt"])),
		UpdatedAt:         firstNonEmpty(asString(row["updated_at"]), asString(row["updatedAt"])),
		ClosedAt:          firstNonEmpty(asString(row["closed_at"]), asString(row["closedAt"])),
		Author:            extractAuthor(firstNonNil(row["user"], row["author"])),
		AuthorAssociation: asString(row["author_association"]),
		AuthorType:        extractActorType(firstNonNil(row["user"], row["author"])),
		Assignees:         extractActorLogins(row["assignees"]),
		AssigneeUsers:     extractActorUsers(row["assignees"]),
		Labels:            extractLabelNames(row["labels"]),
		IsPullRequest:     row["pull_request"] != nil,
		CommentCount:      len(commentRows),
		Comments:          extractCommentInfos(commentRows),
	}, nil
}

func (g *Gateway) GetIssueState(ctx context.Context, input ViewIssueInput) (IssueState, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return IssueState{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return IssueState{}, err
	}
	return IssueState{State: asString(row["state"]), StateReason: firstNonEmpty(asString(row["state_reason"]), asString(row["stateReason"]))}, nil
}

// GetIssueLabels reads only an issue's label names. ViewIssue answers the same
// question but also pages through the entire comment thread, so callers that
// need labels alone pay one gh invocation plus a paginated fetch per issue.
func (g *Gateway) GetIssueLabels(ctx context.Context, input ViewIssueInput) ([]string, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, err
	}
	return extractLabelNames(row["labels"]), nil
}

func (g *Gateway) ListIssueBlockedBy(ctx context.Context, input ListIssueBlockedByInput) ([]IssueDependency, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]IssueDependency, 0, len(rows))
	for _, row := range rows {
		dependency := IssueDependency{Number: asInt64(row["number"]), Repo: dependencyRepo(firstNonNil(row["repository"], row["repo"]), asString(row["repository_url"]), input.Repo)}
		if dependency.Number <= 0 {
			continue
		}
		out = append(out, dependency)
	}
	return out, nil
}

func (g *Gateway) ListBlockedByIssues(ctx context.Context, input ViewIssueInput) ([]DependencyIssue, error) {
	return g.listDependencyIssues(ctx, input, "dependencies/blocked_by")
}

func (g *Gateway) ListBlockingIssues(ctx context.Context, input ViewIssueInput) ([]DependencyIssue, error) {
	return g.listDependencyIssues(ctx, input, "dependencies/blocking")
}

func (g *Gateway) ListSubIssues(ctx context.Context, input ViewIssueInput) ([]DependencyIssue, error) {
	return g.listDependencyIssues(ctx, input, "sub_issues")
}

func (g *Gateway) listDependencyIssues(ctx context.Context, input ViewIssueInput, suffix string) ([]DependencyIssue, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/%s", repo, input.IssueNumber, suffix), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]DependencyIssue, 0, len(rows))
	for _, row := range rows {
		out = append(out, extractDependencyIssue(row, input.Repo))
	}
	return out, nil
}

// findAnyIssueNumberJQ projects each issues page down to number + PR marker only.
// Full issue bodies easily exceed the shell capture cap on busy sandbox repos;
// gh applies this filter before writing to the bounded stdout buffer.
const findAnyIssueNumberJQ = `[.[] | {number, pull_request}]`

func (g *Gateway) FindAnyIssueNumber(ctx context.Context, repo string, cwd string) (int64, error) {
	hostname, repoName := splitRepoHostname(repo)
	for page := 1; ; page++ {
		args := []string{
			"api",
			fmt.Sprintf("repos/%s/issues?state=all&per_page=100&page=%d", repoName, page),
			"--jq",
			findAnyIssueNumberJQ,
		}
		if hostname != "" {
			args = append(args, "--hostname", hostname)
		}
		result, err := g.runGh(ctx, cwd, "", args...)
		if err != nil {
			return 0, err
		}
		rows, err := decodeJSONArray(result.Stdout)
		if err != nil {
			return 0, err
		}
		if len(rows) == 0 {
			return 0, nil
		}
		for _, row := range rows {
			if row["pull_request"] != nil {
				continue
			}
			if issueNumber := asInt64(row["number"]); issueNumber > 0 {
				return issueNumber, nil
			}
		}
	}
}

func (g *Gateway) ListIssueComments(ctx context.Context, input ViewIssueInput) ([]CommentInfo, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	return extractCommentInfos(rows), nil
}

// ListPullRequestCommits returns provider-authored commit identities for the
// PR.  The GitHub commit list is the authority for the human-commit guard;
// local lifecycle metadata cannot detect a later human push.
// listPullRequestAutomationComments keeps PR discovery independent of the size
// of the full issue conversation. gh applies this projection to each page
// before writing to the shell capture buffer, so only comments consumed by the
// fixer/reviewer protocols cross that bounded boundary.
func (g *Gateway) listPullRequestAutomationComments(ctx context.Context, input ViewIssueInput) ([]CommentInfo, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	filter := `.[] | select((.body // "") | (contains("looper:fixer-round") or contains("looper:conflict-notice") or contains("looper:reviewer:automerge-refused"))) | {id,body,html_url,updated_at,user:{login:.user.login}}`
	args := []string{"api", "--paginate", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber), "--jq", filter}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONObjects(result.Stdout)
	if err != nil {
		return nil, err
	}
	return extractCommentInfos(rows), nil
}

// ListIssueCommentsContaining returns only the comments whose body contains one
// of the supplied markers, projected to the fields a marker-driven detector
// needs: id, body, and the author's login and account type.
//
// Reading the whole conversation to find one notice is what makes a large
// discussion fail: the unprojected reader hands every body to a bounded shell
// capture, so a busy pull request returns a truncation error and the caller
// records nothing at all. gh applies this filter to each page before anything
// crosses that boundary, so the size of the discussion cannot erase the answer.
//
// The account type is projected deliberately. A marker is body text, and body
// text is not authority: the detector has to be able to tell the account that
// posts the notice from a human quoting it.
func (g *Gateway) ListIssueCommentsContaining(ctx context.Context, input ViewIssueInput, markers []string) ([]CommentInfo, error) {
	if len(markers) == 0 {
		return []CommentInfo{}, nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	tests := make([]string, 0, len(markers))
	for _, marker := range markers {
		encoded, err := json.Marshal(marker)
		if err != nil {
			return nil, fmt.Errorf("encode comment marker: %w", err)
		}
		tests = append(tests, fmt.Sprintf("contains(%s)", encoded))
	}
	filter := fmt.Sprintf(`.[] | select((.body // "") | (%s)) | {id,body,user:{login:.user.login,type:.user.type}}`, strings.Join(tests, " or "))
	args := []string{"api", "--paginate", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber), "--jq", filter}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONObjects(result.Stdout)
	if err != nil {
		return nil, err
	}
	return extractCommentInfos(rows), nil
}

func (g *Gateway) ListIssueTimeline(ctx context.Context, input IssueTimelineInput) ([]map[string]any, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/timeline", repo, input.IssueNumber), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	return decodeJSONArrayOrPages(result.Stdout)
}

func (g *Gateway) ListIssueReactions(ctx context.Context, input IssueReactionInput) ([]IssueReaction, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	endpoint := fmt.Sprintf("repos/%s/issues/%d/reactions", repo, input.IssueNumber)
	if input.CommentID > 0 {
		endpoint = fmt.Sprintf("repos/%s/issues/comments/%d/reactions", repo, input.CommentID)
	}
	args := []string{"api", "--paginate", "--slurp", endpoint, "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make([]IssueReaction, 0, len(rows))
	for _, row := range rows {
		if reaction, ok := normalizeReaction(row); ok {
			out = append(out, IssueReaction{ID: reaction.ID, Content: reaction.Content, UserLogin: reaction.UserLogin})
		}
	}
	return out, nil
}

func (g *Gateway) AddIssueReaction(ctx context.Context, input CreateIssueReactionInput) error {
	content := strings.TrimSpace(input.Content)
	if content == "" {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	endpoint := fmt.Sprintf("repos/%s/issues/%d/reactions", repo, input.IssueNumber)
	if input.CommentID > 0 {
		endpoint = fmt.Sprintf("repos/%s/issues/comments/%d/reactions", repo, input.CommentID)
	}
	args := []string{"api", endpoint, "--method", "POST", "-H", "Accept: application/vnd.github+json", "-f", "content=" + content}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) CreateIssueComment(ctx context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	if err := outboundguard.Validate(outboundguard.Field{Name: "issue comment body", Text: input.Body}); err != nil {
		return IssueCommentResult{}, err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/comments", repo, input.IssueNumber), "--method", "POST", "-f", "body=" + input.Body}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return IssueCommentResult{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return IssueCommentResult{}, err
	}
	return IssueCommentResult{ID: asInt64(row["id"]), URL: asString(row["html_url"])}, nil
}

// FindIssueBySourceStamp returns the number of an Issue whose body carries the
// given intake stamp, or 0 when none is indexed. Both open and closed Issues are
// searched, because a request replayed after a long outage may target one that
// has since been closed.
//
// This is a best-effort duplicate check, not a lock: GitHub's search index is
// eventually consistent, so an Issue created seconds ago may not be found yet.
// See the intake lane for the trade-off that depends on it.
func (g *Gateway) FindIssueBySourceStamp(ctx context.Context, repo, stamp, cwd string) (int64, error) {
	stamp = strings.TrimSpace(stamp)
	if stamp == "" {
		return 0, fmt.Errorf("intake stamp is required")
	}
	hostname, repoSlug := splitRepoHostname(repo)
	args := []string{"issue", "list", "--repo", repoSlug, "--state", "all", "--limit", "10", "--search", stamp, "--json", "number"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return 0, err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return 0, err
	}
	for _, row := range rows {
		if number := asInt64(row["number"]); number > 0 {
			return number, nil
		}
	}
	return 0, nil
}

// CreateIssue opens a new Issue. Intake uses it to turn a chat message into the
// forge record that the existing Triager and Planner lanes discover; nothing
// here classifies or routes the Issue.
func (g *Gateway) CreateIssue(ctx context.Context, input CreateIssueInput) (CreateIssueResult, error) {
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return CreateIssueResult{}, fmt.Errorf("issue title is required")
	}
	if err := outboundguard.Validate(
		outboundguard.Field{Name: "issue title", Text: title},
		outboundguard.Field{Name: "issue body", Text: input.Body},
	); err != nil {
		return CreateIssueResult{}, err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues", repo), "--method", "POST", "-f", "title=" + title, "-f", "body=" + input.Body}
	for _, label := range input.Labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		args = append(args, "-f", "labels[]="+label)
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return CreateIssueResult{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return CreateIssueResult{}, err
	}
	return CreateIssueResult{Number: asInt64(row["number"]), URL: asString(row["html_url"])}, nil
}

func (g *Gateway) UpdateIssueComment(ctx context.Context, input UpdateIssueCommentInput) error {
	if err := outboundguard.Validate(outboundguard.Field{Name: "issue comment body", Text: input.Body}); err != nil {
		return err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/comments/%d", repo, input.CommentID), "--method", "PATCH", "-f", "body=" + input.Body}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) DeleteIssueComment(ctx context.Context, input DeleteIssueCommentInput) error {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/comments/%d", repo, input.CommentID), "--method", "DELETE"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) CloseIssue(ctx context.Context, input CloseIssueInput) error {
	reason, err := validateCloseIssueStateReason(input.StateReason)
	if err != nil {
		return err
	}
	state, err := g.viewIssueState(ctx, input.Repo, input.IssueNumber, input.CWD)
	if err != nil {
		return err
	}
	if state == "closed" {
		return nil
	}
	_, err = g.runGh(ctx, input.CWD, "", "issue", "close", strconv.FormatInt(input.IssueNumber, 10), "--repo", input.Repo, "--reason", reason)
	if err == nil {
		return nil
	}
	state, stateErr := g.viewIssueState(ctx, input.Repo, input.IssueNumber, input.CWD)
	if stateErr == nil && state == "closed" {
		return nil
	}
	return err
}

func (g *Gateway) AddIssueAssignees(ctx context.Context, input IssueAssigneesInput) error {
	assignees := compactIssueAssignees(input.Assignees)
	if len(assignees) == 0 {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/assignees", repo, input.IssueNumber), "--method", "POST"}
	for _, assignee := range assignees {
		args = append(args, "-f", "assignees[]="+assignee)
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) AddIssueLabels(ctx context.Context, input IssueLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	if err := g.ensureLabelsExist(ctx, input.Repo, input.Labels, input.CWD); err != nil {
		return err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels", repo, input.IssueNumber), "--method", "POST"}
	for _, label := range input.Labels {
		args = append(args, "-f", "labels[]="+label)
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) RemoveIssueLabels(ctx context.Context, input IssueLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	for _, label := range input.Labels {
		args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels/%s", repo, input.IssueNumber, encodeURIComponent(label)), "--method", "DELETE"}
		if hostname != "" {
			args = append(args, "--hostname", hostname)
		}
		_, err := g.runGh(ctx, input.CWD, "", args...)
		if err == nil {
			continue
		}
		if isMissingPullRequestLabelDelete(err) {
			continue
		}
		return err
	}
	return nil
}

func (g *Gateway) GetRepositoryPermission(ctx context.Context, input RepositoryPermissionInput) (string, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/collaborators/%s/permission", repo, input.User)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		if shellErr, ok := err.(*shell.CommandExecutionError); ok && strings.Contains(strings.ToLower(shellErr.Result.Stderr), "404") {
			return "", nil
		}
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	permission := strings.ToLower(strings.TrimSpace(asString(row["permission"])))
	// A 200 with no recognized permission value (e.g. "{}") is a semantically
	// malformed response, not a denial. GitHub's
	// collaborators/{username}/permission endpoint returns the legacy base
	// roles admin/write/read/none (maintain is mapped to write, triage to
	// read); maintain/triage are accepted defensively but never appear in
	// practice. Surfacing an unrecognized value as an error keeps callers
	// fail-closed with a diagnostic instead of silently treating a legitimate
	// maintainer's answer as unauthorized. A 404 (genuine non-collaborator) is
	// mapped to "" and returned without error above, so the empty string
	// reaching here is a malformed 200, not a denial.
	if !isKnownRepositoryPermission(permission) {
		return "", fmt.Errorf("github: malformed collaborator permission response for %s/%s: %q", repo, input.User, permission)
	}
	return permission, nil
}

// isKnownRepositoryPermission reports whether permission is one of the values
// GitHub's collaborators/{username}/permission endpoint can return. The
// documented legacy base roles are admin/write/read/none; maintain and triage
// are included defensively (the endpoint maps them to write/read, but older
// GHES surfaces have historically varied). "none" is a valid denial, not
// malformed: callers route it through RepositoryPermissionAllowsWrite, which
// returns false for it.
func isKnownRepositoryPermission(permission string) bool {
	switch permission {
	case "admin", "maintain", "write", "triage", "read", "none":
		return true
	default:
		return false
	}
}

// RepositoryPermissionAllowsWrite is the shared authority predicate for
// maintainer-confirmed GitHub actions.
func RepositoryPermissionAllowsWrite(permission string) bool {
	switch strings.ToLower(strings.TrimSpace(permission)) {
	case "admin", "maintain", "write":
		return true
	default:
		return false
	}
}

func (g *Gateway) GetRepositorySettings(ctx context.Context, input RepositorySettingsInput) (RepositorySettings, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s", repo)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return RepositorySettings{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return RepositorySettings{}, err
	}
	return RepositorySettings{
		AllowSquashMerge: asBool(row["allow_squash_merge"]),
		AllowMergeCommit: asBool(row["allow_merge_commit"]),
		AllowRebaseMerge: asBool(row["allow_rebase_merge"]),
		AllowAutoMerge:   asBool(row["allow_auto_merge"]),
		Visibility:       repositoryVisibility(row),
	}, nil
}

func repositoryVisibility(row map[string]any) string {
	if visibility := strings.ToLower(strings.TrimSpace(asString(row["visibility"]))); visibility != "" {
		return visibility
	}
	if private, ok := row["private"].(bool); ok {
		if private {
			return "private"
		}
		return "public"
	}
	return "unknown"
}

func (g *Gateway) GetBranchProtection(ctx context.Context, input BranchProtectionInput) (BranchProtection, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/branches/%s/protection", repo, encodeURIComponent(input.Branch))}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		if IsNotFoundError(err) {
			return BranchProtection{}, nil
		}
		return BranchProtection{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return BranchProtection{}, err
	}
	requiredStatusChecks, _ := row["required_status_checks"].(map[string]any)
	hasRequiredChecks := false
	requiredChecks := []string{}
	requiredCheckRules := []RequiredCheckRule{}
	if requiredStatusChecks != nil {
		for _, check := range toObjectSlice(requiredStatusChecks["checks"]) {
			name := firstNonEmpty(asString(check["context"]), asString(check["name"]))
			if strings.TrimSpace(name) != "" {
				requiredChecks = append(requiredChecks, name)
				requiredCheckRules = append(requiredCheckRules, RequiredCheckRule{Context: name, AppID: asInt64(check["app_id"])})
			}
		}
		if len(requiredChecks) > 0 {
			hasRequiredChecks = true
		}
		if contexts, ok := requiredStatusChecks["contexts"].([]any); ok && len(contexts) > 0 {
			hasRequiredChecks = true
			for _, context := range contexts {
				name := asString(context)
				if strings.TrimSpace(name) != "" {
					requiredChecks = append(requiredChecks, name)
					requiredCheckRules = append(requiredCheckRules, RequiredCheckRule{Context: name})
				}
			}
		}
	}
	requiredPullRequestReviews, _ := row["required_pull_request_reviews"].(map[string]any)
	requiredApprovingReviewCount := int(asInt64(requiredPullRequestReviews["required_approving_review_count"]))
	return BranchProtection{
		Enabled:                      true,
		HasRequiredChecks:            hasRequiredChecks,
		RequiredChecks:               uniqueStrings(requiredChecks),
		RequiredCheckRules:           uniqueRequiredCheckRules(requiredCheckRules),
		HasRequiredReviews:           requiredPullRequestReviews != nil,
		RequiredApprovingReviewCount: requiredApprovingReviewCount,
	}, nil
}

func compactIssueAssignees(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (g *Gateway) ViewPullRequest(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.viewPullRequest(ctx, input)
	}
	return g.viewPullRequestWithFields(ctx, input, prViewMetadataJSONFields, false, false)
}

func (g *Gateway) ViewPullRequestForFixer(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	return g.viewPullRequestWithFields(ctx, input, prViewFixerJSONFields, true, true)
}

func (g *Gateway) ViewPullRequestForReviewer(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	return g.viewPullRequestWithFields(ctx, input, prViewReviewerJSONFields, true, true)
}

func (g *Gateway) ViewPullRequestForGatekeeper(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	// Gate decisions must bypass the per-tick discovery snapshot: this method
	// is the fresh provider read that binds a report to the current head.
	return g.viewPullRequestWithFields(ctx, input, prViewGatekeeperJSONFields, false, false)
}

func (g *Gateway) viewPullRequestWithFields(ctx context.Context, input ViewPullRequestInput, fields []string, includeReviewThreads bool, includeIssueComments bool) (PullRequestDetail, error) {
	row, err := g.viewPullRequestRow(ctx, input, fields)
	if err != nil && IsInaccessibleReviewRequestReviewerError(err) {
		row, err = g.viewPullRequestRow(ctx, input, withoutJSONField(fields, "reviewRequests"))
	}
	if err != nil {
		return PullRequestDetail{}, err
	}
	threads := []map[string]any(nil)
	if includeReviewThreads {
		threads, err = g.fetchReviewThreads(ctx, input.Repo, input.PRNumber, input.CWD)
		if err != nil {
			return PullRequestDetail{}, err
		}
	}
	issueComments := []CommentInfo(nil)
	if includeIssueComments {
		issueComments, err = g.listPullRequestAutomationComments(ctx, ViewIssueInput{Repo: input.Repo, IssueNumber: input.PRNumber, CWD: input.CWD})
		if err != nil {
			return PullRequestDetail{}, err
		}
	}
	return pullRequestDetailFromViewRow(row, threads, issueComments), nil
}

func pullRequestDetailFromViewRow(row map[string]any, threads []map[string]any, issueComments []CommentInfo) PullRequestDetail {
	return PullRequestDetail{
		Number:             asInt64(row["number"]),
		Title:              asString(row["title"]),
		Body:               asString(row["body"]),
		URL:                asString(row["url"]),
		State:              asString(row["state"]),
		CreatedAt:          asString(row["createdAt"]),
		UpdatedAt:          asString(row["updatedAt"]),
		ClosedAt:           asString(row["closedAt"]),
		IsDraft:            asBool(row["isDraft"]),
		ReviewDecision:     asString(row["reviewDecision"]),
		Labels:             extractLabelNames(row["labels"]),
		HeadRefName:        asString(row["headRefName"]),
		BaseRefName:        asString(row["baseRefName"]),
		HeadSHA:            asString(row["headRefOid"]),
		BaseSHA:            asString(row["baseRefOid"]),
		Author:             extractAuthor(row["author"]),
		AuthorAssociation:  asString(row["authorAssociation"]),
		CommentCount:       len(issueComments),
		ReviewRequests:     extractReviewRequestLogins(row["reviewRequests"]),
		ReviewRequestUsers: extractReviewRequestUsers(row["reviewRequests"]),
		HasConflicts:       asString(row["mergeStateStatus"]) == "DIRTY",
		Comments:           threads,
		IssueComments:      issueComments,
		Reviews:            toObjectSlice(row["reviews"]),
		Checks:             toObjectSlice(row["statusCheckRollup"]),
		DiffStats:          pullRequestDiffStatsFromRow(row),
		Mergeable:          boolPtrFromValue(row["mergeable"]),
		MergeableState:     ParseMergeabilityState(firstNonEmpty(asString(row["mergeable_state"]), asString(row["mergeStateStatus"]))),
		MergedAt:           asString(row["merged_at"]),
		AutoMerge:          extractAutoMerge(row["auto_merge"]),
	}
}

func pullRequestDiffStatsFromRow(row map[string]any) *PullRequestDiffStats {
	changedFiles, changedFilesOK := firstPresentRowValue(row, "changedFiles", "changed_files")
	deletions, deletionsOK := firstPresentRowValue(row, "deletions")
	if !changedFilesOK || !deletionsOK {
		return nil
	}
	return &PullRequestDiffStats{
		ChangedFiles: int(asInt64(changedFiles)),
		Deletions:    int(asInt64(deletions)),
	}
}

func firstPresentRowValue(row map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		value, ok := row[key]
		if ok && value != nil {
			return value, true
		}
	}
	return nil, false
}

func (g *Gateway) viewPullRequestRow(ctx context.Context, input ViewPullRequestInput, fields []string) (map[string]any, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", strings.Join(fields, ","))
	if err != nil {
		return nil, err
	}
	return decodeJSONObject(result.Stdout)
}

func (g *Gateway) ViewPullRequestMergeWatch(ctx context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/pulls/%d", repo, input.PRNumber), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return PullRequestDetail{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestDetail{}, err
	}
	return PullRequestDetail{
		Number:         asInt64(row["number"]),
		Title:          asString(row["title"]),
		Body:           asString(row["body"]),
		URL:            firstNonEmpty(asString(row["html_url"]), asString(row["url"])),
		State:          asString(row["state"]),
		CreatedAt:      firstNonEmpty(asString(row["created_at"]), asString(row["createdAt"])),
		UpdatedAt:      firstNonEmpty(asString(row["updated_at"]), asString(row["updatedAt"])),
		ClosedAt:       firstNonEmpty(asString(row["closed_at"]), asString(row["closedAt"])),
		IsDraft:        asBool(row["draft"]),
		Author:         extractAuthor(row["user"]),
		MergedAt:       firstNonEmpty(asString(row["merged_at"]), asString(row["mergedAt"])),
		Labels:         extractLabelNames(row["labels"]),
		HeadRefName:    nestedString(row, "head", "ref"),
		BaseRefName:    nestedString(row, "base", "ref"),
		HeadSHA:        nestedString(row, "head", "sha"),
		BaseSHA:        nestedString(row, "base", "sha"),
		DiffStats:      pullRequestDiffStatsFromRow(row),
		Mergeable:      boolPtrFromValue(row["mergeable"]),
		MergeableState: ParseMergeabilityState(firstNonEmpty(asString(row["mergeable_state"]), asString(row["mergeStateStatus"]))),
		AutoMerge:      extractAutoMerge(row["auto_merge"]),
	}, nil
}

func (g *Gateway) ListPullRequestCheckRuns(ctx context.Context, input PullRequestCheckRunsInput) (PullRequestCheckRuns, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=latest&per_page=100", repo, encodeURIComponent(input.Ref)), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	statusArgs := []string{"api", fmt.Sprintf("repos/%s/commits/%s/status?per_page=100", repo, encodeURIComponent(input.Ref)), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		statusArgs = append(statusArgs, "--hostname", hostname)
	}
	statusResult, err := g.runGh(ctx, input.CWD, "", statusArgs...)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	statusRow, err := decodeJSONObject(statusResult.Stdout)
	if err != nil {
		return PullRequestCheckRuns{}, err
	}
	checkRuns := toObjectSlice(row["check_runs"])
	out := PullRequestCheckRuns{TotalCount: int(asInt64(row["total_count"])), CheckRuns: make([]PullRequestCheckRun, 0, len(checkRuns))}
	for _, checkRun := range checkRuns {
		out.CheckRuns = append(out.CheckRuns, PullRequestCheckRun{Name: asString(checkRun["name"]), Status: asString(checkRun["status"]), Conclusion: asString(checkRun["conclusion"]), AppID: nestedInt64(checkRun, "app", "id"), CheckSuiteID: nestedInt64(checkRun, "check_suite", "id")})
	}
	statuses := toObjectSlice(statusRow["statuses"])
	out.StatusesTotalCount = int(asInt64(statusRow["total_count"]))
	out.Statuses = make([]PullRequestStatus, 0, len(statuses))
	seenContexts := map[string]struct{}{}
	for _, status := range statuses {
		contextName := asString(status["context"])
		key := strings.ToLower(strings.TrimSpace(contextName))
		if key == "" {
			continue
		}
		if _, ok := seenContexts[key]; ok {
			continue
		}
		seenContexts[key] = struct{}{}
		out.Statuses = append(out.Statuses, PullRequestStatus{Context: contextName, State: asString(status["state"])})
	}
	return out, nil
}

// RerequestCheckSuite asks GitHub to rerun one check suite. It deliberately
// does not infer a suite from a check name: the caller must carry the exact
// suite ID obtained from the observed branch head.
func (g *Gateway) RerequestCheckSuite(ctx context.Context, input RerequestCheckSuiteInput) error {
	if strings.TrimSpace(input.Repo) == "" {
		return fmt.Errorf("rerequest check suite: repository is required")
	}
	if input.CheckSuiteID <= 0 {
		return fmt.Errorf("rerequest check suite: positive check suite ID is required")
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/check-suites/%d/rerequest", repo, input.CheckSuiteID), "--method", "POST", "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) ListLinkedPullRequests(ctx context.Context, input LinkedPullRequestsInput) ([]LinkedPullRequest, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	owner, repoName := splitRepoOwnerName(repo)
	out := make([]LinkedPullRequest, 0, 20)
	cursor := ""
	for {
		nodes, nextCursor, hasNextPage, err := g.fetchLinkedPullRequestsPage(ctx, input.CWD, hostname, owner, repoName, input.IssueNumber, cursor)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			mergeCommit, _ := node["mergeCommit"].(map[string]any)
			repository, _ := node["repository"].(map[string]any)
			out = append(out, LinkedPullRequest{Number: asInt64(node["number"]), State: asString(node["state"]), Repo: asString(repository["nameWithOwner"]), URL: asString(node["url"]), Merged: asString(node["state"]) == "MERGED" || asString(node["mergedAt"]) != "", MergedAt: asString(node["mergedAt"]), MergeCommitSHA: asString(mergeCommit["oid"])})
		}
		if !hasNextPage || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) ListPullRequestReviewState(ctx context.Context, input PullRequestReviewStateInput) (PullRequestReviewState, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--json", "reviewRequests,reviews")
	if err != nil {
		return PullRequestReviewState{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return PullRequestReviewState{}, err
	}
	latest := map[string]string{}
	last := ""
	for _, review := range toObjectSlice(row["reviews"]) {
		if user := extractAuthor(review["author"]); user != "" {
			latest[user] = asString(review["state"])
		}
		if at := firstNonEmpty(asString(review["submittedAt"]), asString(review["updatedAt"])); at > last {
			last = at
		}
	}
	return PullRequestReviewState{RequestedReviewers: extractReviewRequestLogins(row["reviewRequests"]), LatestReviewPerUser: latest, LastReviewAt: last}, nil
}

func (g *Gateway) ClosePullRequest(ctx context.Context, input ClosePullRequestInput) error {
	state, err := g.viewPullRequestState(ctx, input.Repo, input.PRNumber, input.CWD)
	if err != nil {
		return err
	}
	if state == "closed" {
		return nil
	}
	if state == "merged" {
		return ErrPullRequestAlreadyMerged
	}
	args := []string{"pr", "close", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo}
	if input.DeleteBranch {
		args = append(args, "--delete-branch")
	}
	_, err = g.runGh(ctx, input.CWD, "", args...)
	if err == nil {
		return nil
	}
	state, stateErr := g.viewPullRequestState(ctx, input.Repo, input.PRNumber, input.CWD)
	if stateErr == nil && state == "closed" {
		return nil
	}
	if stateErr == nil && state == "merged" {
		return ErrPullRequestAlreadyMerged
	}
	return err
}

func (g *Gateway) EnableAutoMerge(ctx context.Context, input EnableAutoMergeInput) error {
	strategy := strings.TrimSpace(string(input.Strategy))
	if strategy == "" {
		return fmt.Errorf("auto-merge strategy is required")
	}
	headSHA := strings.TrimSpace(input.HeadSHA)
	if headSHA == "" {
		return fmt.Errorf("auto-merge head SHA is required")
	}
	_, err := g.runGh(ctx, input.CWD, "", "pr", "merge", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--auto", "--"+strategy, "--match-head-commit", headSHA)
	return err
}

func (g *Gateway) GetPullRequestHeadSHA(ctx context.Context, input ViewPullRequestInput) (string, error) {
	result, err := g.runGh(ctx, input.CWD, "", "pr", "view", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--json", "headRefOid")
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	return asString(row["headRefOid"]), nil
}

// ValidateMergifyRouting checks the repository-owned Mergify contract before
// Gatekeeper publishes an auto-merge label. The repository file is the
// authority for how that label is consumed; Gatekeeper must fail closed when
// the file is absent or does not contain the vetoes that protect manual queue
// entries as well as the automatic route.
func (g *Gateway) ValidateMergifyRouting(ctx context.Context, input ValidateMergifyRoutingInput) error {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/contents/.mergify.yml", repo), "--jq", ".content", "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return fmt.Errorf("read .mergify.yml: %w", err)
	}
	encoded := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, result.Stdout)
	content, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode .mergify.yml: %w", err)
	}
	text := string(content)
	queueStart := strings.Index(text, "queue_conditions:")
	if queueStart < 0 {
		return fmt.Errorf(".mergify.yml has no queue_conditions")
	}
	queueEnd := strings.Index(text[queueStart+len("queue_conditions:"):], "\nmerge_protections:")
	if queueEnd < 0 {
		queueEnd = len(text) - queueStart - len("queue_conditions:")
	}
	queue := text[queueStart : queueStart+len("queue_conditions:")+queueEnd]
	for _, fragment := range []string{"- label != needs-human-review", "- label != do-not-merge"} {
		if !strings.Contains(queue, fragment) {
			return fmt.Errorf(".mergify.yml queue_conditions missing %q", fragment)
		}
	}
	if !strings.Contains(text, "merge_protections_settings:") || !strings.Contains(text, "label = auto-merge") {
		return fmt.Errorf(".mergify.yml has no auto-merge label contract")
	}
	return nil
}

func (g *Gateway) ResolveReviewThread(ctx context.Context, input ResolveReviewThreadInput) error {
	hostname, _ := splitRepoHostname(input.Repo)
	thread, err := g.getReviewThread(ctx, input.ThreadID, input.CWD, hostname)
	if err != nil {
		return err
	}
	if thread == nil {
		return &ReviewThreadNotFoundError{ThreadID: input.ThreadID}
	}
	if thread.IsResolved {
		return nil
	}
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"mutation($threadId: ID!) {",
		"  resolveReviewThread(input: { threadId: $threadId }) {",
		"    thread { id isResolved }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId=" + input.ThreadID}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return err
	}
	data, _ := row["data"].(map[string]any)
	resolveRow, _ := data["resolveReviewThread"].(map[string]any)
	threadRow, _ := resolveRow["thread"].(map[string]any)
	if threadRow == nil || !asBool(threadRow["isResolved"]) {
		return fmt.Errorf("failed to resolve review thread %s", input.ThreadID)
	}
	return nil
}

func (g *Gateway) ViewReviewThread(ctx context.Context, input ViewReviewThreadInput) (ReviewThread, error) {
	thread, err := g.getReviewThread(ctx, input.ThreadID, input.CWD, input.Hostname)
	if err != nil {
		return ReviewThread{}, err
	}
	if thread == nil {
		return ReviewThread{}, nil
	}
	out := ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved}
	cursor := ""
	for {
		nodes, nextCursor, hasNextPage, err := g.fetchReviewThreadCommentsPage(ctx, input.Hostname, input.CWD, input.ThreadID, cursor)
		if err != nil {
			return ReviewThread{}, err
		}
		out.Comments = appendReviewThreadComments(out.Comments, nodes)
		if !hasNextPage {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) ListReviewThreads(ctx context.Context, input ListReviewThreadsInput) ([]ReviewThread, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	limit := input.Limit
	if limit <= 0 {
		limit = 100
	}
	owner, name, err := parseRepo(repo)
	if err != nil {
		return nil, err
	}
	out := make([]ReviewThread, 0, min(limit, 100))
	threadsCursor := ""
	for len(out) < limit {
		pageSize := min(100, limit-len(out))
		nodes, nextCursor, hasNextPage, err := g.fetchReviewThreadPage(ctx, hostname, input.CWD, owner, name, input.PRNumber, pageSize, threadsCursor)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			threadRow, _ := node.(map[string]any)
			id := asString(threadRow["id"])
			if id == "" {
				continue
			}
			thread := ReviewThread{ID: id, IsResolved: asBool(threadRow["isResolved"]), Path: asString(threadRow["path"]), Line: asInt64(threadRow["line"])}
			commentsRow, _ := threadRow["comments"].(map[string]any)
			commentNodes, _ := commentsRow["nodes"].([]any)
			thread.Comments = appendReviewThreadComments(thread.Comments, commentNodes)
			commentPageInfo, _ := commentsRow["pageInfo"].(map[string]any)
			commentCursor := asString(commentPageInfo["endCursor"])
			for asBool(commentPageInfo["hasNextPage"]) && commentCursor != "" {
				moreComments, nextCommentCursor, hasMoreComments, err := g.fetchReviewThreadCommentsPage(ctx, hostname, input.CWD, id, commentCursor)
				if err != nil {
					return nil, err
				}
				thread.Comments = appendReviewThreadComments(thread.Comments, moreComments)
				commentPageInfo = map[string]any{"hasNextPage": hasMoreComments, "endCursor": nextCommentCursor}
				commentCursor = nextCommentCursor
			}
			if thread.URL == "" && len(thread.Comments) > 0 {
				thread.URL = thread.Comments[0].URL
			}
			out = append(out, thread)
			if len(out) == limit {
				break
			}
		}
		if !hasNextPage || nextCursor == "" {
			break
		}
		threadsCursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) AddReviewThreadReply(ctx context.Context, input AddReviewThreadReplyInput) error {
	if err := outboundguard.ValidateReviewThreadReply(input.Body, input.ThreadID); err != nil {
		return err
	}
	hostname, _ := splitRepoHostname(input.Repo)
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"mutation($threadId: ID!, $body: String!) {",
		"  addPullRequestReviewThreadReply(input: { pullRequestReviewThreadId: $threadId, body: $body }) {",
		"    comment { id }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId=" + input.ThreadID, "-f", "body=" + input.Body}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return err
	}
	data, _ := row["data"].(map[string]any)
	replyRow, _ := data["addPullRequestReviewThreadReply"].(map[string]any)
	commentRow, _ := replyRow["comment"].(map[string]any)
	if asString(commentRow["id"]) == "" {
		return fmt.Errorf("failed to add review thread reply %s", input.ThreadID)
	}
	return nil
}

// CompareCommits returns the GitHub compare API status between Base and Head
// (one of "identical", "ahead", "behind", "diverged"). It is used by the
// fixer to decide whether a previously-pushed fix commit is still reachable
// from the live PR head.
func (g *Gateway) CompareCommits(ctx context.Context, input CompareCommitsInput) (CompareCommitsResult, error) {
	if input.Repo == "" || input.Base == "" || input.Head == "" {
		return CompareCommitsResult{}, fmt.Errorf("compare commits requires repo, base, and head")
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/compare/%s...%s", repo, input.Base, input.Head)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return CompareCommitsResult{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return CompareCommitsResult{}, err
	}
	return CompareCommitsResult{Status: asString(row["status"])}, nil
}

func (g *Gateway) GetPullRequestDiff(ctx context.Context, input GetPullRequestDiffInput) (string, error) {
	result, err := g.runGhWithTimeout(ctx, input.CWD, "", prDiffGhCommandTimeout, "pr", "diff", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo)
	// Never treat a truncated local capture as a complete diff authority, and never
	// collapse it into GitHub's true oversized-diff signal.
	if result.StdoutTruncated {
		return "", ErrLocalCaptureTruncated
	}
	if err != nil {
		if isDiffTooLargeError(err) {
			return "", ErrDiffTooLarge
		}
		return "", err
	}
	return result.Stdout, nil
}

func (g *Gateway) SubmitReview(ctx context.Context, input SubmitReviewInput) error {
	request := g.reviewSubmitRequest(input)
	if err := validateReviewOutboundContent(input.Body, input.Comments); err != nil {
		// Always redact paths on validation_failed: pre-publish diagnostics must
		// not echo secret-shaped path fields even when the rejection reason is
		// not path-related (and for content-safety rejections of the path itself).
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": redactReviewSubmitRequestPaths(request), "error": err.Error()})
		return err
	}
	if marker, ok := findReviewIdempotencyMarker(input.Body, ""); ok && marker.Outcome == "clean" && len(input.Comments) > 0 {
		err := fmt.Errorf("clean review marker cannot be submitted with review comments")
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": redactReviewSubmitRequestPaths(request), "error": err.Error()})
		return err
	}
	var flags []reviewQualityFlag
	var processing reviewCommentProcessing
	input.Body, input.Comments, flags, processing = normalizeReviewAnchors(input.Body, input.Comments, input.Anchors)
	// Re-validate after normalization: FallbackBody / retarget prefixes embed agent path
	// into the top-level review body, which was not present in the pre-normalize fields.
	if err := validateReviewOutboundContent(input.Body, input.Comments); err != nil {
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": redactReviewSubmitRequestPaths(request), "error": err.Error()})
		return err
	}
	request = g.reviewSubmitRequest(input)
	request["comment_processing"] = map[string]any{
		"original_count":   processing.OriginalCount,
		"submitted_count":  processing.SubmittedCount,
		"normalized_count": processing.NormalizedCount,
		"downgraded_count": processing.DowngradedCount,
		"dropped_count":    processing.DroppedCount,
		"comments":         processing.Comments,
	}
	gateApplies, err := reviewQualityGateApplies(input.Event, input.Body)
	if err != nil {
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": redactReviewSubmitRequestPaths(request), "error": err.Error()})
		return err
	}
	if gateApplies && len(flags) > 0 {
		err := fmt.Errorf("review quality gate failed: %s", formatReviewQualityFlags(flags))
		g.emitReviewSubmitDiagnostic("github_review_submit_validation_failed", map[string]any{"request": redactReviewSubmitRequestPaths(request), "error": err.Error(), "quality_flags": reviewQualityFlagsSummary(flags)})
		return err
	}
	comments := input.Comments[:0]
	for _, comment := range input.Comments {
		comment.Body = normalizeInlineReviewDisclosure(comment.Body, input.Disclosure)
		if strings.TrimSpace(comment.Body) == "" {
			processing.DroppedCount++
			markReviewCommentDropped(processing.Comments, comment.DiagnosticIndex)
			continue
		}
		comments = append(comments, comment)
	}
	input.Comments = comments
	processing.SubmittedCount = len(input.Comments)
	request = g.reviewSubmitRequest(input)
	request["comment_processing"] = map[string]any{
		"original_count":   processing.OriginalCount,
		"submitted_count":  processing.SubmittedCount,
		"normalized_count": processing.NormalizedCount,
		"downgraded_count": processing.DowngradedCount,
		"dropped_count":    processing.DroppedCount,
		"comments":         processing.Comments,
	}
	if len(input.Comments) > 0 || strings.TrimSpace(input.CommitID) != "" {
		hostname, repo := splitRepoHostname(input.Repo)
		endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, input.PRNumber)
		payload := map[string]any{
			"event":     input.Event,
			"body":      emptyToNil(input.Body),
			"commit_id": emptyToNil(input.CommitID),
		}
		if len(input.Comments) > 0 {
			comments := make([]map[string]any, 0, len(input.Comments))
			for _, comment := range input.Comments {
				row := map[string]any{"body": comment.Body}
				if comment.Path != "" {
					row["path"] = comment.Path
				}
				if comment.Line > 0 {
					row["line"] = comment.Line
				}
				if comment.Side != "" {
					row["side"] = comment.Side
				}
				if comment.StartLine > 0 {
					row["start_line"] = comment.StartLine
				}
				if comment.StartSide != "" {
					row["start_side"] = comment.StartSide
				}
				comments = append(comments, row)
			}
			payload["comments"] = comments
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal gh review payload: %w", err)
		}
		request["method"] = "POST"
		request["endpoint"] = endpoint
		g.emitReviewSubmitDiagnostic("github_review_submit_prepared", map[string]any{"request": request})
		args := []string{"api", endpoint, "--method", "POST", "--input", "-", "--include"}
		if hostname != "" {
			args = append(args, "--hostname", hostname)
		}
		result, err := g.runGh(ctx, input.CWD, string(body), args...)
		if err != nil {
			fields := map[string]any{"request": request, "gh_stdout": reviewSubmitRawStdout(result.Stdout), "gh_stderr": result.Stderr, "gh_error": sanitizeReviewSubmitCommandError(err.Error())}
			if response, ok := parseReviewSubmitHTTPResponse(result.Stdout); ok {
				if response.StatusCode > 0 {
					fields["http_status"] = response.StatusCode
				}
				if response.Body != "" {
					fields["response_body"] = response.Body
				}
				if len(response.Headers) > 0 {
					fields["response_headers"] = response.Headers
				}
			}
			g.emitReviewSubmitDiagnostic("github_review_submit_failed", fields)
			return err
		}
		return err
	}
	args := []string{"pr", "review", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--" + strings.ToLower(strings.ReplaceAll(input.Event, "_", "-"))}
	if input.Body != "" {
		args = append(args, "--body", input.Body)
	}
	_, err = g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) emitReviewSubmitDiagnostic(event string, fields map[string]any) {
	if g.reviewSubmitDiagnostic != nil {
		g.reviewSubmitDiagnostic(event, fields)
	}
}

// validateReviewOutboundContent checks review body, inline comment bodies, and
// anchor paths. Paths are included because normalizeReviewAnchors can embed
// them into the published top-level body via FallbackBody / retarget prefixes,
// and kept comments publish path on the GitHub review API.
func validateReviewOutboundContent(body string, comments []ReviewComment) error {
	fields := []outboundguard.Field{{Name: "review body", Text: body}}
	for i, comment := range comments {
		fields = append(fields, outboundguard.Field{
			Name: fmt.Sprintf("inline review comment %d", i+1),
			Text: comment.Body,
		})
		if path := strings.TrimSpace(comment.Path); path != "" {
			fields = append(fields, outboundguard.Field{
				Name: fmt.Sprintf("inline review comment %d path", i+1),
				Text: path,
			})
		}
	}
	return outboundguard.Validate(fields...)
}

func (g *Gateway) reviewSubmitRequest(input SubmitReviewInput) map[string]any {
	_, repo := splitRepoHostname(input.Repo)
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, input.PRNumber)
	request := map[string]any{
		"repo":      input.Repo,
		"pr_number": input.PRNumber,
		"event":     input.Event,
		"commit_id": strings.TrimSpace(input.CommitID),
		"method":    "POST",
		"endpoint":  endpoint,
		"payload": map[string]any{
			"body_marker": reviewSubmitBodyMarkerSummary(input.Body),
			"comments":    reviewSubmitCommentsSummary(input.Comments),
		},
	}
	return request
}

func reviewSubmitBodyMarkerSummary(body string) map[string]any {
	if marker, ok := findReviewIdempotencyMarker(body, ""); ok {
		return map[string]any{"id": marker.ID, "head": marker.Head, "outcome": marker.Outcome}
	}
	return map[string]any{}
}

func reviewSubmitCommentsSummary(comments []ReviewComment) []map[string]any {
	return reviewSubmitCommentsSummaryWithPaths(comments, true)
}

func reviewSubmitCommentsSummaryWithPaths(comments []ReviewComment, includePaths bool) []map[string]any {
	summary := make([]map[string]any, 0, len(comments))
	for idx, comment := range comments {
		entry := map[string]any{"index": reviewCommentDiagnosticIndex(comment, idx)}
		for key, value := range reviewCommentAnchorMap(comment) {
			if key == "path" && !includePaths {
				if strings.TrimSpace(comment.Path) != "" {
					entry["path_present"] = true
				}
				continue
			}
			entry[key] = value
		}
		summary = append(summary, entry)
	}
	return summary
}

// redactReviewSubmitRequestPaths returns a diagnostic-safe request summary for
// validation_failed events. Raw comment paths and nested processing anchors are
// replaced with path_present so secret-shaped path values cannot leak into
// logs/stderr while publication is rejected.
func redactReviewSubmitRequestPaths(request map[string]any) map[string]any {
	if request == nil {
		return nil
	}
	sanitized := make(map[string]any, len(request))
	for key, value := range request {
		sanitized[key] = value
	}
	if payload, ok := sanitized["payload"].(map[string]any); ok {
		payloadCopy := make(map[string]any, len(payload))
		for key, value := range payload {
			payloadCopy[key] = value
		}
		payloadCopy["comments"] = redactDiagnosticCommentList(payloadCopy["comments"])
		sanitized["payload"] = payloadCopy
	}
	if processing, ok := sanitized["comment_processing"].(map[string]any); ok {
		processingCopy := make(map[string]any, len(processing))
		for key, value := range processing {
			processingCopy[key] = value
		}
		processingCopy["comments"] = redactDiagnosticProcessingCommentList(processingCopy["comments"])
		sanitized["comment_processing"] = processingCopy
	}
	return sanitized
}

func redactDiagnosticCommentList(raw any) any {
	switch comments := raw.(type) {
	case []map[string]any:
		redacted := make([]map[string]any, 0, len(comments))
		for _, entry := range comments {
			redacted = append(redacted, redactReviewCommentDiagnosticEntry(entry))
		}
		return redacted
	case []any:
		redacted := make([]map[string]any, 0, len(comments))
		for _, rawEntry := range comments {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				continue
			}
			redacted = append(redacted, redactReviewCommentDiagnosticEntry(entry))
		}
		return redacted
	default:
		return raw
	}
}

func redactDiagnosticProcessingCommentList(raw any) any {
	switch comments := raw.(type) {
	case []map[string]any:
		redacted := make([]map[string]any, 0, len(comments))
		for _, entry := range comments {
			redacted = append(redacted, redactProcessingCommentDiagnosticEntry(entry))
		}
		return redacted
	case []any:
		redacted := make([]map[string]any, 0, len(comments))
		for _, rawEntry := range comments {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				continue
			}
			redacted = append(redacted, redactProcessingCommentDiagnosticEntry(entry))
		}
		return redacted
	default:
		return raw
	}
}

func redactReviewCommentDiagnosticEntry(entry map[string]any) map[string]any {
	if entry == nil {
		return nil
	}
	out := make(map[string]any, len(entry))
	for key, value := range entry {
		if key == "path" {
			if path, ok := value.(string); ok && strings.TrimSpace(path) != "" {
				out["path_present"] = true
			}
			continue
		}
		out[key] = value
	}
	return out
}

func redactProcessingCommentDiagnosticEntry(entry map[string]any) map[string]any {
	if entry == nil {
		return nil
	}
	out := make(map[string]any, len(entry))
	for key, value := range entry {
		switch key {
		case "original_anchor", "final_anchor":
			if anchor, ok := value.(map[string]any); ok {
				out[key] = redactReviewCommentDiagnosticEntry(anchor)
				continue
			}
		}
		out[key] = value
	}
	return out
}

func reviewCommentDiagnosticIndex(comment ReviewComment, fallback int) int {
	if comment.DiagnosticIndex != 0 || fallback == 0 {
		return comment.DiagnosticIndex
	}
	return fallback
}

func reviewQualityFlagsSummary(flags []reviewQualityFlag) []map[string]any {
	summary := make([]map[string]any, 0, len(flags))
	for _, flag := range flags {
		summary = append(summary, map[string]any{"kind": flag.Kind, "detail": flag.Detail})
	}
	return summary
}

func reviewSubmitRawStdout(raw string) string {
	if response, ok := parseReviewSubmitHTTPResponse(raw); ok && response.Body != "" {
		return response.Body
	}
	return raw
}

func sanitizeReviewSubmitCommandError(message string) string {
	message = strings.TrimSpace(message)
	if head, _, ok := strings.Cut(message, "\nstdout:"); ok {
		return strings.TrimSpace(head)
	}
	return message
}

type reviewSubmitHTTPResponse struct {
	StatusCode int
	Headers    map[string]any
	Body       string
}

func parseReviewSubmitHTTPResponse(raw string) (reviewSubmitHTTPResponse, bool) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	parts := strings.SplitN(raw, "\n\n", 2)
	if len(parts) != 2 {
		return reviewSubmitHTTPResponse{}, false
	}
	headLines := strings.Split(parts[0], "\n")
	if len(headLines) == 0 || !strings.HasPrefix(headLines[0], "HTTP/") {
		return reviewSubmitHTTPResponse{}, false
	}
	response := reviewSubmitHTTPResponse{Headers: map[string]any{}, Body: strings.TrimSpace(parts[1])}
	fields := strings.Fields(headLines[0])
	if len(fields) >= 2 {
		if status, err := strconv.Atoi(fields[1]); err == nil {
			response.StatusCode = status
		}
	}
	for _, line := range headLines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		normalizedKey := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		switch normalizedKey {
		case "x_github_request_id", "x_ratelimit_limit", "x_ratelimit_remaining", "x_ratelimit_reset", "retry_after", "content_type":
			response.Headers[normalizedKey] = strings.TrimSpace(value)
		}
	}
	return response, true
}

func normalizeInlineReviewDisclosure(body string, disclosureCfg config.DisclosureConfig) string {
	if !hasInlineReviewDisclosure(body) {
		return body
	}
	if !disclosureCfg.Enabled || !disclosureCfg.Channels.ReviewComment {
		return stripInlineReviewDisclosure(body)
	}
	if disclosureCfg.Channels.InlineCommentVisible || !containsVisibleInlineReviewDisclosure(body) {
		return body
	}
	cleaned := disclosure.StripMarkdownStamp(body)
	if strings.TrimSpace(cleaned) == "" {
		return disclosure.Marker
	}
	return strings.TrimRight(cleaned, "\n") + "\n\n" + disclosure.Marker
}

func stripInlineReviewDisclosure(body string) string {
	if !hasInlineReviewDisclosure(body) {
		return body
	}
	return disclosure.StripMarkdownStamp(body)
}

func hasInlineReviewDisclosure(body string) bool {
	return strings.Contains(body, disclosure.Marker) || containsVisibleInlineReviewDisclosure(body)
}

func containsVisibleInlineReviewDisclosure(body string) bool {
	return strings.Contains(body, "Generated by Looper") ||
		strings.Contains(body, "Powered by Looper") ||
		strings.Contains(body, "Generated by "+disclosure.RepoLinkHTML) ||
		strings.Contains(body, "Powered by "+disclosure.RepoLinkHTML) ||
		strings.Contains(body, "Generated by [Looper]("+disclosure.RepoURL+")") ||
		strings.Contains(body, "Powered by [Looper]("+disclosure.RepoURL+")") ||
		strings.Contains(body, "Generated by [Looper]("+disclosure.CurrentRepoURL+")") ||
		strings.Contains(body, "Powered by [Looper]("+disclosure.CurrentRepoURL+")") ||
		strings.Contains(body, `Generated by <a href="`+disclosure.CurrentRepoURL+`">Looper</a>`) ||
		strings.Contains(body, `Powered by <a href="`+disclosure.CurrentRepoURL+`">Looper</a>`) ||
		strings.Contains(body, "Generated by [Looper]("+disclosure.UpstreamRepoURL+")") ||
		strings.Contains(body, "Powered by [Looper]("+disclosure.UpstreamRepoURL+")") ||
		strings.Contains(body, `Generated by <a href="`+disclosure.UpstreamRepoURL+`">Looper</a>`) ||
		strings.Contains(body, `Powered by <a href="`+disclosure.UpstreamRepoURL+`">Looper</a>`) ||
		strings.Contains(body, "Generated by [Looper]("+disclosure.LegacyRepoURL+")") ||
		strings.Contains(body, "Powered by [Looper]("+disclosure.LegacyRepoURL+")")
}

func (g *Gateway) AddPullRequestComment(ctx context.Context, input PullRequestCommentInput) error {
	if err := outboundguard.Validate(outboundguard.Field{Name: "pull request comment body", Text: input.Body}); err != nil {
		return err
	}
	_, err := g.runGh(ctx, input.CWD, "", "pr", "comment", fmt.Sprintf("%d", input.PRNumber), "--repo", input.Repo, "--body", input.Body)
	return err
}

func (g *Gateway) HasReviewMarker(ctx context.Context, input VerifyReviewMarkerInput) (bool, error) {
	marker, err := g.FindReviewMarker(ctx, input)
	if err != nil {
		return false, err
	}
	return marker.Found, nil
}

func (g *Gateway) FindReviewMarker(ctx context.Context, input VerifyReviewMarkerInput) (ReviewMarkerResult, error) {
	if strings.TrimSpace(input.Marker) == "" {
		return ReviewMarkerResult{}, nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, input.PRNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	reviewsResult, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return ReviewMarkerResult{}, err
	}
	marker := findAllowedReviewMarker(reviewsResult.Stdout, input.Marker, input.AllowedReviewEvents, input.AuthorLogin, input.AllowCleanComment)
	if marker.Found && marker.ReviewID != "" && !input.SkipInlineComments {
		comments, err := g.fetchReviewCommentBodies(ctx, input.Repo, input.PRNumber, marker.ReviewID, input.CWD)
		if err != nil {
			return ReviewMarkerResult{}, err
		}
		marker.InlineCommentBodies = comments
	}
	return marker, nil
}

func (g *Gateway) fetchReviewCommentBodies(ctx context.Context, repo string, prNumber int64, reviewID string, cwd string) ([]string, error) {
	hostname, repoSlug := splitRepoHostname(repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/reviews/%s/comments", repoSlug, prNumber, reviewID)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	bodies := make([]string, 0, len(rows))
	for _, row := range rows {
		if body := asString(row["body"]); strings.TrimSpace(body) != "" {
			bodies = append(bodies, body)
		}
	}
	return bodies, nil
}

func findAllowedReviewMarker(raw string, marker string, allowedReviewEvents []string, authorLogin string, allowCleanComment bool) ReviewMarkerResult {
	expectedAuthorLogin := normalizeReviewMarkerLogin(authorLogin)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		var pages [][]map[string]any
		if err := json.Unmarshal([]byte(raw), &pages); err != nil {
			if parsedMarker, ok := findReviewIdempotencyMarker(raw, marker); expectedAuthorLogin == "" && len(allowedReviewEvents) == 0 && ok {
				return ReviewMarkerResult{Found: true, Outcome: parsedMarker.Outcome, Body: raw}
			}
			return ReviewMarkerResult{}
		}
		for _, page := range pages {
			rows = append(rows, page...)
		}
	}
	var newest ReviewMarkerResult
	for _, row := range rows {
		body, ok := row["body"].(string)
		if !ok {
			continue
		}
		parsedMarker, ok := findReviewIdempotencyMarker(body, marker)
		if !ok {
			continue
		}
		author := reviewAuthorLogin(row)
		if expectedAuthorLogin != "" && normalizeReviewMarkerLogin(author) != expectedAuthorLogin {
			continue
		}
		event := reviewEventFromStateString(row["state"])
		if reviewMarkerEventAllowedForOutcome(parsedMarker.Outcome, event, allowedReviewEvents, allowCleanComment) {
			newest = ReviewMarkerResult{Found: true, Outcome: parsedMarker.Outcome, Event: event, AuthorLogin: author, Body: body, ReviewID: reviewIDString(row)}
		}
	}
	return newest
}

func reviewMarkerEventAllowedForOutcome(outcome string, event string, allowedReviewEvents []string, allowCleanComment bool) bool {
	if event == "" {
		return false
	}
	if len(allowedReviewEvents) == 0 {
		return true
	}
	if !reviewEventAllowed(event, allowedReviewEvents) {
		return false
	}
	switch outcome {
	case "clean":
		if allowCleanComment && event == "COMMENT" {
			return true
		}
		if reviewEventAllowed("APPROVE", allowedReviewEvents) {
			return event == "APPROVE"
		}
		return event == "COMMENT"
	case "blocking":
		if reviewEventAllowed("REQUEST_CHANGES", allowedReviewEvents) {
			return event == "REQUEST_CHANGES"
		}
		return event == "COMMENT"
	case "non_blocking", "actionable":
		return event == "COMMENT"
	default:
		return false
	}
}

func reviewIDString(row map[string]any) string {
	if id := asString(row["id"]); id != "" {
		return id
	}
	if id := asInt64(row["id"]); id != 0 {
		return fmt.Sprintf("%d", id)
	}
	return ""
}

func reviewAuthorLogin(row map[string]any) string {
	user, _ := row["user"].(map[string]any)
	login, _ := user["login"].(string)
	return login
}

func normalizeReviewMarkerLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

type reviewIdempotencyMarker struct {
	ID      string
	Head    string
	Outcome string
}

var reviewMarkerRE = regexp.MustCompile(`<!--\s*looper:review\s+([^>]*)-->`)

func findReviewIdempotencyMarker(body string, marker string) (reviewIdempotencyMarker, bool) {
	marker = strings.TrimSpace(marker)
	for _, parsedMarker := range parseReviewIdempotencyMarkers(body) {
		if marker == "" || parsedMarker.matches(marker) {
			return parsedMarker, true
		}
	}
	return reviewIdempotencyMarker{}, false
}

func parseReviewIdempotencyMarkers(body string) []reviewIdempotencyMarker {
	matches := reviewMarkerRE.FindAllStringSubmatch(body, -1)
	markers := make([]reviewIdempotencyMarker, 0, len(matches))
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		fields := parseReviewMarkerFields(match[1])
		parsedMarker := reviewIdempotencyMarker{ID: fields["id"], Head: fields["head"], Outcome: fields["outcome"]}
		if parsedMarker.ID == "" || parsedMarker.Head == "" || !isValidReviewMarkerOutcome(parsedMarker.Outcome) {
			continue
		}
		markers = append(markers, parsedMarker)
	}
	return markers
}

func isValidReviewMarkerOutcome(outcome string) bool {
	switch outcome {
	case "clean", "non_blocking", "blocking", "actionable":
		return true
	default:
		return false
	}
}

func parseReviewMarkerFields(segment string) map[string]string {
	fields := map[string]string{}
	for _, field := range strings.Fields(segment) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	return fields
}

func (m reviewIdempotencyMarker) matches(marker string) bool {
	fields := parseReviewMarkerFields(strings.TrimPrefix(marker, "looper:review"))
	if id := fields["id"]; id != "" && id != m.ID {
		return false
	}
	if idPrefix := fields["id_prefix"]; idPrefix != "" && !strings.HasPrefix(m.ID, idPrefix) {
		return false
	}
	if head := fields["head"]; head != "" && head != m.Head {
		return false
	}
	if outcome := fields["outcome"]; outcome != "" && outcome != m.Outcome {
		return false
	}
	return strings.HasPrefix(marker, "looper:review") || strings.Contains(marker, "id=") || strings.Contains(marker, "head=") || strings.Contains(marker, "outcome=")
}

func reviewEventFromStateString(raw any) string {
	state, _ := raw.(string)
	return reviewEventFromState(state)
}

func reviewEventAllowed(event string, allowedReviewEvents []string) bool {
	if event == "" {
		return false
	}
	for _, allowed := range allowedReviewEvents {
		if strings.EqualFold(event, allowed) {
			return true
		}
	}
	return false
}

func reviewEventFromState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED":
		return "APPROVE"
	case "COMMENTED":
		return "COMMENT"
	case "CHANGES_REQUESTED":
		return "REQUEST_CHANGES"
	default:
		return ""
	}
}

func (g *Gateway) AddPullRequestReaction(ctx context.Context, input PullRequestReactionInput) error {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/reactions", repo, input.PRNumber), "--method", "POST", "-H", "Accept: application/vnd.github+json", "-f", "content=" + input.Content}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) RemovePullRequestReaction(ctx context.Context, input PullRequestReactionInput) error {
	currentLogin, err := g.GetCurrentUserLoginForRepo(ctx, input.Repo, input.CWD)
	if err != nil {
		return err
	}
	if currentLogin == "" {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/reactions", repo, input.PRNumber), "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return err
	}
	for _, row := range rows {
		reaction, ok := normalizeReaction(row)
		if !ok {
			continue
		}
		if reaction.Content != input.Content || !strings.EqualFold(reaction.UserLogin, currentLogin) {
			continue
		}
		deleteArgs := []string{"api", fmt.Sprintf("repos/%s/issues/%d/reactions/%d", repo, input.PRNumber, reaction.ID), "--method", "DELETE", "-H", "Accept: application/vnd.github+json"}
		if hostname != "" {
			deleteArgs = append(deleteArgs, "--hostname", hostname)
		}
		if _, err := g.runGh(ctx, input.CWD, "", deleteArgs...); err != nil {
			return err
		}
	}
	return nil
}

func (g *Gateway) AddPullRequestLabels(ctx context.Context, input PullRequestLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	if err := g.ensureLabelsExist(ctx, input.Repo, input.Labels, input.CWD); err != nil {
		return err
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels", repo, input.PRNumber), "--method", "POST"}
	for _, label := range input.Labels {
		args = append(args, "-f", "labels[]="+label)
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) RemovePullRequestLabels(ctx context.Context, input PullRequestLabelsInput) error {
	if len(input.Labels) == 0 {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	for _, label := range input.Labels {
		args := []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels/%s", repo, input.PRNumber, encodeURIComponent(label)), "--method", "DELETE"}
		if hostname != "" {
			args = append(args, "--hostname", hostname)
		}
		_, err := g.runGh(ctx, input.CWD, "", args...)
		if err == nil {
			continue
		}
		if isMissingPullRequestLabelDelete(err) {
			continue
		}
		return err
	}
	return nil
}

func (g *Gateway) AddPullRequestReviewers(ctx context.Context, input PullRequestReviewersInput) error {
	if len(input.Reviewers) == 0 {
		return nil
	}
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/pulls/%d/requested_reviewers", repo, input.PRNumber), "--method", "POST"}
	for _, reviewer := range input.Reviewers {
		args = append(args, "-f", "reviewers[]="+reviewer)
	}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

type ReviewSummary struct {
	ID     int64
	State  string // "APPROVED" | "CHANGES_REQUESTED" | "COMMENTED" | "DISMISSED"
	Author string
	// IsBot is GitHub's own classification of the reviewer's account, taken from
	// the REST `user.type` this endpoint returns. It is read here rather than
	// derived from the login because the login alone cannot answer it: REST
	// spells a bot `coderabbitai[bot]` while the GraphQL projection `gh pr view`
	// returns spells the same account `coderabbitai`, so a `[bot]` suffix test
	// reports app reviewers as humans wherever the GraphQL shape is in play.
	IsBot bool
	Body  string
}

// ListPullRequestReviews returns a PR's submitted reviews (REST), with the numeric
// review id needed to dismiss one.
func (g *Gateway) ListPullRequestReviews(ctx context.Context, input ViewPullRequestInput) ([]ReviewSummary, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, input.PRNumber)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArrayOrPages(result.Stdout)
	if err != nil {
		return nil, err
	}
	reviews := make([]ReviewSummary, 0, len(rows))
	for _, row := range rows {
		author := firstNonNil(row["user"], row["author"])
		reviews = append(reviews, ReviewSummary{
			ID:     asInt64(firstNonNil(row["id"], row["databaseId"])),
			State:  asString(row["state"]),
			Body:   asString(row["body"]),
			Author: extractAuthor(author),
			IsBot:  extractAuthorIsBot(author),
		})
	}
	return reviews, nil
}

// UpdatePullRequestBranch merges the base branch into the PR head via GitHub's
// native update-branch (the clean, no-conflict case of keeping a PR current).
// Returns a conflict error when the merge is not clean, so the caller can fall
// back to an agent-driven resolve. Idempotent-ish: a 422 "already up to date" is
// treated as success.
func (g *Gateway) UpdatePullRequestBranch(ctx context.Context, input ViewPullRequestInput) error {
	hostname, repo := splitRepoHostname(input.Repo)
	args := []string{"api", fmt.Sprintf("repos/%s/pulls/%d/update-branch", repo, input.PRNumber), "--method", "PUT", "-H", "Accept: application/vnd.github+json"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "merge conflict") || strings.Contains(msg, "not mergeable") {
			return &MergeConflictError{Repo: input.Repo, PRNumber: input.PRNumber}
		}
		if strings.Contains(err.Error(), "already up to date") {
			return nil
		}
	}
	return err
}

// MergeConflictError signals that a base merge cannot be done cleanly and needs an
// agent to resolve conflicts.
type MergeConflictError struct {
	Repo     string
	PRNumber int64
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("pull request %s#%d has merge conflicts with its base", e.Repo, e.PRNumber)
}

type DismissReviewInput struct {
	Repo     string
	PRNumber int64
	ReviewID int64
	Message  string
	CWD      string
}

// DismissReview dismisses a submitted review (e.g. an unreasonable
// CHANGES_REQUESTED) with an explanation, so it stops blocking the PR.
func (g *Gateway) DismissReview(ctx context.Context, input DismissReviewInput) error {
	hostname, repo := splitRepoHostname(input.Repo)
	message := strings.TrimSpace(input.Message)
	if message == "" {
		message = "Dismissed by looper."
	}
	if err := outboundguard.Validate(outboundguard.Field{Name: "review dismissal message", Text: message}); err != nil {
		return err
	}
	args := []string{"api", fmt.Sprintf("repos/%s/pulls/%d/reviews/%d/dismissals", repo, input.PRNumber, input.ReviewID), "--method", "PUT", "-f", "message=" + message, "-f", "event=DISMISS"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	_, err := g.runGh(ctx, input.CWD, "", args...)
	return err
}

func (g *Gateway) CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (CreatePullRequestResult, error) {
	if err := outboundguard.Validate(
		outboundguard.Field{Name: "pull request title", Text: input.Title},
		outboundguard.Field{Name: "pull request body", Text: input.Body},
	); err != nil {
		return CreatePullRequestResult{}, err
	}
	args := []string{"pr", "create", "--repo", input.Repo, "--head", input.HeadBranch, "--base", input.BaseBranch, "--title", input.Title, "--body", input.Body}
	if input.Draft {
		args = append(args, "--draft")
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return CreatePullRequestResult{}, err
	}
	prURL := strings.TrimSpace(result.Stdout)
	if prURL == "" {
		return CreatePullRequestResult{}, &shell.CommandExecutionError{Message: "gh pr create returned an empty URL", Result: result}
	}
	return CreatePullRequestResult{Number: parsePRNumberFromURL(prURL), URL: prURL}, nil
}

func (g *Gateway) CompareBranches(ctx context.Context, input CompareBranchesInput) (CompareBranchesResult, error) {
	hostname, repo := splitRepoHostname(input.Repo)
	comparison := encodeURIComponent(input.BaseBranch) + "..." + encodeURIComponent(input.HeadBranch)
	args := []string{"api", fmt.Sprintf("repos/%s/compare/%s", repo, comparison)}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, input.CWD, "", args...)
	if err != nil {
		return CompareBranchesResult{}, err
	}
	var payload struct {
		AheadBy      int    `json:"ahead_by"`
		BehindBy     int    `json:"behind_by"`
		Status       string `json:"status"`
		TotalCommits int    `json:"total_commits"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &payload); err != nil {
		return CompareBranchesResult{}, err
	}
	return CompareBranchesResult{AheadBy: payload.AheadBy, BehindBy: payload.BehindBy, Status: payload.Status, TotalCommits: payload.TotalCommits}, nil
}

func (g *Gateway) UpdatePullRequestTitle(ctx context.Context, input UpdatePullRequestTitleInput) error {
	if err := outboundguard.Validate(outboundguard.Field{Name: "pull request title", Text: input.Title}); err != nil {
		return err
	}
	_, err := g.runGh(ctx, input.CWD, "", "pr", "edit", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--title", input.Title)
	return err
}

func (g *Gateway) UpdatePullRequestBody(ctx context.Context, input UpdatePullRequestBodyInput) error {
	if err := outboundguard.Validate(outboundguard.Field{Name: "pull request body", Text: input.Body}); err != nil {
		return err
	}
	_, err := g.runGh(ctx, input.CWD, "", "pr", "edit", strconv.FormatInt(input.PRNumber, 10), "--repo", input.Repo, "--body", input.Body)
	return err
}

func (g *Gateway) IsAuthenticated(ctx context.Context, cwd, hostname string) (bool, error) {
	args := []string{"auth", "status"}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", strings.TrimSpace(hostname))
	}
	_, err := g.runGh(ctx, cwd, "", args...)
	if err == nil {
		return true, nil
	}
	var commandErr *shell.CommandExecutionError
	if errors.As(err, &commandErr) && commandErr.Category == shell.FailureNonZeroExit {
		return false, nil
	}
	return false, err
}

func (g *Gateway) GetCurrentUserIdentity(ctx context.Context, cwd string) (CurrentUserIdentity, error) {
	result, err := g.runGh(ctx, cwd, "", "api", "user", "--jq", `{login: .login, id: .id}`)
	if err != nil {
		if isUserLoginUnsupportedForCurrentToken(err) {
			login, viewErr := g.getViewerLogin(ctx, cwd, "")
			if viewErr != nil {
				return CurrentUserIdentity{}, viewErr
			}
			return CurrentUserIdentity{Login: login}, nil
		}
		return CurrentUserIdentity{}, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return CurrentUserIdentity{}, err
	}
	return CurrentUserIdentity{Login: strings.TrimSpace(asString(row["login"])), NumericID: asInt64(row["id"])}, nil
}

func (g *Gateway) GetCurrentUserLoginForRepo(ctx context.Context, repo string, cwd string) (string, error) {
	if snapshot := discoverySnapshotFromContext(ctx); snapshot != nil {
		return snapshot.getCurrentUserLoginForRepo(ctx, repo, cwd)
	}
	return g.getCurrentUserLoginForRepoRaw(ctx, repo, cwd)
}

func (g *Gateway) getCurrentUserLoginForRepoRaw(ctx context.Context, repo string, cwd string) (string, error) {
	hostname, _ := splitRepoHostname(repo)
	args := []string{"api", "user", "--jq", ".login"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		if isUserLoginUnsupportedForCurrentToken(err) {
			return g.getViewerLogin(ctx, cwd, hostname)
		}
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func (g *Gateway) getViewerLogin(ctx context.Context, cwd string, hostname string) (string, error) {
	args := []string{"api", "graphql", "-f", `query=query { viewer { login } }`}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	data, _ := row["data"].(map[string]any)
	viewer, _ := data["viewer"].(map[string]any)
	return strings.TrimSpace(asString(viewer["login"])), nil
}

func isUserLoginUnsupportedForCurrentToken(err error) bool {
	var commandErr *shell.CommandExecutionError
	if !errors.As(err, &commandErr) {
		return false
	}
	combined := strings.ToLower(strings.TrimSpace(strings.Join([]string{commandErr.Error(), commandErr.Result.Stdout, commandErr.Result.Stderr}, "\n")))
	return strings.Contains(combined, "resource not accessible by integration")
}

func (g *Gateway) DetectCurrentRepository(ctx context.Context, cwd string) (string, error) {
	result, err := g.runGh(ctx, cwd, "", "repo", "view", "--json", "nameWithOwner,url")
	if err != nil {
		return "", err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return "", err
	}
	repo := hostQualifiedRepo(asString(row["nameWithOwner"]), asString(row["url"]))
	if err := validateGitHubRepoSlug(repo); err != nil {
		return "", err
	}
	return repo, nil
}

// ensureLabels creates every definition the repository does not already have.
//
// Create-only, always. Looper needs a label to exist; it has no claim on how a
// maintainer has worded one that already does.
//
// The requested definitions are the authority. Listing is drift detection —
// it exists only to skip creates that would fail anyway — so a failed list
// must not block the caller: nothing is then known to exist, every create is
// attempted, and one that loses to an existing label is tolerated exactly as a
// lost race is. Any other create failure stops immediately because the caller's
// label application will not happen.
func (g *Gateway) ensureLabels(ctx context.Context, repo, cwd string, definitions []labels.Definition) error {
	if len(definitions) == 0 {
		return nil
	}

	existing, listErr := g.listRepositoryLabels(ctx, repo, cwd)
	if listErr != nil {
		existing = nil
	}

	for _, definition := range definitions {
		if _, exists := existing[labels.Normalize(definition.Name)]; exists {
			continue
		}
		if _, err := g.runGh(ctx, cwd, "", "label", "create", definition.Name, "--repo", repo, "--color", definition.Color, "--description", definition.Description); err != nil && !isLabelAlreadyExistsError(err) {
			return fmt.Errorf("create label %s: %w", definition.Name, err)
		}
	}
	return nil
}

func (g *Gateway) CapturePullRequestSnapshot(ctx context.Context, input CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	transportRepo := strings.TrimSpace(input.TransportRepo)
	if transportRepo == "" {
		transportRepo = input.Repo
	}
	detail, err := g.ViewPullRequestForReviewer(ctx, ViewPullRequestInput{Repo: transportRepo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, err
	}
	diff, err := g.GetPullRequestDiff(ctx, GetPullRequestDiffInput{Repo: transportRepo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		if !errors.Is(err, ErrDiffTooLarge) && !errors.Is(err, ErrLocalCaptureTruncated) {
			return storage.PullRequestSnapshotRecord{}, err
		}
	}
	capturedAt := strings.TrimSpace(input.CapturedAt)
	if capturedAt == "" {
		capturedAt = g.now().UTC().Format(javaScriptISOStringLayout)
	}
	payloadMap := map[string]any{"detail": detail, "diff": diff}
	if errors.Is(err, ErrDiffTooLarge) {
		payloadMap["diffTruncated"] = true
		payloadMap["diffTruncationReason"] = DiffTruncationReasonGitHubTooLarge
	}
	if errors.Is(err, ErrLocalCaptureTruncated) {
		payloadMap["diffTruncated"] = true
		payloadMap["diffTruncationReason"] = DiffTruncationReasonLocalCapture
	}
	payload, err := json.Marshal(payloadMap)
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, fmt.Errorf("marshal pull request snapshot payload: %w", err)
	}
	unresolvedCount := int64(countUnresolvedThreads(detail.Comments))
	return storage.PullRequestSnapshotRecord{
		ID:                    randomID(),
		ProjectID:             input.ProjectID,
		Repo:                  input.Repo,
		PRNumber:              input.PRNumber,
		HeadSHA:               valueOr(detail.HeadSHA, "unknown"),
		BaseSHA:               stringPtrIfNotEmpty(detail.BaseSHA),
		Title:                 stringPtrIfNotEmpty(detail.Title),
		Body:                  stringPtrIfNotEmpty(detail.Body),
		Author:                stringPtrIfNotEmpty(detail.Author),
		DiffRef:               stringPtr(fmt.Sprintf("gh:pr-diff:%s:%d", input.Repo, input.PRNumber)),
		ChecksSummary:         stringPtrIfNotEmpty(summarizeChecks(detail.Checks)),
		UnresolvedThreadCount: &unresolvedCount,
		ReviewState:           stringPtrIfNotEmpty(detail.ReviewDecision),
		PayloadJSON:           stringPtr(string(payload)),
		CapturedAt:            capturedAt,
		CreatedAt:             capturedAt,
	}, nil
}

type reviewThreadNode struct {
	ID         string
	IsResolved bool
}

type githubReaction struct {
	ID        int64
	Content   string
	UserLogin string
}

func (g *Gateway) fetchReviewThreads(ctx context.Context, repo string, prNumber int64, cwd string) ([]map[string]any, error) {
	hostname, repoOnly := splitRepoHostname(repo)
	owner, name, err := parseRepo(repoOnly)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 100)
	cursor := ""
	for {
		nodes, nextCursor, hasNextPage, err := g.fetchReviewThreadsSummaryPage(ctx, hostname, cwd, owner, name, prNumber, cursor)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			normalized, ok := normalizeReviewThread(node)
			if ok {
				threadRow, _ := node.(map[string]any)
				commentsRow, _ := threadRow["comments"].(map[string]any)
				commentNodes, _ := commentsRow["nodes"].([]any)
				pageInfo, _ := commentsRow["pageInfo"].(map[string]any)
				commentCursor := asString(pageInfo["endCursor"])
				hasMoreComments := asBool(pageInfo["hasNextPage"])
				allCommentNodes := append([]any(nil), commentNodes...)
				for hasMoreComments {
					moreComments, nextCommentCursor, hasMore, err := g.fetchReviewThreadCommentsPage(ctx, hostname, cwd, asString(normalized["threadId"]), commentCursor)
					if err != nil {
						return nil, err
					}
					allCommentNodes = append(allCommentNodes, moreComments...)
					commentCursor = nextCommentCursor
					hasMoreComments = hasMore
				}
				if fingerprint := reviewThreadFingerprintFromNodes(allCommentNodes); fingerprint != "" {
					normalized["threadFingerprint"] = fingerprint
				}
				if result, key := fixerResultFromReviewThreadNodes(allCommentNodes); result != "" {
					normalized["fixerResult"] = result
					normalized["fixerAttemptKey"] = key
				}
				out = append(out, normalized)
			}
		}
		if !hasNextPage || nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	return out, nil
}

func (g *Gateway) fetchReviewThreadPage(ctx context.Context, hostname, cwd, owner, name string, prNumber int64, limit int, cursor string) ([]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($owner: String!, $name: String!, $prNumber: Int!, $limit: Int!, $after: String) {",
		"  repository(owner: $owner, name: $name) {",
		"    pullRequest(number: $prNumber) {",
		"      reviewThreads(first: $limit, after: $after) {",
		"        nodes {",
		"          id isResolved path line",
		"          comments(first: 100) {",
		"            nodes {",
		"              id body createdAt updatedAt path line url authorAssociation",
		"              author { login }",
		"              originalCommit { oid }",
		"              commit { oid }",
		"            }",
		"            pageInfo { hasNextPage endCursor }",
		"          }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "owner=" + owner, "-F", "name=" + name, "-F", fmt.Sprintf("prNumber=%d", prNumber), "-F", fmt.Sprintf("limit=%d", limit)}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", hostname)
	}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	return decodeReviewThreadsResponse(result.Stdout)
}

func (g *Gateway) fetchReviewThreadsSummaryPage(ctx context.Context, hostname, cwd, owner, name string, prNumber int64, cursor string) ([]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($owner: String!, $name: String!, $prNumber: Int!, $after: String) {",
		"  repository(owner: $owner, name: $name) {",
		"    pullRequest(number: $prNumber) {",
		"      reviewThreads(first: 100, after: $after) {",
		"        nodes {",
		"          id",
		"          isResolved",
		"          path",
		"          line",
		"          comments(first: 100) {",
		"            nodes { id body updatedAt url path line authorAssociation author { login } }",
		"            pageInfo { hasNextPage endCursor }",
		"          }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "owner=" + owner, "-F", "name=" + name, "-F", fmt.Sprintf("prNumber=%d", prNumber)}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", hostname)
	}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	return decodeReviewThreadsResponse(result.Stdout)
}

func (g *Gateway) fetchReviewThreadCommentsPage(ctx context.Context, hostname, cwd, threadID, cursor string) ([]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($threadId: ID!, $after: String) {",
		"  node(id: $threadId) {",
		"    ... on PullRequestReviewThread {",
		"      comments(first: 100, after: $after) {",
		"        nodes {",
		"          id body createdAt updatedAt path line url authorAssociation",
		"          author { login }",
		"          originalCommit { oid }",
		"          commit { oid }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId=" + threadID}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", hostname)
	}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, "", false, err
	}
	data, _ := row["data"].(map[string]any)
	node, _ := data["node"].(map[string]any)
	commentsRow, _ := node["comments"].(map[string]any)
	nodes, _ := commentsRow["nodes"].([]any)
	pageInfo, _ := commentsRow["pageInfo"].(map[string]any)
	return nodes, asString(pageInfo["endCursor"]), asBool(pageInfo["hasNextPage"]), nil
}

func decodeReviewThreadsResponse(stdout string) ([]any, string, bool, error) {
	row, err := decodeJSONObject(stdout)
	if err != nil {
		return nil, "", false, err
	}
	data, _ := row["data"].(map[string]any)
	repoRow, _ := data["repository"].(map[string]any)
	prRow, _ := repoRow["pullRequest"].(map[string]any)
	threadsRow, _ := prRow["reviewThreads"].(map[string]any)
	nodes, _ := threadsRow["nodes"].([]any)
	pageInfo, _ := threadsRow["pageInfo"].(map[string]any)
	return nodes, asString(pageInfo["endCursor"]), asBool(pageInfo["hasNextPage"]), nil
}

func (g *Gateway) fetchLinkedPullRequestsPage(ctx context.Context, cwd, hostname, owner, repo string, issueNumber int64, cursor string) ([]map[string]any, string, bool, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($owner: String!, $repo: String!, $number: Int!, $after: String) {",
		"  repository(owner: $owner, name: $repo) {",
		"    issue(number: $number) {",
		"      closedByPullRequestsReferences(first: 20, after: $after) {",
		"        nodes {",
		"          number",
		"          state",
		"          url",
		"          repository { nameWithOwner }",
		"          mergedAt",
		"          mergeCommit { oid }",
		"        }",
		"        pageInfo { hasNextPage endCursor }",
		"      }",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "owner=" + owner, "-F", "repo=" + repo, "-F", fmt.Sprintf("number=%d", issueNumber)}
	if cursor != "" {
		args = append(args, "-F", "after="+cursor)
	}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, "", false, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, "", false, err
	}
	data, _ := row["data"].(map[string]any)
	repository, _ := data["repository"].(map[string]any)
	issue, _ := repository["issue"].(map[string]any)
	refs, _ := issue["closedByPullRequestsReferences"].(map[string]any)
	nodesAny, _ := refs["nodes"].([]any)
	nodes := make([]map[string]any, 0, len(nodesAny))
	for _, node := range nodesAny {
		nodeRow, _ := node.(map[string]any)
		if nodeRow == nil {
			continue
		}
		nodes = append(nodes, nodeRow)
	}
	pageInfo, _ := refs["pageInfo"].(map[string]any)
	return nodes, asString(pageInfo["endCursor"]), asBool(pageInfo["hasNextPage"]), nil
}

func appendReviewThreadComments(dst []ReviewThreadComment, nodes []any) []ReviewThreadComment {
	for _, commentNode := range nodes {
		commentRow, _ := commentNode.(map[string]any)
		commentID := asString(commentRow["id"])
		if commentID == "" {
			continue
		}
		dst = append(dst, ReviewThreadComment{ID: commentID, Body: asString(commentRow["body"]), Author: extractAuthor(commentRow["author"]), AuthorAssociation: asString(commentRow["authorAssociation"]), CreatedAt: asString(commentRow["createdAt"]), UpdatedAt: asString(commentRow["updatedAt"]), Path: asString(commentRow["path"]), Line: asInt64(commentRow["line"]), OriginalCommitOID: extractOID(commentRow["originalCommit"]), CommitOID: extractOID(commentRow["commit"]), URL: asString(commentRow["url"])})
	}
	return dst
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (g *Gateway) getReviewThread(ctx context.Context, threadID, cwd string, hostname string) (*reviewThreadNode, error) {
	args := []string{"api", "graphql", "-f", "query=" + strings.Join([]string{
		"query($threadId: ID!) {",
		"  node(id: $threadId) {",
		"    ... on PullRequestReviewThread {",
		"      id",
		"      isResolved",
		"    }",
		"  }",
		"}",
	}, "\n"), "-F", "threadId=" + threadID}
	if strings.TrimSpace(hostname) != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return nil, err
	}
	row, err := decodeJSONObject(result.Stdout)
	if err != nil {
		return nil, err
	}
	data, _ := row["data"].(map[string]any)
	node, _ := data["node"].(map[string]any)
	id := asString(node["id"])
	if id == "" {
		return nil, nil
	}
	return &reviewThreadNode{ID: id, IsResolved: asBool(node["isResolved"])}, nil
}

// ensureLabelsExist creates any label about to be applied that the repository
// does not have yet, so that applying a label to a fresh repository does not
// fail on a missing label.
//
// Presentation comes from labels.Standard when the label is one Looper owns,
// and falls back to a neutral default for anything else — a project may
// configure its own trigger labels, and those have no entry in the table.
func (g *Gateway) ensureLabelsExist(ctx context.Context, repo string, wanted []string, cwd string) error {
	definitions := make([]labels.Definition, 0, len(wanted))
	seen := map[string]struct{}{}
	for _, label := range wanted {
		normalized := labels.Normalize(label)
		if normalized == "" {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		definitions = append(definitions, labelPresentation(label))
	}
	return g.ensureLabels(ctx, repo, cwd, definitions)
}

// isLabelAlreadyExistsError reports whether a `gh label create` failure is the
// duplicate-label outcome real gh produces without --force. The label already
// exists, so the caller can treat it as success rather than aborting a label
// application that lost a create race.
func isLabelAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
}

// labelPresentation resolves the color and description to create a label with.
func labelPresentation(label string) labels.Definition {
	normalized := labels.Normalize(label)
	for _, definition := range labels.Standard() {
		if labels.Normalize(definition.Name) == normalized {
			return definition
		}
	}
	return labels.Definition{Name: label, Color: "5319e7", Description: "Managed by looper"}
}

func (g *Gateway) listRepositoryLabels(ctx context.Context, repo string, cwd string) (map[string]labels.Definition, error) {
	result, err := g.runGh(ctx, cwd, "", "label", "list", "--repo", repo, "--limit", "1000", "--json", "name,color,description")
	if err != nil {
		return nil, err
	}
	rows, err := decodeJSONArray(result.Stdout)
	if err != nil {
		return nil, err
	}
	out := make(map[string]labels.Definition, len(rows))
	for _, row := range rows {
		name := strings.TrimSpace(asString(row["name"]))
		if name == "" {
			continue
		}
		out[strings.ToLower(name)] = labels.Definition{Name: name, Color: normalizeLabelColor(asString(row["color"])), Description: strings.TrimSpace(asString(row["description"]))}
	}
	return out, nil
}

func (g *Gateway) runGh(ctx context.Context, cwd, stdin string, args ...string) (shell.Result, error) {
	return g.runGhWithTimeout(ctx, cwd, stdin, defaultGhCommandTimeout, args...)
}

// mergeIntoProcessEnv materializes a full child environment from the parent
// process environment plus overrides. shell.Options.Env replaces the child
// environment wholesale, so a partial map would strip PATH/HOME from gh.
// Returns nil when there is nothing to override, which keeps inheritance.
func mergeIntoProcessEnv(overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		return nil
	}
	merged := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		merged[key] = value
	}
	for key, value := range overrides {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		merged[key] = value
	}
	return merged
}

func (g *Gateway) runGhWithTimeout(ctx context.Context, cwd, stdin string, timeout time.Duration, args ...string) (shell.Result, error) {
	result, err := g.ghRun(ctx, shell.Options{Command: g.ghPath, Args: args, CWD: valueOr(strings.TrimSpace(cwd), g.cwd), Env: g.ghEnv, Stdin: stdin, Timeout: timeout})
	if result.StdoutTruncated || result.StderrTruncated {
		streams := make([]string, 0, 2)
		if result.StdoutTruncated {
			streams = append(streams, fmt.Sprintf("stdout after %d bytes", len(result.Stdout)))
		}
		if result.StderrTruncated {
			streams = append(streams, fmt.Sprintf("stderr after %d bytes", len(result.Stderr)))
		}
		message := "GitHub command output truncated: " + strings.Join(streams, ", ")
		if err != nil {
			message += "; command error: " + err.Error()
		}
		return result, &shell.CommandExecutionError{Message: message, Result: result}
	}
	if err != nil && isTransientGitHubMessage(strings.Join([]string{err.Error(), result.Stdout, result.Stderr}, "\n")) {
		return result, &TransientError{Err: err}
	}
	return result, err
}

func isDiffTooLargeError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "http 406") || strings.Contains(message, "too_large") || strings.Contains(message, "diff exceeded maximum number of lines")
}

func defaultLimit(limit int) int {
	if limit <= 0 {
		return 30
	}
	return limit
}

func withoutJSONField(fields []string, field string) []string {
	out := make([]string, 0, len(fields))
	for _, candidate := range fields {
		if candidate != field {
			out = append(out, candidate)
		}
	}
	return out
}

func parseRepo(repo string) (string, string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid GitHub repo: %s", repo)
	}
	return parts[0], parts[1], nil
}

func validateGitHubRepoSlug(repo string) error {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if (len(parts) != 2 && len(parts) != 3) || strings.TrimSpace(parts[len(parts)-2]) == "" || strings.TrimSpace(parts[len(parts)-1]) == "" {
		return fmt.Errorf("invalid GitHub repo: %s", repo)
	}
	if len(parts) == 3 && strings.TrimSpace(parts[0]) == "" {
		return fmt.Errorf("invalid GitHub repo: %s", repo)
	}
	return nil
}

func dependencyRepo(value any, repositoryURL string, fallback string) string {
	repository, _ := value.(map[string]any)
	if fullName := strings.TrimSpace(asString(repository["full_name"])); fullName != "" {
		return hostQualifiedRepo(fullName, firstNonEmpty(repositoryURL, asString(repository["url"])))
	}
	if apiURL := strings.TrimSpace(firstNonEmpty(repositoryURL, asString(repository["url"]))); apiURL != "" {
		if parsed, err := url.Parse(apiURL); err == nil {
			trimmed := strings.Trim(parsed.Path, "/")
			if index := strings.Index(trimmed, "repos/"); index >= 0 {
				nameWithOwner := strings.TrimPrefix(trimmed[index:], "repos/")
				if parsed.Hostname() != "" && parsed.Hostname() != "api.github.com" && parsed.Hostname() != "github.com" {
					return parsed.Hostname() + "/" + nameWithOwner
				}
				return nameWithOwner
			}
		}
	}
	return strings.TrimSpace(fallback)
}

func hostQualifiedRepo(nameWithOwner string, repoURL string) string {
	repo := strings.TrimSpace(nameWithOwner)
	parsed, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || parsed.Hostname() == "" || parsed.Hostname() == "github.com" || parsed.Hostname() == "api.github.com" {
		return repo
	}
	return parsed.Hostname() + "/" + repo
}

// SplitRepoHostname exposes the enterprise-host split so callers building URLs
// for a repository do not have to assume github.com.
func SplitRepoHostname(repo string) (hostname, slug string) {
	return splitRepoHostname(repo)
}

func splitRepoHostname(repo string) (string, string) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) == 3 && strings.TrimSpace(parts[0]) != "" {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]) + "/" + strings.TrimSpace(parts[2])
	}
	return "", strings.TrimSpace(repo)
}

func summarizeChecks(checks []map[string]any) string {
	if len(checks) == 0 {
		return ""
	}
	states := make([]string, 0, len(checks))
	for _, check := range checks {
		state := asString(check["conclusion"])
		if state == "" {
			state = asString(check["state"])
		}
		if state == "" {
			state = "unknown"
		}
		states = append(states, state)
	}
	return strings.Join(states, ", ")
}

func countUnresolvedThreads(comments []map[string]any) int {
	count := 0
	for _, comment := range comments {
		state := asString(comment["state"])
		if state == "" {
			if value, ok := comment["isResolved"].(bool); ok {
				state = fmt.Sprintf("%t", value)
			}
		}
		if state != "RESOLVED" && state != "true" {
			count++
		}
	}
	return count
}

func normalizeReviewThread(value any) (map[string]any, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	threadID := asString(row["id"])
	if threadID == "" {
		return nil, false
	}
	commentsRow, _ := row["comments"].(map[string]any)
	nodes, _ := commentsRow["nodes"].([]any)
	var first map[string]any
	if len(nodes) > 0 {
		first, _ = nodes[0].(map[string]any)
	}
	commentID := threadID
	if id := asString(first["id"]); id != "" {
		commentID = id
	}
	isResolved := asBool(row["isResolved"])
	out := map[string]any{
		"id":         commentID,
		"threadId":   threadID,
		"source":     "review_thread",
		"state":      ternary(isResolved, "RESOLVED", "UNRESOLVED"),
		"isResolved": isResolved,
	}
	if body := asString(first["body"]); body != "" {
		out["body"] = body
	}
	if author := extractAuthor(first["author"]); author != "" {
		out["author"] = author
	}
	if url := asString(first["url"]); url != "" {
		out["url"] = url
	}
	if path := asString(first["path"]); path != "" {
		out["path"] = path
	} else if path := asString(row["path"]); path != "" {
		out["path"] = path
	}
	if line := asInt64(first["line"]); line > 0 {
		out["line"] = line
	} else if line := asInt64(row["line"]); line > 0 {
		out["line"] = line
	}
	if fingerprint := reviewThreadFingerprintFromNodes(nodes); fingerprint != "" {
		out["threadFingerprint"] = fingerprint
	}
	return out, true
}

// fixerResultFromReviewThreadNodes projects only explicit Looper reply
// markers into the reviewer/fixer handoff. Visible prose is never treated as
// a fixer outcome; the marker's commit/fingerprint is the attempt identity so
// retries do not increment a convergence budget by re-observing one result.
func fixerResultFromReviewThreadNodes(nodes []any) (string, string) {
	for _, raw := range nodes {
		row, _ := raw.(map[string]any)
		body := asString(row["body"])
		if match := fixerDeclinedEvidencePattern.FindStringSubmatch(body); len(match) == 2 {
			return "declined", match[1]
		}
		if match := fixerDeferredEvidencePattern.FindStringSubmatch(body); len(match) == 2 {
			return "deferred", match[1]
		}
		if match := fixerReplyEvidencePattern.FindStringSubmatch(body); len(match) == 2 {
			return "fixed", match[1]
		}
	}
	return "", ""
}

func reviewThreadFingerprintFromNodes(nodes []any) string {
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		comment, _ := node.(map[string]any)
		if comment == nil {
			continue
		}
		if strings.Contains(asString(comment["body"]), "<!-- looper-fixer-reply ") {
			continue
		}
		id := strings.TrimSpace(asString(comment["id"]))
		updatedAt := strings.TrimSpace(asString(comment["updatedAt"]))
		if id == "" && updatedAt == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s@%s", id, updatedAt))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "|")
}

func validateCloseIssueStateReason(value string) (string, error) {
	normalized := strings.TrimSpace(value)
	switch normalized {
	case "completed", "not_planned":
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid issue close reason %q", value)
	}
}

func (g *Gateway) viewIssueState(ctx context.Context, repo string, issueNumber int64, cwd string) (string, error) {
	hostname, repoSlug := splitRepoHostname(repo)
	args := []string{"api", fmt.Sprintf("repos/%s/issues/%d", repoSlug, issueNumber), "--jq", ".state"}
	if hostname != "" {
		args = append(args, "--hostname", hostname)
	}
	result, err := g.runGh(ctx, cwd, "", args...)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(result.Stdout)), nil
}

func (g *Gateway) viewPullRequestDraft(ctx context.Context, repo string, prNumber int64, cwd string) (bool, error) {
	result, err := g.runGh(ctx, cwd, "", "pr", "view", strconv.FormatInt(prNumber, 10), "--repo", repo, "--json", "isDraft", "--jq", ".isDraft")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(result.Stdout), "true"), nil
}

func (g *Gateway) viewPullRequestState(ctx context.Context, repo string, prNumber int64, cwd string) (string, error) {
	result, err := g.runGh(ctx, cwd, "", "pr", "view", strconv.FormatInt(prNumber, 10), "--repo", repo, "--json", "state", "--jq", ".state")
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(result.Stdout)), nil
}

func normalizeReaction(value any) (githubReaction, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		return githubReaction{}, false
	}
	id := asInt64(row["id"])
	content := asString(row["content"])
	userLogin := extractAuthor(row["user"])
	if id <= 0 || content == "" || userLogin == "" {
		return githubReaction{}, false
	}
	return githubReaction{ID: id, Content: content, UserLogin: userLogin}, true
}

func extractDependencyIssue(value map[string]any, defaultRepo string) DependencyIssue {
	repositoryURL := asString(value["repository_url"])
	repo := extractIssueRepository(value["repository"])
	repo = completeIssueRepository(repo, repositoryURL, defaultRepo)
	return DependencyIssue{
		ID:            asInt64(value["id"]),
		Number:        asInt64(value["number"]),
		Title:         asString(value["title"]),
		URL:           asString(value["url"]),
		HTMLURL:       asString(value["html_url"]),
		RepositoryURL: repositoryURL,
		State:         asString(value["state"]),
		StateReason:   asString(value["state_reason"]),
		Repository:    repo,
	}
}

func completeIssueRepository(repo IssueRepository, repositoryURL string, defaultRepo string) IssueRepository {
	fullName, name := parseRepositoryIdentity(repositoryURL)
	if fullName == "" {
		fullName = strings.TrimSpace(defaultRepo)
		_, fallbackRepo := splitRepoHostname(fullName)
		_, name = splitRepoOwnerName(fallbackRepo)
	}
	if repo.Name == "" {
		repo.Name = name
	}
	if repo.FullName == "" {
		repo.FullName = fullName
	}
	if repo.URL == "" {
		repo.URL = repositoryURL
	}
	return repo
}

func parseRepositoryIdentity(repositoryURL string) (fullName string, name string) {
	parsed, err := url.Parse(strings.TrimSpace(repositoryURL))
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "repos":
		parts = parts[:3]
	case len(parts) >= 5 && parts[0] == "api" && parts[1] == "v3" && parts[2] == "repos":
		parts = parts[2:5]
	default:
		return "", ""
	}
	fullName = parts[1] + "/" + parts[2]
	if hostname := strings.TrimSpace(parsed.Hostname()); hostname != "" && hostname != "github.com" && hostname != "api.github.com" {
		fullName = hostname + "/" + fullName
	}
	return fullName, parts[2]
}

func extractIssueRepository(value any) IssueRepository {
	row, _ := value.(map[string]any)
	if row == nil {
		return IssueRepository{}
	}
	return IssueRepository{
		Name:     asString(row["name"]),
		FullName: asString(row["full_name"]),
		URL:      asString(row["url"]),
		HTMLURL:  asString(row["html_url"]),
	}
}

func normalizeLabelColor(value string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "#")
}

func decodeJSONObject(value string) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil, invalidJSONError(value, err)
	}
	if out == nil {
		return map[string]any{}, nil
	}
	return out, nil
}

func decodeJSONArray(value string) ([]map[string]any, error) {
	var out []map[string]any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil, invalidJSONError(value, err)
	}
	if out == nil {
		return []map[string]any{}, nil
	}
	return out, nil
}

func decodeJSONArrayOrPages(value string) ([]map[string]any, error) {
	rows, err := decodeJSONArray(value)
	if err == nil {
		return rows, nil
	}
	var pages [][]map[string]any
	if pageErr := json.Unmarshal([]byte(value), &pages); pageErr != nil {
		return nil, err
	}
	for _, page := range pages {
		rows = append(rows, page...)
	}
	if rows == nil {
		return []map[string]any{}, nil
	}
	return rows, nil
}

func decodeJSONObjects(value string) ([]map[string]any, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	rows := make([]map[string]any, 0)
	for {
		var item any
		if err := decoder.Decode(&item); err != nil {
			if errors.Is(err, io.EOF) {
				return rows, nil
			}
			return nil, invalidJSONError(value, err)
		}
		switch typed := item.(type) {
		case map[string]any:
			rows = append(rows, typed)
		case []any:
			for _, entry := range typed {
				row, ok := entry.(map[string]any)
				if !ok {
					return nil, invalidJSONError(value, fmt.Errorf("expected JSON object in array, got %T", entry))
				}
				rows = append(rows, row)
			}
		default:
			return nil, invalidJSONError(value, fmt.Errorf("expected JSON object or array, got %T", item))
		}
	}
}

func invalidJSONError(stdout string, err error) error {
	message := "Invalid gh JSON payload"
	errText := ""
	if err != nil {
		errText = err.Error()
		message += ": " + errText
	}
	message += fmt.Sprintf("; stdoutBytes=%d", len(stdout))
	if sample := summarizeInvalidJSONPayload(stdout); sample != "" {
		message += "; stdoutSample=" + strconv.Quote(sample)
	}
	return &shell.CommandExecutionError{Message: message, Result: shell.Result{ExitCode: 0, Stdout: stdout, Stderr: errText}}
}

func summarizeInvalidJSONPayload(stdout string) string {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return ""
	}
	stdout = strings.Join(strings.Fields(stdout), " ")
	const maxSampleBytes = 240
	if len(stdout) <= maxSampleBytes {
		return stdout
	}
	return strings.TrimSpace(stdout[:maxSampleBytes]) + "…"
}

func toObjectSlice(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func extractCommentInfos(value any) []CommentInfo {
	items := toObjectSlice(value)
	if len(items) == 0 {
		if rows, ok := value.([]map[string]any); ok {
			items = append(items, rows...)
		}
	}
	out := make([]CommentInfo, 0, len(items))
	for _, row := range items {
		author := firstNonNil(row["author"], row["user"])
		out = append(out, CommentInfo{
			ID:                asInt64(firstNonNil(row["id"], row["databaseId"])),
			Author:            extractAuthor(author),
			IsBot:             extractAuthorIsBot(author),
			AuthorAssociation: firstNonEmpty(asString(row["authorAssociation"]), asString(row["author_association"])),
			Body:              asString(row["body"]),
			CreatedAt:         firstNonEmpty(asString(row["createdAt"]), asString(row["created_at"])),
			UpdatedAt:         firstNonEmpty(asString(row["updatedAt"]), asString(row["updated_at"])),
			URL:               firstNonEmpty(asString(row["url"]), asString(row["html_url"])),
		})
	}
	return out
}

func splitRepoOwnerName(repo string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(repo), "/", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(repo), ""
	}
	return parts[0], parts[1]
}

func digNodes(row map[string]any, path ...string) []map[string]any {
	current := any(row)
	for _, key := range path {
		object, _ := current.(map[string]any)
		current = object[key]
	}
	object, _ := current.(map[string]any)
	return toObjectSlice(object["nodes"])
}

func extractAuthor(value any) string {
	row, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if login := asString(row["login"]); login != "" {
		return login
	}
	return asString(row["name"])
}

// extractAuthorIsBot reads GitHub's own classification of an account: REST spells
// it `type: "Bot"`, and gh's GraphQL projections spell it `is_bot: true`.
func extractAuthorIsBot(value any) bool {
	row, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(asString(row["type"])), "Bot") {
		return true
	}
	return asBool(row["is_bot"])
}

func extractActorType(value any) string {
	row, _ := value.(map[string]any)
	return asString(row["type"])
}
func extractOID(value any) string {
	row, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return asString(row["oid"])
}

func extractReviewRequestLogins(value any) []string {
	users := extractReviewRequestUsers(value)
	if users == nil {
		return nil
	}
	out := make([]string, 0, len(users))
	for _, user := range users {
		if user.Login != "" {
			out = append(out, user.Login)
		}
	}
	return out
}

func extractReviewRequestUsers(value any) []GitHubUser {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]GitHubUser, 0, len(items))
	for _, item := range items {
		if user, ok := extractReviewRequestUser(item); ok {
			out = append(out, user)
		}
	}
	return out
}

func extractReviewRequestUser(value any) (GitHubUser, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		return GitHubUser{}, false
	}
	if asString(row["__typename"]) == "User" {
		login := asString(row["login"])
		if login == "" {
			return GitHubUser{}, false
		}
		return GitHubUser{Login: login, ID: asInt64(firstNonNil(row["databaseId"], row["id"]))}, true
	}
	reviewer, _ := row["requestedReviewer"].(map[string]any)
	if asString(reviewer["__typename"]) == "User" {
		login := asString(reviewer["login"])
		if login == "" {
			return GitHubUser{}, false
		}
		return GitHubUser{Login: login, ID: asInt64(firstNonNil(reviewer["databaseId"], reviewer["id"]))}, true
	}
	return GitHubUser{}, false
}

func extractActorLogins(value any) []string {
	users := extractActorUsers(value)
	out := make([]string, 0, len(users))
	for _, user := range users {
		if user.Login != "" {
			out = append(out, user.Login)
		}
	}
	return out
}

func extractActorUsers(value any) []GitHubUser {
	items, ok := value.([]any)
	if !ok {
		return []GitHubUser{}
	}
	out := make([]GitHubUser, 0, len(items))
	for _, item := range items {
		if login := extractAuthor(item); login != "" {
			row, _ := item.(map[string]any)
			out = append(out, GitHubUser{Login: login, ID: asInt64(firstNonNil(row["databaseId"], row["id"]))})
		}
	}
	return out
}

func extractLabelNames(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name := asString(row["name"]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func extractLabelNamesFromConnection(value any) []string {
	row, _ := value.(map[string]any)
	if row == nil {
		return []string{}
	}
	return extractLabelNames(row["nodes"])
}

func normalizeGitHubLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func parsePRNumberFromURL(value string) int64 {
	match := prNumberURLPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0
	}
	return asInt64(match[1])
}

func isMissingPullRequestLabelDelete(err error) bool {
	commandErr, ok := err.(*shell.CommandExecutionError)
	if !ok {
		return false
	}
	output := strings.ToLower(commandErr.Result.Stdout + "\n" + commandErr.Result.Stderr)
	return strings.Contains(output, "404") && strings.Contains(output, "label")
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func asBool(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func boolPtrFromValue(value any) *bool {
	typed, ok := value.(bool)
	if !ok {
		return nil
	}
	return &typed
}

func asInt64(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		result, _ := typed.Int64()
		return result
	case string:
		var result int64
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &result)
		return result
	default:
		return 0
	}
}

func randomID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func encodeURIComponent(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func stringPtr(value string) *string { return &value }

func stringPtrIfNotEmpty(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func emptyToNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func nestedString(value map[string]any, path ...string) string {
	current := any(value)
	for _, part := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = next[part]
	}
	return asString(current)
}

func nestedInt64(value map[string]any, path ...string) int64 {
	current := any(value)
	for _, part := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return 0
		}
		current = next[part]
	}
	return asInt64(current)
}

func extractAutoMerge(value any) *PullRequestAutoMerge {
	row, ok := value.(map[string]any)
	if !ok || row == nil {
		return nil
	}
	return &PullRequestAutoMerge{
		EnabledBy:   extractAuthor(firstNonNil(row["enabled_by"], row["enabledBy"])),
		MergeMethod: firstNonEmpty(asString(row["merge_method"]), asString(row["mergeMethod"])),
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, strings.TrimSpace(value))
	}
	return out
}

func uniqueRequiredCheckRules(values []RequiredCheckRule) []RequiredCheckRule {
	seen := map[string]struct{}{}
	out := make([]RequiredCheckRule, 0, len(values))
	for _, value := range values {
		value.Context = strings.TrimSpace(value.Context)
		if value.Context == "" {
			continue
		}
		key := fmt.Sprintf("%s\x00%d", strings.ToLower(value.Context), value.AppID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func ternary[T any](condition bool, truthy, falsy T) T {
	if condition {
		return truthy
	}
	return falsy
}
