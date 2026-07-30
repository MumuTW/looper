package worker

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/bootstrap"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/disclosure"
	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/specpr"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/lifecycle"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/loops/failureclass"
	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/network/protocol"
	"github.com/MumuTW/looper/internal/networkpolicy"
	"github.com/MumuTW/looper/internal/processcontainment"
	"github.com/MumuTW/looper/internal/reproducer"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/validation"
	"github.com/MumuTW/looper/internal/worker/workflow"
	"github.com/MumuTW/looper/internal/workgraphdispatch"
	"github.com/MumuTW/looper/internal/worktreesafety"
)

const (
	stepPrepareWork     = workflow.StepPrepareWork
	stepPrepareWorktree = workflow.StepPrepareWorktree
	stepPlan            = workflow.StepPlan
	stepExecute         = workflow.StepExecute
	stepValidate        = workflow.StepValidate
	stepOpenPR          = workflow.StepOpenPR

	defaultAgentTimeout     = time.Hour
	defaultClaimTTL         = 10 * time.Minute
	defaultRetryDelay       = 5 * time.Second
	defaultRetryMax         = 3
	defaultIssueLimit       = 30
	personalIssueQueryLimit = 100

	workerBranchSlugMaxLength        = 30
	workerBranchSlugMaxWords         = 5
	workerBranchHashLength           = 16
	workerPRDedupeLookupLimit        = 1000
	maxPublicIssueClaimSummaryLength = 240
)

var (
	workerANSIEscapePattern              = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	workerStructuredResultMessagePattern = regexp.MustCompile(`^Worker completed without a valid structured result \(parse status: ([a-z_]+)\)\. See Looper logs for details\.$`)
	// ErrSuccessfulClaimFinalization prevents generic failure recovery from
	// requeueing externally completed work when the atomic terminal write fails.
	ErrSuccessfulClaimFinalization = errors.New("worker successful claim finalization failed")
)

// WorkerStep aliases the extracted workflow authority's step type; the
// pipeline order and resume decisions live in internal/worker/workflow.
type WorkerStep = workflow.Step

type PullRequestSummary struct {
	Number      int64
	URL         string
	State       string
	HeadRefName string
	BaseRefName string
}

type IssueSummary struct {
	Number                       int64
	Title                        string
	Body                         string
	URL                          string
	Author                       string
	Assignees                    []string
	AssigneeUsers                []networkpolicy.GitHubUser
	Labels                       []string
	PersonalAssigneeAutoAssigned bool
}

type PullRequestDetail struct {
	Number             int64
	Title              string
	Body               string
	URL                string
	State              string
	HeadRefName        string
	BaseRefName        string
	HeadSHA            string
	Labels             []string
	ReviewRequests     []string
	ReviewRequestUsers []networkpolicy.GitHubUser
}

type IssueDetail struct {
	Number        int64
	Title         string
	Body          string
	URL           string
	State         string
	IsPullRequest bool
	AssigneeUsers []networkpolicy.GitHubUser
	Labels        []string
}

type IssueCommentInput struct {
	Repo            string
	IssueNumber     int64
	Body            string
	CWD             string
	DisclosureAgent string
	DisclosureModel string
}

type IssueAssigneesInput struct {
	Repo        string
	IssueNumber int64
	Assignees   []string
	CWD         string
}

type IssueCommentResult struct {
	ID  int64
	URL string
}

type UpdateIssueCommentInput struct {
	Repo            string
	CommentID       int64
	Body            string
	CWD             string
	DisclosureAgent string
	DisclosureModel string
}

type CreatePullRequestInput struct {
	Repo            string
	HeadBranch      string
	BaseBranch      string
	Title           string
	Body            string
	Draft           bool
	CWD             string
	DisclosureAgent string
	DisclosureModel string
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
	Repo            string
	PRNumber        int64
	Body            string
	CWD             string
	DisclosureAgent string
	DisclosureModel string
}

type PullRequestLabelsInput struct {
	Repo     string
	PRNumber int64
	Labels   []string
	CWD      string
}

type PullRequestReviewersInput struct {
	Repo      string
	PRNumber  int64
	Reviewers []string
	CWD       string
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type ViewIssueInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}

type ListOpenPullRequestsInput struct {
	Repo  string
	CWD   string
	Limit int
	Label string
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

type GitHubGateway interface {
	ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error)
	ListOpenIssues(context.Context, ListOpenIssuesInput) ([]IssueSummary, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	ViewIssue(context.Context, ViewIssueInput) (IssueDetail, error)
	GetCurrentUserLogin(context.Context, string, string) (string, error)
	AddIssueAssignees(context.Context, IssueAssigneesInput) error
	CreateIssueComment(context.Context, IssueCommentInput) (IssueCommentResult, error)
	UpdateIssueComment(context.Context, UpdateIssueCommentInput) error
	CreatePullRequest(context.Context, CreatePullRequestInput) (CreatePullRequestResult, error)
	CompareBranches(context.Context, CompareBranchesInput) (CompareBranchesResult, error)
	UpdatePullRequestBody(context.Context, UpdatePullRequestBodyInput) error
	UpdatePullRequestTitle(context.Context, UpdatePullRequestTitleInput) error
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	RemovePullRequestLabels(context.Context, PullRequestLabelsInput) error
	AddPullRequestReviewers(context.Context, PullRequestReviewersInput) error
}

type CreateWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	Branch            string
	BaseBranch        string
	PRNumber          int64
	ProtectedBranches []string
	CheckoutMode      string
}

type CreateWorktreeResult struct {
	WorktreePath string
	Branch       string
	BaseBranch   string
	HeadSHA      string
	WorktreeID   string
}

type PrepareWorktreeInput struct {
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	Branch          string
	ExpectedHeadSHA string
	Remote          string
}

type PrepareWorktreeResult struct {
	HeadSHA string
	Clean   bool
}

type PushInput struct {
	RepoPath              string
	WorktreeRoot          string
	WorktreePath          string
	Branch                string
	Remote                string
	LocalHeadSHA          string
	ExpectedRemoteHeadSHA string
	ProtectedBranches     []string
}

type InspectHeadInput struct {
	RepoPath       string
	WorktreeRoot   string
	WorktreePath   string
	BaseRef        string
	ContentPaths   []string
	CompareHeadSHA string
}

type InspectHeadResult struct {
	HeadSHA               string
	Branch                string
	NewCommitSHAs         []string
	HasUncommittedChanges bool
	ChangedFiles          []string
	StagedFiles           []string
	UntrackedFiles        []string
	DiffFingerprint       string
	ContentFingerprint    string
	HeadDescendsFromCompare bool
}

type VerifyWorktreeIdentityInput struct {
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	ExpectedBranch  string
	ExpectedHeadSHA string
	CheckoutMode    string
}

type CommitInput struct {
	RepoPath        string
	WorktreeRoot    string
	WorktreePath    string
	Message         string
	DisclosureAgent string
	DisclosureModel string
}

type CommitResult struct{ CommitSHA string }

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	PrepareWorktree(context.Context, PrepareWorktreeInput) (PrepareWorktreeResult, error)
	InspectHead(context.Context, InspectHeadInput) (InspectHeadResult, error)
	VerifyWorktreeIdentity(context.Context, VerifyWorktreeIdentityInput) error
	Commit(context.Context, CommitInput) (CommitResult, error)
	Push(context.Context, PushInput) error
}

type AgentRunInput struct {
	ExecutionID         string
	ProjectID           string
	LoopID              string
	RunID               string
	Prompt              string
	NativeResumePrompt  string
	NativeSessionID     string
	WorkingDirectory    string
	Timeout             time.Duration
	HeartbeatTimeout    time.Duration
	Metadata            map[string]any
	IdempotencyKey      string
	RestrictToolNetwork bool
	OnBeforeTimeout     func(context.Context, agent.TimeoutObservation) error
	// UseSnapshot + SnapshotVendor/Model override the executor config for this
	// start when the run has a durable agent snapshot (execution authority).
	UseSnapshot    bool
	SnapshotVendor string
	SnapshotModel  *string
}

type AgentResult struct {
	Status                       string
	Summary                      string
	Stdout                       string
	Stderr                       string
	ParseStatus                  string
	ChangedFiles                 []string
	Commits                      []string
	Lifecycle                    *lifecycle.State
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
	PreTimeoutError              string
}

const validationGatedLocalOnlyPrompt = `

VALIDATION-GATED LOCAL-ONLY EXECUTION:
Looper's daemon already fetched and prepared the repository state. Tool network access is intentionally disabled. Do not run gh, git fetch/pull/push, access remote URLs, or fail merely because the forge is unavailable. Use the local checkout and the context already supplied in this prompt. Edit, test, and commit locally only; Looper will validate the exact commit and publish it afterward.`

const testFirstDeliveryPrompt = `TEST-FIRST DELIVERY EXPECTATION:
When feasible, add or update a failing automated test first and confirm it captures the requested behavior. Then write the smallest implementation that makes it pass, and refactor while keeping the suite green.
Every pull request must state the automated coverage added or updated and the relevant commands run. If automated coverage is not feasible, explain why coverage is not feasible in the final summary so the PR records that exception.
Repository CI checks must be green before merge.`

type AgentExecution interface {
	Wait(context.Context) (AgentResult, error)
	Kill(string) error
}

type AgentExecutor interface {
	Start(context.Context, AgentRunInput) (AgentExecution, error)
}

type ValidationResult struct {
	Passed          bool                       `json:"passed"`
	Summary         string                     `json:"summary,omitempty"`
	Output          string                     `json:"output,omitempty"`
	HeadSHA         string                     `json:"headSha,omitempty"`
	FailureCategory validation.FailureCategory `json:"failureCategory,omitempty"`
}

type ValidationInput struct {
	CWD      string
	Commands []string
}

type ValidationRunner func(context.Context, ValidationInput) (ValidationResult, error)

type AgentExecutionStartedInput struct {
	ExecutionID string
	ProjectID   string
	LoopID      string
	RunID       string
	Subtitle    string
	Body        string
	DedupeKey   string
}

type AgentExecutionStartedFunc func(context.Context, AgentExecutionStartedInput) error

type NetworkStatusGateway interface {
	Status(context.Context) (protocol.NodeStatusResponse, error)
}

type RunCompletedInput struct {
	ProjectID         string
	LoopID            string
	RunID             string
	Subtitle          string
	Status            string
	Summary           string
	FailureKind       runpipe.QueueFailureKind
	PullRequestNumber int64
	PullRequestURL    string
}

type RunCompletedFunc func(context.Context, RunCompletedInput) error

type Options struct {
	DB                              *sql.DB
	Repos                           *storage.Repositories
	GitHub                          GitHubGateway
	GitHubCLIAvailable              *bool
	GitHubCLIAutoPROpeningAvailable func(context.Context, string, string) bool
	Git                             GitGateway
	AgentExecutor                   AgentExecutor
	Logger                          bootstrap.Logger
	Now                             func() time.Time
	AgentTimeout                    time.Duration
	AgentIdleTimeout                time.Duration
	ClaimTTL                        time.Duration
	ValidationCommands              []string
	ValidationCommandsByProject     map[string][]string
	ValidationRunner                ValidationRunner
	// ContainmentTracker registers validation shell handles with the Execution
	// Supervisor for shutdown drain / retain-storage (#577). Nil in tests or
	// when the runner is not daemon-owned.
	ContainmentTracker      processcontainment.LiveTracker
	AllowAutoCommit         bool
	AllowAutoPush           bool
	OpenPRStrategy          config.OpenPRStrategy
	Disclosure              *config.DisclosureConfig
	AgentRuntime            string
	AgentProfileID          string
	CustomInstructions      *config.Config
	AgentModel              *string
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	OnAgentExecutionStarted AgentExecutionStartedFunc
	OnRunCompleted          RunCompletedFunc
	DiscoveryPolicy         DiscoveryPolicy
	OnQueueItemEnqueued     func()
	Network                 NetworkStatusGateway
	// HITLEnabled gates the mid-run human-in-the-loop feature. When false (the
	// default) none of the HITL code paths run and the worker behaves exactly as
	// before. HITLNotify, when set, sends the ask-card to the human channel.
	HITLEnabled bool
	HITLNotify  HITLNotifyFunc
	// HITLAnswerTransport selects how a mid-run ask is delivered: "github" (post it
	// as a PR comment; the default) or "feishu"/"respond" (via HITLNotify / the API).
	HITLAnswerTransport string
	HITLGitHub          HITLGitHubSettings
}

// HITLGitHubSettings tunes the GitHub PR-comment ask transport.
type HITLGitHubSettings struct {
	AwaitingLabel string
	MentionLogins []string
}

// HITLAskNotification is the payload the worker hands to HITLNotify when an agent
// pauses mid-run to ask a human.
type HITLAskNotification struct {
	ProjectID string
	LoopID    string
	LoopSeq   int64
	RunID     string
	Repo      string
	Title     string
	Question  string
	Options   []string

	// Source + trigger, so the human knows what they are deciding and whose task
	// it is. SourceType/Ref/URL identify the origin (GitHub Issue #132 → link);
	// TriggerLogin is who created it.
	SourceType   string
	SourceRef    string
	SourceURL    string
	TriggerLogin string

	// The agent's decision brief.
	Recommendation    string
	RecommendedOption string
	Consequences      map[string]string
	Confidence        string
}

// HITLNotifyFunc delivers a mid-run ask to the human channel (e.g. a Feishu
// app-bot card). Best-effort; a returned error is logged, not fatal.
type HITLNotifyFunc func(context.Context, HITLAskNotification) error

type DiscoveryPolicy struct {
	AutoDiscovery              bool
	Labels                     []string
	LabelMode                  config.LabelMode
	RequireAssigneeCurrentUser bool
	RoutedClaimPolicy          networkpolicy.ProjectPolicy
}

// Runner is the Worker: a reactive Role that implements a Spec or an Issue,
// producing a Pull Request. Stateful: it persists runs in the local SQLite
// database.
type Runner struct {
	db                          *sql.DB
	repos                       *storage.Repositories
	github                      GitHubGateway
	git                         GitGateway
	agentExecutor               AgentExecutor
	logger                      bootstrap.Logger
	now                         func() time.Time
	agentTimeout                time.Duration
	agentIdleTimeout            time.Duration
	claimTTL                    time.Duration
	validationCommands          []string
	validationCommandsByProject map[string][]string
	validationRunner            ValidationRunner
	containmentTracker          processcontainment.LiveTracker
	allowAutoCommit             bool
	allowAutoPush               bool
	githubCLIAvailable          bool
	githubCLICheck              func(context.Context, string, string) bool
	openPRStrategy              config.OpenPRStrategy
	disclosure                  config.DisclosureConfig
	agentRuntime                string
	agentProfileID              string
	customInstructions          config.Config
	projectRoleConfig           *config.Config
	agentModel                  *string
	retryBaseDelay              time.Duration
	retryMaxAttempts            int64
	onAgentExecutionStarted     AgentExecutionStartedFunc
	onRunCompleted              RunCompletedFunc
	discoveryPolicy             DiscoveryPolicy
	onQueueItemEnqueued         func()
	network                     NetworkStatusGateway
	hitlEnabled                 bool
	hitlNotify                  HITLNotifyFunc
	hitlAnswerTransport         string
	hitlGitHub                  HITLGitHubSettings
	workGraphs                  *workgraphdispatch.Service
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Limit     int
	Snapshot  *githubinfra.DiscoverySnapshot
}

type DiscoveryResult struct {
	CreatedLoopIDs []string
	QueueItems     []storage.QueueItemRecord
	Skipped        int
}

type workerInput struct {
	Title    string `json:"title,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	SpecPath string `json:"specPath,omitempty"`
	Repo     string `json:"repo,omitempty"`
	// IssueRepo is the source issue repository, which may differ from Repo for cross-repo closing references.
	IssueRepo     string `json:"issueRepo,omitempty"`
	BaseBranch    string `json:"baseBranch,omitempty"`
	ExecutionMode string `json:"executionMode,omitempty"`
	IssueNumber   int64  `json:"issueNumber,omitempty"`
	IssueURL      string `json:"issueUrl,omitempty"`
	// TriggerLogin is who created/assigned the source issue (GitHub login), shown
	// as attribution on the HITL ask card so a human knows whose task this is.
	TriggerLogin                 string `json:"triggerLogin,omitempty"`
	PRNumber                     int64  `json:"prNumber,omitempty"`
	PRTitle                      string `json:"prTitle,omitempty"`
	Branch                       string `json:"branch,omitempty"`
	HeadSHA                      string `json:"headSha,omitempty"`
	AutoDiscovered               bool   `json:"autoDiscovered,omitempty"`
	PersonalAssigneeAutoAssigned bool   `json:"personalAssigneeAutoAssigned,omitempty"`
	// IssueClaimOverride is the durable operator authority from a force=true
	// manual issue dispatch. It follows the worker through PR creation and
	// adoption so source-issue admission is not re-applied mid-lifecycle.
	IssueClaimOverride   bool                 `json:"issueClaimOverride,omitempty"`
	RoutedClaimMatchMode string               `json:"routedClaimMatchMode,omitempty"`
	Reviewers            []string             `json:"reviewers,omitempty"`
	Reproduction         *reproducer.Manifest `json:"reproduction,omitempty"`
}

type workerCheckpoint struct {
	ResumePolicy   string                `json:"resumePolicy,omitempty"`
	Work           *workerInput          `json:"work,omitempty"`
	ClaimedLockKey string                `json:"claimedLockKey,omitempty"`
	IssueClaim     *checkpointIssueClaim `json:"issueClaim,omitempty"`
	Worktree       *checkpointWorktree   `json:"worktree,omitempty"`
	Plan           *checkpointPlan       `json:"plan,omitempty"`
	Execution      *checkpointExecution  `json:"execution,omitempty"`
	Lifecycle      *lifecycle.State      `json:"gitPrLifecycle,omitempty"`
	// ReproductionAbsent is set when capture first observes no applicable
	// manifest. A later manifest is only adopted after a completed agent
	// execution (intentional worker-authored reproduction); pre-execution
	// resume must not treat a mid-crash agent file as the contract.
	ReproductionAbsent bool `json:"reproductionAbsent,omitempty"`
	// ReproductionBaseline is the durable red evidence captured before the
	// Worker agent is allowed to edit the worktree. It is separate from
	// Validation because a non-zero reproduction test is expected here while
	// the completion gate requires that same test to pass later.
	ReproductionBaseline *ValidationResult `json:"reproductionBaseline,omitempty"`
	Validation           *ValidationResult `json:"validation,omitempty"`
	PullRequest          *checkpointPullPR `json:"pullRequest,omitempty"`
	SkipReason           string            `json:"skipReason,omitempty"`
}

type checkpointIssueClaim struct {
	Repo        string `json:"repo,omitempty"`
	IssueNumber int64  `json:"issueNumber,omitempty"`
	CommentID   int64  `json:"commentId,omitempty"`
	CommentURL  string `json:"commentUrl,omitempty"`
	Status      string `json:"status,omitempty"`
}

type checkpointWorktree struct {
	ID           string `json:"id,omitempty"`
	Path         string `json:"path,omitempty"`
	Branch       string `json:"branch,omitempty"`
	BaseBranch   string `json:"baseBranch,omitempty"`
	HeadSHA      string `json:"headSha,omitempty"`
	CheckoutMode string `json:"checkoutMode,omitempty"`
}

type checkpointPlan struct {
	Summary string   `json:"summary,omitempty"`
	Items   []string `json:"items,omitempty"`
}

type checkpointExecution struct {
	Status                       string            `json:"status,omitempty"`
	Summary                      string            `json:"summary,omitempty"`
	ParseStatus                  string            `json:"parseStatus,omitempty"`
	ChangedFiles                 []string          `json:"changedFiles,omitempty"`
	Commits                      []string          `json:"commits,omitempty"`
	Lifecycle                    *lifecycle.State  `json:"gitPrLifecycle,omitempty"`
	Stdout                       string            `json:"stdout,omitempty"`
	GitReconciled                bool              `json:"gitReconciled,omitempty"`
	TimeoutType                  string            `json:"timeoutType,omitempty"`
	ConfiguredIdleTimeoutSeconds int64             `json:"configuredIdleTimeoutSeconds,omitempty"`
	ConfiguredMaxRuntimeSeconds  int64             `json:"configuredMaxRuntimeSeconds,omitempty"`
	ElapsedRuntimeSeconds        int64             `json:"elapsedRuntimeSeconds,omitempty"`
	LastProgressAt               string            `json:"lastProgressAt,omitempty"`
	ProgressBeforeTimeout        *worktreeProgress `json:"progressBeforeTimeout,omitempty"`
	ProgressSnapshotError        string            `json:"progressSnapshotError,omitempty"`
}

// worktreeProgress records only status metadata; it never persists diff
// content. It is captured immediately before an executor timeout terminates
// the process group, so a retry can prove what it inherited.
type worktreeProgress struct {
	HeadSHA            string   `json:"headSha,omitempty"`
	WorktreeID         string   `json:"worktreeId,omitempty"`
	Branch             string   `json:"branch,omitempty"`
	ChangedFiles       []string `json:"changedFiles,omitempty"`
	ChangedFileCount   int      `json:"changedFileCount"`
	StagedFiles        []string `json:"stagedFiles,omitempty"`
	StagedFileCount    int      `json:"stagedFileCount"`
	UntrackedFiles     []string `json:"untrackedFiles,omitempty"`
	UntrackedFileCount int      `json:"untrackedFileCount"`
	DiffFingerprint    string   `json:"diffFingerprint,omitempty"`
	ContentFingerprint      string   `json:"contentFingerprint,omitempty"`
	HeadDescendsFromCompare bool     `json:"-"`
	TimeoutType        string   `json:"timeoutType,omitempty"`
	LastProgressAt     string   `json:"lastProgressAt,omitempty"`
	CapturedAt         string   `json:"capturedAt,omitempty"`
}

type checkpointPullPR struct {
	Number  int64  `json:"number,omitempty"`
	URL     string `json:"url,omitempty"`
	HeadSHA string `json:"headSha,omitempty"`
}

type resumedRunContext struct {
	Run        storage.RunRecord
	StartStep  WorkerStep
	Checkpoint workerCheckpoint
	Resumed    bool
}

type stepInput struct {
	Project    storage.ProjectRecord
	Loop       storage.LoopRecord
	Run        storage.RunRecord
	QueueItem  storage.QueueItemRecord
	Checkpoint workerCheckpoint
}

type loopUpsertResult struct {
	record      storage.LoopRecord
	created     bool
	skipEnqueue bool
}

func validateCompletedExecutionCheckpoint(execution *checkpointExecution) error {
	if execution == nil || execution.Status != "completed" {
		return nil
	}
	if execution.ParseStatus == "parsed" {
		return nil
	}
	return &runpipe.LoopError{
		Message: invalidWorkerStructuredResultMessage(execution.ParseStatus),
		Kind:    runpipe.FailureRetryableTransient,
	}
}

func invalidWorkerStructuredResultMessage(parseStatus string) string {
	return fmt.Sprintf("Worker completed without a valid structured result (parse status: %s). See Looper logs for details.", firstNonEmpty(parseStatus, "missing"))
}

func reproductionFailure(err error) *runpipe.LoopError {
	return &runpipe.LoopError{Message: "Reproduction test integrity check failed: " + err.Error(), Kind: runpipe.FailureManualIntervention}
}

// captureWorkerReproduction records an optional manifest once the worktree is
// prepared. A later retry must see the same manifest; silently adopting a new
// one would let a run replace the reproduction authority after the agent ran.
//
// A manifest that declares an originating issue is only adopted for the task
// that owns that issue. A manifest left in the base branch after a previous
// reproduction fix merged is already-fixed history, not the current task's
// reproduction; adopting it would gate unrelated work on a now-green test.
func captureWorkerReproduction(checkpoint *workerCheckpoint, worktreePath string) error {
	if checkpoint == nil || checkpoint.Work == nil {
		return nil
	}
	manifest, err := reproducer.Load(worktreePath)
	if err != nil {
		return reproductionFailure(err)
	}
	if checkpoint.Work.Reproduction != nil {
		if manifest == nil {
			return reproductionFailure(errors.New("reproduction manifest is missing"))
		}
		if !checkpoint.Work.Reproduction.Equal(*manifest) {
			return reproductionFailure(errors.New("reproduction manifest changed during run"))
		}
		if err := checkpoint.Work.Reproduction.Verify(worktreePath); err != nil {
			return reproductionFailure(err)
		}
		return nil
	}
	if manifest != nil {
		if !workerReproductionManifestAppliesToTask(*manifest, *checkpoint.Work) {
			// No applicable contract for this task (e.g. scoped to another
			// issue). Record the negative observation so a later same-issue
			// agent file is not adopted on pre-execution resume.
			if checkpoint.Work.Reproduction == nil {
				checkpoint.ReproductionAbsent = true
			}
			return nil
		}
		if checkpoint.ReproductionAbsent {
			// Negative observation was durable. Only a completed agent execution
			// may introduce the reproduction contract (worker-authored test).
			// Pre-execution resume must not adopt a mid-crash agent file.
			if checkpoint.Execution == nil || checkpoint.Execution.Status != "completed" {
				return reproductionFailure(errors.New("reproduction manifest appeared before agent execution completed"))
			}
		} else if checkpoint.Work.Reproduction == nil && workerPastInitialReproductionCapture(*checkpoint) {
			// Legacy checkpoint without ReproductionAbsent after upgrade:
			// fail closed rather than adopt a mid-run agent file as authority.
			return reproductionFailure(errors.New("legacy checkpoint has unknown reproduction absence; refusing to adopt a mid-run manifest"))
		}
		// First capture must Verify the test hash before the daemon will
		// execute testCommand or treat the manifest as the run authority.
		if err := manifest.Verify(worktreePath); err != nil {
			return reproductionFailure(err)
		}
		checkpoint.Work.Reproduction = manifest
		checkpoint.ReproductionAbsent = false
		return nil
	}
	// Durable negative observation so crash-resume cannot treat a later
	// agent-authored file as the original contract.
	if checkpoint.Work.Reproduction == nil {
		checkpoint.ReproductionAbsent = true
	}
	return nil
}

// workerPastInitialReproductionCapture reports whether the run already advanced
// past the first durable capture opportunity (for legacy fail-closed migration).
//
// A prepared worktree alone is NOT past the first capture: prepare-worktree
// assigns Worktree then immediately calls captureWorkerReproduction. Treating
// Worktree presence as "already captured" would refuse every applicable
// base-branch manifest on a brand-new Worker run.
func workerPastInitialReproductionCapture(checkpoint workerCheckpoint) bool {
	return checkpoint.Execution != nil ||
		checkpoint.Plan != nil ||
		checkpoint.Validation != nil ||
		checkpoint.PullRequest != nil
}

// workerReproductionManifestAppliesToTask reports whether a manifest loaded
// from the checkout is the reproduction authority for this Worker task. A
// manifest scoped to a different issue is treated as already-fixed history.
func workerReproductionManifestAppliesToTask(manifest reproducer.Manifest, work workerInput) bool {
	return manifest.AppliesToIssue(work.IssueNumber, firstNonEmpty(work.IssueRepo, work.Repo))
}

func verifyWorkerReproduction(checkpoint workerCheckpoint, worktreePath string) error {
	if checkpoint.Work == nil || checkpoint.Work.Reproduction == nil {
		return nil
	}
	if err := checkpoint.Work.Reproduction.Verify(worktreePath); err != nil {
		return reproductionFailure(err)
	}
	return nil
}

// reproductionBaselineScope controls whether a baseline gate may observe new
// red evidence or may only reuse evidence captured before the Worker agent
// edited the worktree.
type reproductionBaselineScope int

const (
	// reproductionBaselineScopeCreate may run the reproduction command and
	// record a fresh red observation. It is used only before the Worker agent
	// is allowed to edit the worktree (prepare-worktree and pre-execution
	// execute).
	reproductionBaselineScopeCreate reproductionBaselineScope = iota
	// reproductionBaselineScopeReuseOnly never creates new evidence. It is
	// used after the Worker agent may have edited or committed the worktree
	// (post-execution execute, validate, open-pr). A missing baseline fails
	// closed instead of manufacturing red evidence from a possibly mutated
	// checkout, because a reproduction observed after execution says nothing
	// about the original state the agent started from.
	reproductionBaselineScopeReuseOnly
)

// ensureWorkerReproductionBaseline proves that the committed reproduction is
// actually red before the Worker agent can edit the worktree. The manifest and
// test hash remain the authority; this result is only the durable observation
// that the named command failed. A passing command is not a reproduction, and
// operational failures are not accepted as a red test.
//
// A persisted baseline is only reused when it is an ordinary failing-test
// result bound to the checkout that produced it. A rejected observation
// (immediate pass or operational failure) is discarded so a retry re-derives
// instead of accepting evidence that is not a red test, and a checkout that
// drifted from the recorded head is re-derived before execution. In a
// reuse-only scope a missing baseline fails closed rather than running the
// reproduction against a worktree the agent may already have changed.
func (r *Runner) ensureWorkerReproductionBaseline(ctx context.Context, checkpoint *workerCheckpoint, worktreePath, repoPath, worktreeRoot string, scope reproductionBaselineScope) error {
	if checkpoint == nil || checkpoint.Work == nil || checkpoint.Work.Reproduction == nil {
		return nil
	}
	if err := verifyWorkerReproduction(*checkpoint, worktreePath); err != nil {
		return err
	}
	if baseline := checkpoint.ReproductionBaseline; baseline != nil {
		if workerReproductionBaselineAcceptable(*baseline) && r.workerReproductionBaselineCheckoutMatches(ctx, *baseline, checkpoint.Worktree, worktreePath, repoPath, worktreeRoot, scope) {
			return nil
		}
		// The persisted observation is either not an ordinary red test or was
		// captured against a different checkout. Discard it so a retry
		// re-derives instead of accepting rejected or stale evidence.
		checkpoint.ReproductionBaseline = nil
	}
	if scope == reproductionBaselineScopeReuseOnly {
		return &runpipe.LoopError{Message: "Reproduction baseline evidence is absent after Worker execution; refusing to manufacture red evidence from a possibly mutated worktree. Resume the run from the prepare step so the baseline is captured before the agent edits the checkout.", Kind: runpipe.FailureManualIntervention}
	}
	manifest := checkpoint.Work.Reproduction
	result, err := r.runValidation(ctx, ValidationInput{CWD: worktreePath, Commands: []string{manifest.TestCommand}})
	if err != nil {
		return &runpipe.LoopError{Message: fmt.Sprintf("Reproduction baseline verification failed: %v", err), Kind: runpipe.FailureRetryableTransient}
	}
	if strings.TrimSpace(result.HeadSHA) == "" && checkpoint.Worktree != nil {
		// Worktree.HeadSHA is the immutable checkout head recorded by the
		// prepare-worktree step. Keep it with the red observation when the
		// validation runner does not provide its own head evidence.
		result.HeadSHA = strings.TrimSpace(checkpoint.Worktree.HeadSHA)
	}
	// Reject filesystem changes made by the baseline command. The reproduction
	// must fail without dirtying the worktree the Worker starts on; otherwise
	// the agent begins on side effects and reconcileWorkerGitState can fold
	// them into a fallback commit as if the agent produced them.
	if dirty, derr := r.workerReproductionBaselineWorktreeDirty(ctx, checkpoint.Worktree, worktreePath, repoPath, worktreeRoot); derr != nil {
		return &runpipe.LoopError{Message: fmt.Sprintf("Reproduction baseline worktree cleanliness check failed: %v", derr), Kind: runpipe.FailureRetryableTransient}
	} else if dirty {
		return &runpipe.LoopError{Message: "Reproduction baseline verification failed: reproduction command modified the worktree; refusing to start Worker on uncommitted side effects", Kind: runpipe.FailureManualIntervention}
	}
	checkpoint.ReproductionBaseline = &result
	if result.Passed {
		return &runpipe.LoopError{Message: "Reproduction baseline verification failed: named test passed immediately; a reproduction must fail before Worker execution", Kind: runpipe.FailureManualIntervention}
	}
	if result.FailureCategory != validation.FailureNonZeroExit {
		policy := validation.PolicyFor(result.FailureCategory)
		kind := runpipe.QueueFailureKind(policy.FailureKind)
		if kind == "" {
			kind = runpipe.FailureManualIntervention
		}
		return &runpipe.LoopError{Message: fmt.Sprintf("Reproduction baseline verification failed: named test did not produce an ordinary failing test result (category %q, summary %q)", result.FailureCategory, result.Summary), Kind: kind}
	}
	return nil
}

// workerReproductionBaselineAcceptable reports whether a persisted baseline is
// an ordinary failing-test result that can be reused as red evidence. An
// immediate pass or an operational failure (timeout, etc.) is not a
// reproduction and must not gate Worker execution.
func workerReproductionBaselineAcceptable(result ValidationResult) bool {
	return !result.Passed && result.FailureCategory == validation.FailureNonZeroExit
}

// workerReproductionBaselineCheckoutMatches reports whether the checkout being
// acted on still matches the head recorded with the red evidence. Pre-execution
// (create scope) the head must not have drifted: if the worktree was restored,
// reset, or advanced while the manifest and test stayed unchanged, code outside
// the hashed test can make the reproduction green and the old red observation
// must not be trusted. Post-execution (reuse-only scope) the head legitimately
// advances from agent commits, so the recorded head is not expected to match
// and reuse trusts the pre-execution capture.
func (r *Runner) workerReproductionBaselineCheckoutMatches(ctx context.Context, baseline ValidationResult, worktree *checkpointWorktree, worktreePath, repoPath, worktreeRoot string, scope reproductionBaselineScope) bool {
	if scope == reproductionBaselineScopeReuseOnly {
		return true
	}
	if r.git == nil {
		return true
	}
	recorded := strings.TrimSpace(baseline.HeadSHA)
	if recorded == "" {
		return true
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: repoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktreePath, BaseRef: workerReproductionBaseRef(worktree)})
	if err != nil {
		// If the checkout cannot be inspected, do not silently accept stale
		// evidence; treat it as a mismatch so the caller re-derives or fails.
		return false
	}
	return strings.TrimSpace(inspect.HeadSHA) == recorded
}

// workerReproductionBaselineWorktreeDirty reports whether the worktree has
// uncommitted changes, used to reject filesystem side effects produced by the
// reproduction command. When no git gateway is configured (unit tests) the
// check is skipped.
func (r *Runner) workerReproductionBaselineWorktreeDirty(ctx context.Context, worktree *checkpointWorktree, worktreePath, repoPath, worktreeRoot string) (bool, error) {
	if r.git == nil {
		return false, nil
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: repoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktreePath, BaseRef: workerReproductionBaseRef(worktree)})
	if err != nil {
		return false, err
	}
	return inspect.HasUncommittedChanges, nil
}

func workerReproductionBaseRef(worktree *checkpointWorktree) string {
	if worktree == nil {
		return ""
	}
	return firstNonEmpty(worktree.HeadSHA, worktree.BaseBranch)
}

// persistWorkerReproductionBaseline closes the crash window between observing
// a red reproduction and starting the next operation. Normal workflow steps
// also persist their returned checkpoint, but execute/validate/open-pr can be
// entered directly when resuming an older run, so the evidence must be written
// before those operations proceed.
func (r *Runner) persistWorkerReproductionBaseline(ctx context.Context, input stepInput, checkpoint workerCheckpoint, wasPresent bool) error {
	if wasPresent || checkpoint.ReproductionBaseline == nil || strings.TrimSpace(input.Run.ID) == "" {
		return nil
	}
	if err := r.persistCheckpoint(ctx, input.Run.ID, checkpoint); err != nil {
		return &runpipe.LoopError{Message: fmt.Sprintf("persist reproduction baseline checkpoint: %v", err), Kind: runpipe.FailureRetryableAfterResume}
	}
	return nil
}

func workerValidationCommands(base []string, work workerInput) []string {
	commands := append([]string(nil), base...)
	if work.Reproduction != nil && strings.TrimSpace(work.Reproduction.TestCommand) != "" && !containsWorkerCommand(commands, work.Reproduction.TestCommand) {
		commands = append(commands, work.Reproduction.TestCommand)
	}
	return commands
}

func containsWorkerCommand(commands []string, target string) bool {
	for _, command := range commands {
		if command == target {
			return true
		}
	}
	return false
}

func sanitizePublicIssueClaimSummary(summary string) string {
	cleaned := strings.TrimSpace(disclosure.StripMarkdownStamp(summary))
	if cleaned == "" {
		return ""
	}
	cleaned = workerANSIEscapePattern.ReplaceAllString(cleaned, "")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return ""
	}
	match := workerStructuredResultMessagePattern.FindStringSubmatch(cleaned)
	if len(match) != 2 {
		return "See Looper logs for details."
	}
	cleaned = invalidWorkerStructuredResultMessage(match[1])
	runes := []rune(cleaned)
	if len(runes) > maxPublicIssueClaimSummaryLength {
		cleaned = strings.TrimSpace(string(runes[:maxPublicIssueClaimSummaryLength])) + "…"
	}
	return cleaned
}

func sanitizePublicPausedIssueClaimSummary(summary string) string {
	cleaned := strings.TrimSpace(disclosure.StripMarkdownStamp(summary))
	cleaned = workerANSIEscapePattern.ReplaceAllString(cleaned, "")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if cleaned == "" {
		return ""
	}
	if strings.HasPrefix(cleaned, "Worker worktree path ") && strings.Contains(cleaned, " is stale (") {
		return "Worker checkpoint worktree is unavailable or unsafe. Restore the original branch checkout, then resume the worker run."
	}
	runes := []rune(cleaned)
	if len(runes) > maxPublicIssueClaimSummaryLength {
		cleaned = strings.TrimSpace(string(runes[:maxPublicIssueClaimSummaryLength])) + "…"
	}
	return cleaned
}

func checkpointExecutionFromAgentResult(result AgentResult) *checkpointExecution {
	return &checkpointExecution{
		Status: result.Status, Summary: result.Summary, ParseStatus: result.ParseStatus,
		ChangedFiles: append([]string(nil), result.ChangedFiles...), Commits: append([]string(nil), result.Commits...), Lifecycle: result.Lifecycle, Stdout: result.Stdout,
		TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds,
		ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt,
		ProgressSnapshotError: result.PreTimeoutError,
	}
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	agentTimeout := options.AgentTimeout
	if agentTimeout <= 0 {
		agentTimeout = defaultAgentTimeout
	}
	agentIdleTimeout := options.AgentIdleTimeout
	if agentIdleTimeout <= 0 {
		agentIdleTimeout = 15 * time.Minute
	}
	claimTTL := options.ClaimTTL
	if claimTTL <= 0 {
		claimTTL = defaultClaimTTL
	}
	retryBaseDelay := options.RetryBaseDelay
	if retryBaseDelay <= 0 {
		retryBaseDelay = defaultRetryDelay
	}
	retryMaxAttempts := options.RetryMaxAttempts
	if retryMaxAttempts == 0 {
		retryMaxAttempts = defaultRetryMax
	}
	githubCLIAvailable := options.GitHub != nil
	if options.GitHubCLIAvailable != nil {
		githubCLIAvailable = *options.GitHubCLIAvailable
	}
	strategy := options.OpenPRStrategy
	if strategy == "" {
		strategy = config.OpenPRStrategyManual
	}
	disclosureCfg := config.DefaultDisclosureConfig()
	if options.Disclosure != nil {
		disclosureCfg = *options.Disclosure
	}
	policy := options.DiscoveryPolicy
	if policy.LabelMode == "" {
		policy = DiscoveryPolicy{AutoDiscovery: true, Labels: []string{labels.DefaultWorkerReadyTrigger}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}
	}
	runner := &Runner{
		db:                          options.DB,
		repos:                       options.Repos,
		github:                      options.GitHub,
		git:                         options.Git,
		agentExecutor:               options.AgentExecutor,
		logger:                      options.Logger,
		now:                         now,
		agentTimeout:                agentTimeout,
		agentIdleTimeout:            agentIdleTimeout,
		claimTTL:                    claimTTL,
		validationCommands:          append([]string(nil), options.ValidationCommands...),
		validationCommandsByProject: cloneValidationCommandsByProject(options.ValidationCommandsByProject),
		validationRunner:            options.ValidationRunner,
		containmentTracker:          options.ContainmentTracker,
		allowAutoCommit:             options.AllowAutoCommit,
		allowAutoPush:               options.AllowAutoPush,
		githubCLIAvailable:          githubCLIAvailable,
		githubCLICheck:              options.GitHubCLIAutoPROpeningAvailable,
		openPRStrategy:              strategy,
		disclosure:                  disclosureCfg,
		agentRuntime:                strings.TrimSpace(options.AgentRuntime),
		agentProfileID:              strings.TrimSpace(options.AgentProfileID),
		customInstructions:          customInstructionConfig(options.CustomInstructions),
		projectRoleConfig:           options.CustomInstructions,
		agentModel:                  cloneStringPtr(options.AgentModel),
		retryBaseDelay:              retryBaseDelay,
		retryMaxAttempts:            retryMaxAttempts,
		onAgentExecutionStarted:     options.OnAgentExecutionStarted,
		onRunCompleted:              options.OnRunCompleted,
		discoveryPolicy:             policy,
		onQueueItemEnqueued:         options.OnQueueItemEnqueued,
		network:                     options.Network,
		hitlEnabled:                 options.HITLEnabled,
		hitlNotify:                  options.HITLNotify,
		hitlAnswerTransport:         options.HITLAnswerTransport,
		hitlGitHub:                  options.HITLGitHub,
	}
	runner.workGraphs = workgraphdispatch.New(workgraphdispatch.Options{DB: options.DB, Repositories: options.Repos, Now: now, RetryMaxAttempts: retryMaxAttempts, OnEnqueued: options.OnQueueItemEnqueued})
	return runner
}

func cloneValidationCommandsByProject(source map[string][]string) map[string][]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string][]string, len(source))
	for projectID, commands := range source {
		cloned[projectID] = append([]string(nil), commands...)
	}
	return cloned
}

func (r *Runner) validationCommandsForProject(projectID string) []string {
	if commands, ok := r.validationCommandsByProject[projectID]; ok {
		return commands
	}
	return r.validationCommands
}

// projectValidationPolicyKnown reports whether projectID has a resolved
// validation policy entry from the runtime catalog. The runtime always
// supplies validationCommandsByProject (possibly empty) from
// config.ResolveProjectValidationCommandsByID; a nil map means the runner was
// constructed without catalog policy information (tests/legacy), so the legacy
// global-defaults fallback in validationCommandsForProject is preserved. A
// non-nil map without an entry for projectID means the project was removed from
// the catalog — an unstanced legacy row quarantined because
// defaults.validationCommands is empty and it carries no validation stance.
// Its durable sticky retry must fail closed rather than fall back to the empty
// defaults, which would let validation.RunCommands report success without
// running anything and let the retry modify or publish code without the
// project-owned validation gate that caused it to be quarantined.
func (r *Runner) projectValidationPolicyKnown(projectID string) bool {
	if r.validationCommandsByProject == nil {
		return true
	}
	_, ok := r.validationCommandsByProject[projectID]
	return ok
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	if r.repos == nil || r.repos.Projects == nil || r.repos.Loops == nil || r.repos.Queue == nil || r.github == nil {
		return DiscoveryResult{}, fmt.Errorf("worker discovery is not configured")
	}
	project, err := r.repos.Projects.GetByID(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project == nil {
		return DiscoveryResult{}, fmt.Errorf("project not found: %s", input.ProjectID)
	}
	if project.Archived {
		return DiscoveryResult{Skipped: 1}, nil
	}
	policy := r.discoveryPolicyForProject(project.ID)
	if !policy.AutoDiscovery {
		return DiscoveryResult{Skipped: 1}, nil
	}
	login := ""
	personalProject := r.isPersonalProject(project.ID) && !networkpolicy.IsRouted(policy.RoutedClaimPolicy)
	if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
		login, err = r.github.GetCurrentUserLogin(ctx, input.Repo, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		login = normalizeLogin(login)
		if login != "" {
			policy.RoutedClaimPolicy.GitHubLogin = login
		}
	} else if policy.RequireAssigneeCurrentUser {
		var err error
		login, err = r.github.GetCurrentUserLogin(ctx, input.Repo, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		login = normalizeLogin(login)
	}
	if policy.RequireAssigneeCurrentUser && !networkpolicy.IsRouted(policy.RoutedClaimPolicy) && login == "" {
		return DiscoveryResult{Skipped: 1}, nil
	}
	assigneeFilter := ""
	if !networkpolicy.IsRouted(policy.RoutedClaimPolicy) && policy.RequireAssigneeCurrentUser && !personalProject {
		assigneeFilter = login
	}
	resultLimit := effectiveIssueLimit(input.Limit)
	queryLimit := input.Limit
	if personalProject && policy.RequireAssigneeCurrentUser && queryLimit < personalIssueQueryLimit {
		queryLimit = personalIssueQueryLimit
	}
	requiredTargetLabel, err := r.requiredTargetLabel(ctx, project.ID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	search := ""
	if personalProject && policy.RequireAssigneeCurrentUser {
		search = "author:" + login
	}
	issues, err := r.listOpenIssuesForDiscovery(ctx, ListOpenIssuesInput{Repo: input.Repo, CWD: project.RepoPath, Limit: queryLimit, Assignee: assigneeFilter, Search: search}, policy, requiredTargetLabel)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	eligible := 0
	for _, issue := range issues {
		if domain.IsAutoLaneHeld(domain.LoopTypeWorker, issue.Labels) {
			result.Skipped++
			continue
		}
		personalAssignmentCandidate := shouldConsiderPersonalAssignment(issue, login, personalProject, policy)
		if !personalAssignmentCandidate && !shouldClaimWorkerIssue(issue, login, policy) {
			result.Skipped++
			continue
		}
		if requiredTargetLabel != "" && !labels.Has(issue.Labels, requiredTargetLabel) {
			result.Skipped++
			continue
		}
		fingerprint := buildWorkerDiscoveryFingerprint(input.Repo, firstNonEmpty(derefString(project.BaseBranch), "main"), issue)
		loopResult, err := r.ensureLoopForDiscoveredIssue(ctx, *project, input.Repo, issue, fingerprint)
		if err != nil {
			// A live reviewer/fixer source-issue claim makes only this issue
			// ineligible. Continue discovering unrelated work.
			if _, occupied := storage.IsIssueClaimConflictError(err); occupied {
				result.Skipped++
				continue
			}
			return DiscoveryResult{}, err
		}
		if loopResult.created {
			result.CreatedLoopIDs = append(result.CreatedLoopIDs, loopResult.record.ID)
		}
		if loopResult.skipEnqueue || loopResult.record.Status == "paused" || loopResult.record.Status == "human_takeover" || loopResult.record.Status == "completed" || loopResult.record.Status == "failed" || loopResult.record.Status == "awaiting_human" {
			result.Skipped++
			continue
		}
		assigned, err := r.assignPersonalIssueIfEligible(ctx, *project, input.Repo, issue, login, personalProject, policy.RequireAssigneeCurrentUser)
		if err != nil {
			// Surface personal-assignment failures so a persistent token
			// permission gap does not silently stall the lane. The issue is
			// skipped, but other candidates are still scanned and the error is
			// logged for operator diagnosis.
			if r.logger != nil {
				r.logger.Warn("worker personal issue assignment failed", map[string]any{"projectId": project.ID, "repo": input.Repo, "issueNumber": issue.Number, "error": err.Error()})
			}
			result.Skipped++
			continue
		}
		issue = assigned
		// Reconcile the audit marker when a prior partial success left the
		// daemon assigned but the durable marker was never persisted. The
		// GitHub assignee is the source of truth for the assignment; the
		// marker only records that the daemon owns this personal issue.
		if !issue.PersonalAssigneeAutoAssigned && personalAssigneeAlreadyAssigned(issue, login, personalProject, policy) {
			issue.PersonalAssigneeAutoAssigned = true
		}
		if issue.PersonalAssigneeAutoAssigned {
			loopResult.record, err = r.markPersonalAssignment(ctx, loopResult.record)
			if err != nil {
				return DiscoveryResult{}, err
			}
		}
		queueItem, err := r.enqueueDiscoveredIssue(ctx, *project, loopResult.record, input.Repo, issue, fingerprint)
		if err != nil {
			return DiscoveryResult{}, err
		}
		result.QueueItems = append(result.QueueItems, queueItem)
		// Count only actionable (admitted, assigned, and queued) issues toward
		// the personal limit so paused/completed/failed/awaiting-human loops
		// and assignment failures do not exhaust the window before later
		// unassigned work is reached.
		eligible++
		if eligible >= resultLimit {
			break
		}
	}
	return result, nil
}

func (r *Runner) discoveryPolicyForProject(projectID string) DiscoveryPolicy {
	if r.projectRoleConfig == nil {
		return r.discoveryPolicy
	}
	role, ok := config.ProjectCodingRoleConfig(*r.projectRoleConfig, projectID, config.CodingRoleWorker)
	if !ok {
		return r.discoveryPolicy
	}
	return DiscoveryPolicy{AutoDiscovery: role.Discovery.Enabled, Labels: append([]string(nil), role.Discovery.Labels...), LabelMode: role.Discovery.LabelMode, RequireAssigneeCurrentUser: role.Discovery.RequireAssigneeCurrentUser, RoutedClaimPolicy: networkpolicy.ProjectPolicyForProject(*r.projectRoleConfig, projectID)}
}

func (r *Runner) isPersonalProject(projectID string) bool {
	return r.projectRoleConfig != nil && config.IsPersonalProject(*r.projectRoleConfig, projectID)
}

func shouldConsiderPersonalAssignment(issue IssueSummary, login string, personalProject bool, policy DiscoveryPolicy) bool {
	return personalProject && policy.RequireAssigneeCurrentUser && normalizeLogin(login) != "" && strings.EqualFold(strings.TrimSpace(issue.Author), strings.TrimSpace(login)) && len(issue.Assignees) == 0 && len(issue.AssigneeUsers) == 0 && config.LabelsMatch(issue.Labels, policy.Labels, policy.LabelMode)
}

// personalAssigneeAlreadyAssigned reports whether the daemon is already an
// assignee of a self-authored personal-project issue. This is the stale-state
// left by a prior partial success (assignment persisted on GitHub, audit
// marker not), and lets rediscovery reconcile the durable marker without a
// second assignment mutation.
func personalAssigneeAlreadyAssigned(issue IssueSummary, login string, personalProject bool, policy DiscoveryPolicy) bool {
	return personalProject && policy.RequireAssigneeCurrentUser && normalizeLogin(login) != "" && strings.EqualFold(strings.TrimSpace(issue.Author), strings.TrimSpace(login)) && includesLogin(issue.Assignees, login) && config.LabelsMatch(issue.Labels, policy.Labels, policy.LabelMode)
}

func (r *Runner) assignPersonalIssueIfEligible(ctx context.Context, project storage.ProjectRecord, repo string, issue IssueSummary, login string, personalProject, requireAssignee bool) (IssueSummary, error) {
	if !personalProject || !requireAssignee || normalizeLogin(login) == "" || !strings.EqualFold(strings.TrimSpace(issue.Author), strings.TrimSpace(login)) || len(issue.Assignees) > 0 || len(issue.AssigneeUsers) > 0 {
		return issue, nil
	}
	if err := r.github.AddIssueAssignees(ctx, IssueAssigneesInput{Repo: repo, IssueNumber: issue.Number, Assignees: []string{login}, CWD: project.RepoPath}); err != nil {
		return issue, fmt.Errorf("assign self-authored worker issue %s#%d: %w", repo, issue.Number, err)
	}
	issue.Assignees = append(issue.Assignees, login)
	issue.PersonalAssigneeAutoAssigned = true
	return issue, nil
}

func (r *Runner) markPersonalAssignment(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	var mergeErr error
	updated, err := r.updateLoop(ctx, loop, func(record *storage.LoopRecord) {
		metadataJSON, err := mergeLoopMetadataJSON(record.MetadataJSON, map[string]any{"personalProjectAutoAssigned": true})
		if err != nil {
			mergeErr = err
			return
		}
		record.MetadataJSON = &metadataJSON
	})
	if mergeErr != nil {
		return storage.LoopRecord{}, fmt.Errorf("merge personal assignment metadata: %w", mergeErr)
	}
	return updated, err
}

func (r *Runner) requiredTargetLabel(ctx context.Context, projectID string) (string, error) {
	if r.projectNetworkMode(projectID) != config.ProjectNetworkModeRouted {
		return "", nil
	}
	if r.network == nil {
		return "", fmt.Errorf("worker network status is not configured")
	}
	status, err := r.network.Status(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status.Membership.NodeName) == "" {
		return "", fmt.Errorf("worker network status is missing node name")
	}
	return protocol.TargetLabelForNode(status.Membership.NodeName), nil
}

func (r *Runner) projectNetworkMode(projectID string) config.ProjectNetworkMode {
	if r == nil || r.projectRoleConfig == nil {
		return config.ProjectNetworkModeOff
	}
	for _, project := range r.projectRoleConfig.Projects {
		if project.ID != projectID {
			continue
		}
		if project.Network.Mode != "" {
			return project.Network.Mode
		}
		break
	}
	return config.ProjectNetworkModeOff
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*runpipe.ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("worker runner is not configured")
	}
	claimed, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, "worker")
	if err != nil || claimed == nil {
		return nil, err
	}
	return r.ProcessClaimedQueueItem(ctx, *claimed)
}

func (r *Runner) ProcessClaimedQueueItem(ctx context.Context, queueItem storage.QueueItemRecord) (*runpipe.ProcessResult, error) {
	result, err := r.ProcessClaimedItem(ctx, queueItem)
	if err != nil {
		if errors.Is(err, ErrSuccessfulClaimFinalization) {
			return nil, err
		}
		return r.recoverClaimedItem(ctx, queueItem, err)
	}
	return &result, nil
}

func (r *Runner) recoverClaimedItem(ctx context.Context, queueItem storage.QueueItemRecord, err error) (*runpipe.ProcessResult, error) {
	failure := r.classifyFailure(err)
	failedQueue, failErr := r.failQueueItem(ctx, queueItem, failure.Kind, failure.Message)
	if failErr != nil {
		return nil, failErr
	}
	if err := r.reconcileRecoveredLoop(ctx, queueItem, failedQueue, failure.Kind); err != nil {
		return nil, err
	}
	if shouldNotifyCompletedRun(failure.Kind, failedQueue) {
		r.notifyRecoveredRunCompleted(ctx, queueItem, failure)
	}
	return &runpipe.ProcessResult{LoopID: derefString(queueItem.LoopID), QueueItemID: queueItem.ID, Status: "failed", Summary: failure.Message, FailureKind: failure.Kind}, nil
}

func (r *Runner) reconcileRecoveredLoop(ctx context.Context, queueItem storage.QueueItemRecord, failedQueue *storage.QueueItemRecord, failureKind runpipe.QueueFailureKind) error {
	if queueItem.LoopID == nil {
		return nil
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil {
		return err
	}
	// A run may fail before the status flip to running commits. In that
	// partial-transition case the loop is still queued, but the durable queue
	// result still has to reconcile it instead of leaving a terminal queue item
	// paired with a retryable loop.
	if loop == nil || (loop.Status != "running" && loop.Status != "queued") {
		return nil
	}
	_, err = r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.LastRunAt = runpipe.StringPtr(r.nowISO())
		if updated.Status == "paused" || updated.Status == "human_takeover" {
			updated.NextRunAt = nil
		} else if failedQueue != nil && failedQueue.Status == "queued" {
			updated.Status = "queued"
			updated.NextRunAt = runpipe.StringPtr(failedQueue.AvailableAt)
		} else {
			updated.Status = "paused"
			stampWorkerFailedDiscoveryFingerprint(updated, queueItem)
			updated.NextRunAt = nil
		}
	})
	return err
}

func (r *Runner) ProcessClaimedItem(ctx context.Context, queueItem storage.QueueItemRecord) (runpipe.ProcessResult, error) {
	if queueItem.Type != "worker" {
		return runpipe.ProcessResult{}, fmt.Errorf("unsupported queue item type: %s", queueItem.Type)
	}
	if queueItem.LoopID == nil {
		return runpipe.ProcessResult{}, fmt.Errorf("worker queue item requires loopId")
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil {
		return runpipe.ProcessResult{}, err
	}
	if loop == nil {
		return runpipe.ProcessResult{}, fmt.Errorf("loop not found: %s", *queueItem.LoopID)
	}
	project, err := r.repos.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil {
		return runpipe.ProcessResult{}, err
	}
	if project == nil {
		return runpipe.ProcessResult{}, fmt.Errorf("project not found: %s", loop.ProjectID)
	}
	if result, finalized, err := r.finalizePriorSuccessfulClaim(ctx, *loop, queueItem); err != nil || finalized {
		return result, err
	}
	if graphQueueItem(queueItem) {
		claimed, err := r.workGraphs.ClaimWorkerNode(ctx, loop.ID)
		if err != nil {
			return runpipe.ProcessResult{}, err
		}
		if !claimed {
			reclaimed, err := r.workGraphs.ReclaimWorkerNode(ctx, loop.ID)
			if err != nil {
				return runpipe.ProcessResult{}, err
			}
			if !reclaimed {
				return r.finishDuplicateGraphQueueItem(ctx, *loop, queueItem)
			}
		}
	}
	if err := r.revalidateRoutedWorkerClaim(ctx, *project, *loop, queueItem); err != nil {
		return runpipe.ProcessResult{}, err
	}
	// A project removed from the runtime catalog (quarantined unstanced legacy
	// row with empty defaults.validationCommands) has no resolved validation
	// policy. Its durable sticky retry must fail closed before any agent work
	// so it cannot modify or publish code without the project-owned validation
	// gate. No run exists yet, so terminal-fail the queue item and pause the
	// loop inline (recoverClaimedItem only reconciles loops already flipped to
	// running, which this check precedes). The project becomes runnable again
	// once repaired via PATCH or once defaults.validationCommands is configured.
	if !r.projectValidationPolicyKnown(project.ID) {
		failure := &runpipe.LoopError{
			Message: fmt.Sprintf("project %s is quarantined without a validation policy; repair the project validation stance or configure defaults.validationCommands before retrying", project.ID),
			Kind:    runpipe.FailureManualIntervention,
		}
		failedQueue, err := r.failQueueItem(ctx, queueItem, failure.Kind, failure.Message)
		if err != nil {
			return runpipe.ProcessResult{}, err
		}
		if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
			updated.LastRunAt = runpipe.StringPtr(r.nowISO())
			updated.Status = "paused"
			stampWorkerFailedDiscoveryFingerprint(updated, queueItem)
			updated.NextRunAt = nil
		}); err != nil {
			return runpipe.ProcessResult{}, err
		}
		// This branch terminalizes a durable sticky retry before a run is
		// created, so ProcessClaimedQueueItem sees a successful result and does
		// not enter recoverClaimedItem. Preserve the same operator notification
		// contract as every other manual-intervention failure after both durable
		// queue and loop writes have committed.
		if shouldNotifyCompletedRun(failure.Kind, failedQueue) {
			r.notifyRecoveredRunCompleted(ctx, queueItem, failure)
		}
		return runpipe.ProcessResult{LoopID: loop.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.Message, FailureKind: failure.Kind}, nil
	}
	resumedRun, err := r.createRunContext(ctx, *loop)
	if err != nil {
		return runpipe.ProcessResult{}, err
	}
	run := resumedRun.Run
	checkpoint := resumedRun.Checkpoint
	claimedLockKey := ""
	acquiredClaimedLock := false
	if resumedRun.StartStep != stepPrepareWork {
		claimedLockKey = checkpoint.ClaimedLockKey
	}
	if claimedLockKey != "" {
		acquired, err := r.reacquireClaimedLock(ctx, claimedLockKey, queueItem.ID)
		if err != nil {
			return runpipe.ProcessResult{}, err
		}
		if !acquired {
			return runpipe.ProcessResult{}, &runpipe.LoopError{Message: fmt.Sprintf("Worker lock is already held for %s", claimedLockKey), Kind: runpipe.FailureRetryableTransient}
		}
		acquiredClaimedLock = true
	}
	defer func() {
		if acquiredClaimedLock && claimedLockKey != "" {
			releaseCtx := context.Background()
			_ = r.repos.Locks.Release(releaseCtx, claimedLockKey)
			if strings.HasPrefix(claimedLockKey, "issue:") && checkpoint.PullRequest != nil && checkpoint.Work != nil && checkpoint.Work.Repo != "" && checkpoint.PullRequest.Number > 0 {
				prLockKey := storage.PullRequestLockKey(derefString(queueItem.ProjectID), checkpoint.Work.Repo, checkpoint.PullRequest.Number)
				if err := r.repos.Queue.UpdateLockKey(releaseCtx, queueItem.ID, prLockKey, r.nowISO()); err != nil {
					if r.logger != nil {
						r.logger.Warn("worker queue lock retarget failed", map[string]any{"queueItemId": queueItem.ID, "lockKey": prLockKey, "error": err.Error()})
					}
				} else {
					checkpoint.ClaimedLockKey = prLockKey
					if err := r.persistCheckpoint(releaseCtx, run.ID, checkpoint); err != nil && r.logger != nil {
						r.logger.Warn("worker checkpoint lock retarget failed", map[string]any{"runId": run.ID, "queueItemId": queueItem.ID, "lockKey": prLockKey, "error": err.Error()})
					}
				}
			}
		}
	}()
	if resumedRun.Resumed {
		result, handled, err := r.stopObsoleteResumedIssueRun(ctx, *project, *loop, run, queueItem, &checkpoint, &claimedLockKey)
		if err != nil || handled {
			return result, err
		}
		if resumedRun.StartStep != stepPrepareWork {
			if held, summary, err := r.workerHoldSummary(ctx, *project, *loop, queueItem, checkpoint); err != nil {
				return runpipe.ProcessResult{}, err
			} else if held {
				return r.finishHeldWorkerQueueItem(ctx, *project, *loop, &run, queueItem, checkpoint, summary)
			}
		}
	}
	if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.Status = "running"
		updated.LastRunAt = runpipe.StringPtr(run.StartedAt)
		updated.NextRunAt = nil
	}); err != nil {
		return runpipe.ProcessResult{}, err
	}
	if err := validateWorkerResumeCheckpoint(resumedRun.StartStep, checkpoint); err != nil {
		failure := r.classifyFailureWithBoundary(err, failureclass.BoundaryCheckpoint)
		latest := r.getLatestCheckpoint(ctx, run, checkpoint)
		if latest.ResumePolicy == "" {
			latest.ResumePolicy = "replay_step"
		}
		if _, err := r.completeRun(ctx, run, "failed", failure.Message, failure.Message, latest); err != nil {
			return runpipe.ProcessResult{}, err
		}
		failedQueue, err := r.failQueueItem(ctx, queueItem, failure.Kind, failure.Message)
		if err != nil {
			return runpipe.ProcessResult{}, err
		}
		if shouldNotifyCompletedRun(failure.Kind, failedQueue) {
			r.notifyRunCompleted(ctx, buildRunCompletedInput(*project, *loop, run, latest, "failed", failure.Kind, failure.Message))
		}
		if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
			updated.LastRunAt = runpipe.StringPtr(r.nowISO())
			if updated.Status == "paused" || updated.Status == "human_takeover" {
				updated.NextRunAt = nil
			} else if failedQueue != nil && failedQueue.Status == "queued" {
				updated.Status = "queued"
				updated.NextRunAt = runpipe.StringPtr(failedQueue.AvailableAt)
			} else {
				updated.Status = "paused"
				stampWorkerFailedDiscoveryFingerprint(updated, queueItem)
				updated.NextRunAt = nil
			}
		}); err != nil {
			return runpipe.ProcessResult{}, err
		}
		return runpipe.ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.Message, FailureKind: failure.Kind}, nil
	}

	for _, step := range workflow.From(resumedRun.StartStep) {
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return runpipe.ProcessResult{}, err
		}
		checkpoint, err = r.executeStep(ctx, step, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Checkpoint: checkpoint})
		if awaiting, ok := asAwaitingHumanError(err); ok {
			return r.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Checkpoint: checkpoint}, run, checkpoint, awaiting)
		}
		if err != nil {
			var holdErr *runpipe.HoldSkipError
			if errors.As(err, &holdErr) {
				return r.finishHeldWorkerQueueItem(ctx, *project, *loop, &run, queueItem, checkpoint, holdErr.Summary)
			}
			failure := r.classifyFailureWithBoundary(err, workerFailureBoundaryForStep(step))
			latest := r.getLatestCheckpoint(ctx, run, checkpoint)
			latest.ResumePolicy = loops.NormalizeResumePolicy(string(failure.Kind), latest.ResumePolicy)
			if _, err := r.completeRun(ctx, run, "failed", failure.Message, failure.Message, latest); err != nil {
				return runpipe.ProcessResult{}, err
			}
			failedQueue, err := r.failQueueItem(ctx, queueItem, failure.Kind, failure.Message)
			if err != nil {
				return runpipe.ProcessResult{}, err
			}
			if shouldNotifyCompletedRun(failure.Kind, failedQueue) {
				r.notifyRunCompleted(ctx, buildRunCompletedInput(*project, *loop, run, latest, "failed", failure.Kind, failure.Message))
			}
			if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
				updated.LastRunAt = runpipe.StringPtr(r.nowISO())
				if updated.Status == "paused" || updated.Status == "human_takeover" {
					updated.NextRunAt = nil
				} else if failedQueue != nil && failedQueue.Status == "queued" {
					updated.Status = "queued"
					updated.NextRunAt = runpipe.StringPtr(failedQueue.AvailableAt)
				} else {
					updated.Status = "paused"
					stampWorkerFailedDiscoveryFingerprint(updated, queueItem)
					updated.NextRunAt = nil
				}
			}); err != nil {
				return runpipe.ProcessResult{}, err
			}
			r.syncIssueClaim(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem}, &latest, issueClaimStatusForFailure(latest, failedQueue, failure.Kind), failure.Message)
			return runpipe.ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.Message, FailureKind: failure.Kind}, nil
		}
		if step == stepPrepareWork && checkpoint.SkipReason == "" {
			claimedLockKey = checkpoint.ClaimedLockKey
			acquiredClaimedLock = claimedLockKey != ""
		}
		run, err = r.persistStepCompleted(ctx, run, step, checkpoint)
		if err != nil {
			return runpipe.ProcessResult{}, err
		}
		if checkpoint.SkipReason != "" {
			break
		}
	}

	summary := r.buildSuccessSummary(*loop, checkpoint)
	status := "success"
	if checkpoint.SkipReason != "" {
		status = "skipped"
	}
	completedRun, err := r.finalizeSuccessfulClaim(ctx, run, queueItem, *loop, checkpoint, summary)
	if err != nil {
		return runpipe.ProcessResult{}, err
	}
	finalIssueClaimStatus := issueClaimStatusSuccess
	if checkpoint.SkipReason != "" {
		finalIssueClaimStatus = issueClaimStatusPaused
	}
	r.syncIssueClaim(ctx, stepInput{Project: *project, Loop: *loop, Run: completedRun, QueueItem: queueItem}, &checkpoint, finalIssueClaimStatus, summary)
	r.notifyRunCompleted(ctx, buildRunCompletedInput(*project, *loop, completedRun, checkpoint, statusForCheckpoint(checkpoint), "", summary))
	return runpipe.ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary, PullRequestNumber: pullRequestNumber(checkpoint.PullRequest)}, nil
}

func (r *Runner) revalidateRoutedWorkerClaim(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord) error {
	policy := r.discoveryPolicyForProject(project.ID)
	if !networkpolicy.IsRouted(policy.RoutedClaimPolicy) || r.github == nil || loop.TargetType != "issue" {
		return nil
	}
	repo := derefString(loop.Repo)
	if repo == "" {
		repo = derefString(queueItem.Repo)
	}
	issueNumber := parseIssueNumberFromTargetID(derefString(loop.TargetID))
	if issueNumber == 0 {
		issueNumber = parseIssueNumberFromTargetID(queueItem.TargetID)
	}
	if repo == "" || issueNumber == 0 {
		return nil
	}
	issue, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: project.RepoPath})
	if err != nil {
		return err
	}
	decision := networkpolicy.EvaluateWorker(policy.RoutedClaimPolicy, issue.Labels, issue.AssigneeUsers)
	if !decision.Allowed {
		return &runpipe.LoopError{Message: fmt.Sprintf("Skipped routed worker claim for %s#%d: %s", repo, issueNumber, decision.Reason), Kind: runpipe.FailureManualIntervention}
	}
	return nil
}

func (r *Runner) executeStep(ctx context.Context, step WorkerStep, input stepInput) (workerCheckpoint, error) {
	switch step {
	case stepPrepareWork:
		return r.runPrepareWorkStep(ctx, input)
	case stepPrepareWorktree:
		return r.runPrepareWorktreeStep(ctx, input)
	case stepPlan:
		return r.runPlanStep(input)
	case stepExecute:
		return r.runExecuteStep(ctx, input)
	case stepValidate:
		return r.runValidateStep(ctx, input)
	case stepOpenPR:
		return r.runOpenPRStep(ctx, input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported worker step: %s", step)
	}
}

func (r *Runner) reacquireClaimedLock(ctx context.Context, claimedLockKey string, owner string) (bool, error) {
	nowISO := r.nowISO()
	reason := "worker-run-resume"
	expiresAt := eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL))
	acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: claimedLockKey, Owner: owner, Reason: &reason, ExpiresAt: expiresAt, CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		return false, err
	}
	if acquired {
		return true, nil
	}
	lock, err := r.repos.Locks.Get(ctx, claimedLockKey)
	if err != nil {
		return false, err
	}
	if lock == nil || lock.Owner != owner {
		return false, nil
	}
	refreshed, err := r.repos.Locks.Refresh(ctx, storage.LockRecord{Key: claimedLockKey, Owner: owner, Reason: &reason, ExpiresAt: expiresAt, UpdatedAt: nowISO})
	if err != nil {
		return false, err
	}
	return refreshed, nil
}

func (r *Runner) stopObsoleteResumedIssueRun(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, run storage.RunRecord, queueItem storage.QueueItemRecord, checkpoint *workerCheckpoint, claimedLockKey *string) (runpipe.ProcessResult, bool, error) {
	if checkpoint == nil || checkpoint.Work == nil || checkpoint.Work.ExecutionMode != "create-pr" || checkpoint.Work.IssueNumber <= 0 || r.github == nil {
		return runpipe.ProcessResult{}, false, nil
	}
	if err := r.validateWorkerIssueStillOpen(ctx, project.RepoPath, *checkpoint.Work); err == nil {
		return runpipe.ProcessResult{}, false, nil
	} else if !isWorkerIssueTargetObsolete(err) {
		return runpipe.ProcessResult{}, false, nil
	} else {
		if checkpoint.ClaimedLockKey != "" {
			if err := r.repos.Locks.Release(context.Background(), checkpoint.ClaimedLockKey); err != nil {
				return runpipe.ProcessResult{}, true, err
			}
			checkpoint.ClaimedLockKey = ""
		}
		if claimedLockKey != nil {
			*claimedLockKey = ""
		}
		checkpoint.SkipReason = fmt.Sprintf("Worker stopped because %s is no longer an open issue", formatIssueReference(issueLookupRepo(*checkpoint.Work), checkpoint.Work.IssueNumber))
		checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
		summary := r.buildSuccessSummary(loop, *checkpoint)
		completedRun, err := r.finalizeSuccessfulClaim(ctx, run, queueItem, loop, *checkpoint, summary)
		if err != nil {
			return runpipe.ProcessResult{}, true, err
		}
		r.syncIssueClaim(ctx, stepInput{Project: project, Loop: loop, Run: completedRun, QueueItem: queueItem}, checkpoint, issueClaimStatusPaused, summary)
		r.notifyRunCompleted(ctx, buildRunCompletedInput(project, loop, completedRun, *checkpoint, statusForCheckpoint(*checkpoint), "", summary))
		return runpipe.ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "skipped", Summary: summary}, true, nil
	}
}

func (r *Runner) validateWorkerIssueStillOpen(ctx context.Context, cwd string, work workerInput) error {
	if r.github == nil || work.ExecutionMode != "create-pr" || work.IssueNumber <= 0 {
		return nil
	}
	lookupRepo := issueLookupRepo(work)
	issue, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: lookupRepo, IssueNumber: work.IssueNumber, CWD: cwd})
	if err != nil {
		return err
	}
	return validateWorkerIssueTarget(lookupRepo, work.IssueNumber, issue)
}

func isWorkerIssueTargetObsolete(err error) bool {
	var loopErr *runpipe.LoopError
	return errors.As(err, &loopErr) && loopErr.Kind == runpipe.FailureNonRetryable
}

func (r *Runner) workerBranchAheadOfBase(ctx context.Context, project storage.ProjectRecord, work workerInput, worktree checkpointWorktree) (bool, error) {
	branch := firstNonEmpty(worktree.Branch, work.Branch)
	if branch == "" || work.BaseBranch == "" {
		return true, nil
	}
	if r.github != nil && work.Repo != "" {
		comparison, err := r.github.CompareBranches(ctx, CompareBranchesInput{Repo: work.Repo, BaseBranch: work.BaseBranch, HeadBranch: branch, CWD: project.RepoPath})
		if err != nil {
			return false, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
		return comparison.AheadBy > 0, nil
	}
	if r.git == nil {
		return true, nil
	}
	worktreeRoot, err := workerWorktreeRoot(project)
	if err != nil {
		return false, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: work.BaseBranch})
	if err != nil {
		return false, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	return len(inspect.NewCommitSHAs) > 0, nil
}

func (r *Runner) runPrepareWorkStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Work != nil {
		if err := r.validateWorkerIssueStillOpen(ctx, input.Project.RepoPath, *checkpoint.Work); err != nil {
			if isWorkerIssueTargetObsolete(err) {
				checkpoint.SkipReason = fmt.Sprintf("Worker stopped because %s is no longer an open issue", formatIssueReference(issueLookupRepo(*checkpoint.Work), checkpoint.Work.IssueNumber))
				checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
				return checkpoint, nil
			}
			return checkpoint, err
		}
	}
	work, err := r.resolveWorkerInput(ctx, input.Project, input.Loop, input.QueueItem, checkpoint)
	if err != nil {
		return checkpoint, err
	}
	holdWork, retargetedToPullRequest := workerHoldInputForTarget(work, input.Loop, input.QueueItem)
	if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
		return checkpoint, err
	} else if held {
		return checkpoint, &runpipe.HoldSkipError{Summary: summary}
	}
	lockKey := derefString(input.QueueItem.LockKey)
	if lockKey == "" {
		if work.ExecutionMode == "push-existing" && work.Repo != "" && work.PRNumber > 0 {
			lockKey = storage.PullRequestLockKey(input.Project.ID, work.Repo, work.PRNumber)
		} else if work.IssueNumber > 0 {
			lockKey = storage.IssueLockKey(input.Project.ID, issueLookupRepo(work), work.IssueNumber)
		} else {
			lockKey = fmt.Sprintf("worker:%s", input.Loop.ID)
		}
	}
	nowISO := r.nowISO()
	reason := "worker-run"
	acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: input.QueueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		return checkpoint, err
	}
	if !acquired {
		return checkpoint, &runpipe.LoopError{Message: fmt.Sprintf("Worker lock is already held for %s", lockKey), Kind: runpipe.FailureRetryableTransient}
	}
	policy := r.discoveryPolicyForProject(input.Project.ID)
	if work.IssueNumber > 0 && r.github != nil && (!work.AutoDiscovered || policy.RequireAssigneeCurrentUser) {
		if err := r.selfAssignIssue(ctx, work, input.Project.RepoPath); err != nil {
			_ = r.repos.Locks.Release(context.Background(), lockKey)
			return checkpoint, err
		}
	}
	if input.Loop.TargetType == "pull_request" && work.Repo != "" && work.PRNumber > 0 && r.github != nil {
		_ = r.github.RemovePullRequestLabels(ctx, PullRequestLabelsInput{Repo: work.Repo, PRNumber: work.PRNumber, Labels: []string{labels.SpecReady}, CWD: input.Project.RepoPath})
	}
	checkpoint.Work = &work
	checkpoint.ClaimedLockKey = lockKey
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	checkpoint.SkipReason = ""
	r.syncIssueClaim(ctx, input, &checkpoint, issueClaimStatusRunning, "")
	return checkpoint, nil
}

func (r *Runner) selfAssignIssue(ctx context.Context, work workerInput, cwd string) error {
	if r.github == nil || work.IssueNumber <= 0 {
		return nil
	}
	repo := issueLookupRepo(work)
	if repo == "" {
		return nil
	}
	login, err := r.github.GetCurrentUserLogin(ctx, repo, cwd)
	if err != nil {
		return &runpipe.LoopError{Message: fmt.Sprintf("Unable to resolve GitHub login for worker issue self-assignment on %s#%d: %v", repo, work.IssueNumber, err), Kind: runpipe.FailureRetryableAfterResume}
	}
	login = normalizeLogin(login)
	if login == "" {
		return nil
	}
	if err := r.github.AddIssueAssignees(ctx, IssueAssigneesInput{Repo: repo, IssueNumber: work.IssueNumber, Assignees: []string{login}, CWD: cwd}); err != nil {
		return &runpipe.LoopError{Message: fmt.Sprintf("Unable to assign issue %s#%d to %s: %v", repo, work.IssueNumber, login, err), Kind: runpipe.FailureRetryableAfterResume}
	}
	return nil
}

func (r *Runner) runPrepareWorktreeStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	projectMetadata := parseJSONObject(input.Project.MetadataJSON)
	worktreeRoot := stringFromAnyDefault(projectMetadata["worktreeRoot"])
	if worktreeRoot == "" {
		worktreeRoot, err = config.DefaultProjectWorktreeRoot(input.Project.ID, input.Project.RepoPath)
		if err != nil {
			return checkpoint, err
		}
	}
	if checkpoint.Worktree != nil {
		if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: checkpoint.Worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err == nil {
			// Reusing an existing path skips CreateWorktree/RestoreWorktree, so
			// revoke any fixer owner marker stamped on this checkout.
			if err := worktreesafety.ClearFixerOwnerToken(checkpoint.Worktree.Path); err != nil {
				return checkpoint, err
			}
			if err := captureWorkerReproduction(&checkpoint, checkpoint.Worktree.Path); err != nil {
				return checkpoint, err
			}
			baselineWasPresent := checkpoint.ReproductionBaseline != nil
			if err := r.ensureWorkerReproductionBaseline(ctx, &checkpoint, checkpoint.Worktree.Path, input.Project.RepoPath, worktreeRoot, reproductionBaselineScopeCreate); err != nil {
				return checkpoint, err
			}
			if err := r.persistWorkerReproductionBaseline(ctx, input, checkpoint, baselineWasPresent); err != nil {
				return checkpoint, err
			}
			return checkpoint, nil
		} else {
			// A checkpointed path may hold uncommitted progress even when the
			// predecessor crashed before it could persist an execution result.
			// Recreating its deterministic branch path here would silently turn a
			// same-worktree continuation into a fresh checkout. Stop for an
			// operator to recover the exact recorded checkout instead.
			return checkpoint, staleWorkerWorktreeError(*checkpoint.Worktree, work, err.Error())
		}
	}
	branch := work.Branch
	if branch == "" {
		if work.ExecutionMode == "push-existing" && work.PRNumber > 0 {
			branch = fmt.Sprintf("pr-%d", work.PRNumber)
		} else {
			branch = buildWorkerBranchName(work, input.Loop.ID)
		}
	}
	if _, err := loops.DecodeMetadataObjectForWrite(input.Loop.MetadataJSON); err != nil {
		return checkpoint, fmt.Errorf("validate worktree metadata before create: %w", err)
	}
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID:         input.Project.ID,
		RepoPath:          input.Project.RepoPath,
		WorktreeRoot:      worktreeRoot,
		Branch:            branch,
		BaseBranch:        work.BaseBranch,
		PRNumber:          work.PRNumber,
		ProtectedBranches: compactStrings([]string{work.BaseBranch}),
	})
	if err != nil {
		return checkpoint, err
	}
	if work.ExecutionMode == "push-existing" {
		prepared, err := r.git.PrepareWorktree(ctx, PrepareWorktreeInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: created.WorktreePath, Branch: created.Branch, ExpectedHeadSHA: work.HeadSHA})
		if err != nil {
			return checkpoint, err
		}
		if !prepared.Clean {
			return checkpoint, &runpipe.LoopError{Message: fmt.Sprintf("Worker worktree is dirty for branch %s", created.Branch), Kind: runpipe.FailureManualIntervention}
		}
		if prepared.HeadSHA != "" {
			created.HeadSHA = prepared.HeadSHA
		}
	}
	worktreeID := created.WorktreeID
	baseBranch := created.BaseBranch
	if baseBranch == "" {
		baseBranch = work.BaseBranch
	}
	metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"worktreeId": worktreeID, "worktreePath": created.WorktreePath, "branch": created.Branch, "baseBranch": baseBranch})
	if err != nil {
		// Continuing without the persisted worktree keys would run the agent
		// in a worktree the loop's durable state cannot account for.
		return checkpoint, fmt.Errorf("record worktree metadata: %w", err)
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = runpipe.StringPtr(metadataJSON) }); err != nil {
		return checkpoint, fmt.Errorf("record worktree metadata: %w", err)
	}
	checkpoint.Worktree = &checkpointWorktree{ID: worktreeID, Path: created.WorktreePath, Branch: created.Branch, BaseBranch: baseBranch, HeadSHA: created.HeadSHA, CheckoutMode: "branch"}
	checkpoint.Lifecycle = lifecycle.NewState(lifecycle.AgentManagedWithFallbackPolicy("worker", work.ExecutionMode == "create-pr"), created.Branch, baseBranch)
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	if err := captureWorkerReproduction(&checkpoint, created.WorktreePath); err != nil {
		return checkpoint, err
	}
	baselineWasPresent := checkpoint.ReproductionBaseline != nil
	if err := r.ensureWorkerReproductionBaseline(ctx, &checkpoint, created.WorktreePath, input.Project.RepoPath, worktreeRoot, reproductionBaselineScopeCreate); err != nil {
		return checkpoint, err
	}
	if err := r.persistWorkerReproductionBaseline(ctx, input, checkpoint, baselineWasPresent); err != nil {
		return checkpoint, err
	}
	return checkpoint, nil
}

func (r *Runner) ensureWorkerWorktreeUsable(ctx context.Context, input stepInput, checkpoint *workerCheckpoint, work workerInput, worktree checkpointWorktree) (checkpointWorktree, error) {
	if strings.TrimSpace(worktree.Path) == "" {
		return worktree, nil
	}
	worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
	if rootErr != nil {
		return worktree, &runpipe.LoopError{Message: rootErr.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
		return worktree, staleWorkerWorktreeError(worktree, work, err.Error())
	}
	if err := r.verifyCheckpointWorktreeRecord(ctx, input.Project, worktree); err != nil {
		return worktree, err
	}
	info, err := os.Stat(worktree.Path)
	if err == nil {
		if !info.IsDir() {
			return worktree, staleWorkerWorktreeError(worktree, work, "path exists but is not a directory")
		}
		if _, gitErr := os.Stat(filepath.Join(worktree.Path, ".git")); gitErr != nil {
			if errors.Is(gitErr, os.ErrNotExist) {
				return worktree, staleWorkerWorktreeError(worktree, work, "path is not a usable git worktree")
			}
			return worktree, &runpipe.LoopError{Message: fmt.Sprintf("Unable to inspect worker worktree git metadata at %s for branch %s: %v", worktree.Path, firstNonEmpty(worktree.Branch, work.Branch, "unknown"), gitErr), Kind: runpipe.FailureRetryableTransient}
		}
		checkoutMode := strings.TrimSpace(worktree.CheckoutMode)
		if checkoutMode == "" {
			checkoutMode = "branch"
		}
		expectedBranch := firstNonEmpty(worktree.Branch, work.Branch)
		if checkoutMode == "detached" {
			if strings.TrimSpace(worktree.HeadSHA) == "" {
				return worktree, staleWorkerWorktreeError(worktree, work, "checkpoint detached head is not recorded")
			}
		} else if expectedBranch == "" {
			return worktree, staleWorkerWorktreeError(worktree, work, "checkpoint branch is not recorded")
		}
		if r.git == nil {
			return worktree, &runpipe.LoopError{Message: "Unable to verify worker worktree identity: git gateway is not configured", Kind: runpipe.FailureRetryableTransient}
		}
		if identityErr := r.git.VerifyWorktreeIdentity(ctx, VerifyWorktreeIdentityInput{
			RepoPath:        input.Project.RepoPath,
			WorktreeRoot:    worktreeRoot,
			WorktreePath:    worktree.Path,
			ExpectedBranch:  expectedBranch,
			ExpectedHeadSHA: strings.TrimSpace(worktree.HeadSHA),
			CheckoutMode:    checkoutMode,
		}); identityErr != nil {
			return worktree, staleWorkerWorktreeError(worktree, work, "checkout identity mismatch: "+identityErr.Error())
		}
		return worktree, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return worktree, &runpipe.LoopError{Message: fmt.Sprintf("Unable to inspect worker worktree path %s for branch %s: %v", worktree.Path, firstNonEmpty(worktree.Branch, work.Branch, "unknown"), err), Kind: runpipe.FailureRetryableTransient}
	}
	return worktree, staleWorkerWorktreeError(worktree, work, "path does not exist")
}

// verifyCheckpointWorktreeRecord detects a replacement row without making the
// mutable worktree table resume authority. A missing historical row is not
// evidence of replacement; only an active row for the checkpoint path+branch
// with a different ID proves the Worker would start in a replacement checkout.
func (r *Runner) verifyCheckpointWorktreeRecord(ctx context.Context, project storage.ProjectRecord, worktree checkpointWorktree) error {
	id := strings.TrimSpace(worktree.ID)
	if id == "" || r.repos == nil {
		return nil
	}
	records, err := r.repos.Worktrees.ListByProject(ctx, project.ID)
	if err != nil {
		return &runpipe.LoopError{Message: fmt.Sprintf("Unable to inspect worker worktree records: %v", err), Kind: runpipe.FailureRetryableTransient}
	}
	for _, record := range records {
		if record.CleanedAt != nil || record.Status == "cleaned" || record.Status == "retired" {
			continue
		}
		if storage.WorktreePathsEquivalent(record.WorktreePath, worktree.Path) && strings.TrimSpace(record.Branch) == strings.TrimSpace(worktree.Branch) && record.ID != id {
			return staleWorkerWorktreeError(worktree, workerInput{}, "checkpoint worktree record has been replaced")
		}
	}
	return nil
}

func workerWorktreeRoot(project storage.ProjectRecord) (string, error) {
	projectMetadata := parseJSONObject(project.MetadataJSON)
	worktreeRoot := stringFromAnyDefault(projectMetadata["worktreeRoot"])
	if worktreeRoot != "" {
		return worktreeRoot, nil
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

func staleWorkerWorktreeError(worktree checkpointWorktree, work workerInput, detail string) error {
	branch := firstNonEmpty(worktree.Branch, work.Branch, "unknown")
	return &runpipe.LoopError{Message: fmt.Sprintf("Worker worktree path %s for branch %s is stale (%s). Run `git -C <repo> worktree list` to find or recreate the branch worktree, then resume the worker run.", worktree.Path, branch, detail), Kind: runpipe.FailureManualIntervention}
}

func (r *Runner) runPlanStep(input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.Plan != nil {
		return checkpoint, nil
	}
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	items := compactStrings([]string{
		prefixIfPresent("Implement: ", work.Prompt),
	})
	if len(items) == 0 {
		items = []string{work.Title}
	}
	checkpoint.Plan = &checkpointPlan{Summary: work.Title, Items: items}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runExecuteStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	executionCompleted := checkpoint.Execution != nil && checkpoint.Execution.Status == "completed"
	if executionCompleted {
		if err := validateCompletedExecutionCheckpoint(checkpoint.Execution); err != nil {
			return checkpoint, err
		}
		if checkpoint.Execution.GitReconciled {
			return checkpoint, nil
		}
	}
	if !executionCompleted && !r.allowAutoCommit {
		checkpoint.SkipReason = fmt.Sprintf("Auto commit disabled; manual execution required for worker %s", input.Loop.ID)
		checkpoint.ResumePolicy = "manual_intervention"
		return checkpoint, nil
	}
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err = r.ensureWorkerWorktreeUsable(ctx, input, &checkpoint, work, worktree)
	if err != nil {
		return checkpoint, err
	}
	worktreeRoot, err := workerWorktreeRoot(input.Project)
	if err != nil {
		return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	if err := captureWorkerReproduction(&checkpoint, worktree.Path); err != nil {
		return checkpoint, err
	}
	baselineWasPresent := checkpoint.ReproductionBaseline != nil
	executeScope := reproductionBaselineScopeCreate
	if executionCompleted {
		executeScope = reproductionBaselineScopeReuseOnly
	}
	if err := r.ensureWorkerReproductionBaseline(ctx, &checkpoint, worktree.Path, input.Project.RepoPath, worktreeRoot, executeScope); err != nil {
		return checkpoint, err
	}
	if err := r.persistWorkerReproductionBaseline(ctx, input, checkpoint, baselineWasPresent); err != nil {
		return checkpoint, err
	}
	// Persist ReproductionAbsent / adopted contract before agent start so a
	// crash mid-agent cannot lose the negative observation on resume.
	if !executionCompleted {
		if err := r.persistCheckpoint(ctx, input.Run.ID, checkpoint); err != nil {
			return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
	}
	work = *checkpoint.Work
	if !executionCompleted {
		// Crash recovery: if StageHITLGateEvidence left ask.pending before
		// suspendForHuman persisted the ask, do not start another agent turn.
		// Re-enter the suspension path from the staged evidence first.
		if r.hitlEnabled {
			if awaiting, awaitErr := r.detectHumanAsk(ctx, input, worktree.Path, ""); awaitErr != nil {
				return checkpoint, awaitErr
			} else if awaiting != nil {
				return checkpoint, awaiting
			}
		}
		if err := r.verifyTimeoutProgressBeforeReplacement(ctx, input.Project, input.Run.ID, work, worktree, &checkpoint); err != nil {
			return checkpoint, err
		}
		agentVendor, agentModel, _, useSnapshot, err := r.identityFromRun(input.Run)
		if err != nil {
			return checkpoint, fmt.Errorf("resolve run agent identity: %w", err)
		}
		validationCommands := workerValidationCommands(r.validationCommandsForProject(input.Project.ID), work)
		prompt, instructionBlock, err := buildWorkerPromptWithInstructions(worktree.Path, input.Project.ID, r.customInstructions, work, checkpoint.Plan, r.canAgentCreatePR(ctx, input.Project.ID, work, input.Project.RepoPath), r.disclosure, agentVendor, derefString(agentModel))
		if err != nil {
			return checkpoint, err
		}
		if len(validationCommands) > 0 {
			prompt += validationGatedLocalOnlyPrompt
		}
		if work.Reproduction != nil {
			prompt += "\n\n" + work.Reproduction.PromptInstruction()
		}
		// HITL (gated): let the agent pause to ask a human, and on resume feed the
		// human's answer back into the same agent session.
		nativeResumePrompt := ""
		// Snapshot the inbox actually included in this prompt: acknowledgement
		// after the turn must remove exactly these messages, not ones that
		// arrive while the agent is running.
		includedHumanInbox := loops.ReadHumanInbox(input.Loop.MetadataJSON)
		nativeSessionID := ""
		if r.hitlEnabled {
			prompt += hitlPromptInstruction
			nativeResumePrompt, nativeSessionID = r.pendingHumanAnswer(ctx, &input.Loop, agentVendor)
			if nativeResumePrompt != "" {
				prompt += "\n\n" + nativeResumePrompt
			}
		}
		// Handed back from an interactive human takeover: resume the exact session
		// the human drove (independent of HITL) so the daemon sees their turns.
		if nativeSessionID == "" {
			if takeoverPrompt, takeoverSession := r.pendingTakeoverResume(ctx, &input.Loop, agentVendor); takeoverPrompt != "" {
				nativeResumePrompt = takeoverPrompt
				nativeSessionID = takeoverSession
				prompt += "\n\n" + takeoverPrompt
			}
		}
		// Free-text human messages queued in the thread at any time (a follow-up
		// question, a new instruction, or an answer to interpret) — drain them into
		// this turn, resuming the same session so the agent has the full
		// conversation. Conversational: the agent may answer + ask again rather than
		// treat them as a final decision.
		if r.hitlEnabled {
			if inbox := includedHumanInbox; len(inbox) > 0 {
				var msgs strings.Builder
				msgs.WriteString("While you were working, the human sent these messages in the task thread:")
				for _, m := range inbox {
					if t := strings.TrimSpace(m.Text); t != "" {
						msgs.WriteString("\n- ")
						msgs.WriteString(t)
					}
				}
				msgs.WriteString("\nRead them in context and respond appropriately: if a message answers a question you asked, proceed using it; if it is a follow-up question or a new instruction, address it — and if you still need a human decision, ask again (write .looper/ask.json) with your response to what they said. Do not ignore these messages.")
				prompt += "\n\n" + msgs.String()
				if nativeSessionID == "" {
					nativeSessionID = r.latestNativeSessionID(ctx, input.Loop.ID, agentVendor)
				}
			}
		}
		executionID := eventlog.NewEventID("agent")
		metadata := map[string]any{"loopType": "worker", "title": work.Title, "repo": work.Repo, "baseBranch": work.BaseBranch}
		for key, value := range config.CustomInstructionMetadata(instructionBlock, prompt) {
			metadata[key] = value
		}
		holdWork, retargetedToPullRequest := workerHoldInputForTarget(work, input.Loop, input.QueueItem)
		if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
			return checkpoint, err
		} else if held {
			return checkpoint, &runpipe.HoldSkipError{Summary: summary}
		}
		useSnap, snapVendor, snapModel := agentRunSnapshotFields(agentVendor, agentModel, useSnapshot)
		var (
			preTimeoutProgress *worktreeProgress
			progressMu         sync.Mutex
		)
		onBeforeTimeout := func(timeoutCtx context.Context, observation agent.TimeoutObservation) error {
			progress, progressErr := r.captureWorktreeProgress(timeoutCtx, input.Project, work, worktree, observation, nil)
			progressMu.Lock()
			defer progressMu.Unlock()
			if progressErr != nil {
				checkpoint.Execution = &checkpointExecution{Status: "timeout_observing", ProgressSnapshotError: progressErr.Error()}
				checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
				if persistErr := r.persistCheckpoint(timeoutCtx, input.Run.ID, checkpoint); persistErr != nil {
					return errors.Join(progressErr, fmt.Errorf("persist pre-timeout worker progress failure: %w", persistErr))
				}
				return progressErr
			}
			preTimeoutProgress = &progress
			checkpoint.Execution = &checkpointExecution{Status: "timeout_observing", ProgressBeforeTimeout: &progress}
			checkpoint.ResumePolicy = "retry_from_timeout_context"
			if err := r.persistCheckpoint(timeoutCtx, input.Run.ID, checkpoint); err != nil {
				return fmt.Errorf("persist pre-timeout worker progress: %w", err)
			}
			return nil
		}
		execution, err := r.agentExecutor.Start(ctx, AgentRunInput{
			ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID,
			Prompt: prompt, NativeResumePrompt: nativeResumePrompt, NativeSessionID: nativeSessionID,
			WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout,
			Metadata: metadata, IdempotencyKey: fmt.Sprintf("worker:%s", input.Loop.ID),
			RestrictToolNetwork: len(validationCommands) > 0,
			OnBeforeTimeout:     onBeforeTimeout,
			UseSnapshot:         useSnap, SnapshotVendor: snapVendor, SnapshotModel: snapModel,
		})
		if err != nil {
			return checkpoint, err
		}
		if r.onAgentExecutionStarted != nil {
			_ = r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: work.Title, Body: "Worker started", DedupeKey: fmt.Sprintf("runtime.agent.started:worker:%s", input.Run.ID)})
		}
		result, err := execution.Wait(ctx)
		progressMu.Lock()
		defer progressMu.Unlock()
		if err != nil {
			return checkpoint, err
		}
		// HITL (gated): after the agent turn, if it wrote an ask sentinel, suspend
		// the run so a human can answer. Returned as a typed error the step loop
		// converts into an awaiting_human suspension (not a failure).
		if r.hitlEnabled && result.Status == "completed" {
			if awaiting, awaitErr := r.detectHumanAsk(ctx, input, worktree.Path, executionID); awaitErr != nil {
				return checkpoint, awaitErr
			} else if awaiting != nil {
				return checkpoint, awaiting
			}
		}
		if result.Status != "completed" {
			checkpoint.Execution = checkpointExecutionFromAgentResult(result)
			checkpoint.Execution.ProgressBeforeTimeout = preTimeoutProgress
			if result.PreTimeoutError != "" {
				checkpoint.Execution.ProgressSnapshotError = result.PreTimeoutError
				checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
				if err := r.persistCheckpoint(ctx, input.Run.ID, checkpoint); err != nil {
					return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
				}
				return checkpoint, &runpipe.LoopError{Message: "Worker timeout progress snapshot failed: " + result.PreTimeoutError, Kind: runpipe.FailureManualIntervention}
			}
			if result.Status == "timeout" && preTimeoutProgress != nil {
				if err := r.verifyTimeoutProgressAfterTermination(ctx, input.Project, work, worktree, *preTimeoutProgress); err != nil {
					checkpoint.Execution.ProgressSnapshotError = err.Error()
					checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
					if persistErr := r.persistCheckpoint(ctx, input.Run.ID, checkpoint); persistErr != nil {
						return checkpoint, &runpipe.LoopError{Message: persistErr.Error(), Kind: runpipe.FailureRetryableAfterResume}
					}
					return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureManualIntervention}
				}
			}
			checkpoint.ResumePolicy = "retry_from_timeout_context"
			if err := r.persistCheckpoint(ctx, input.Run.ID, checkpoint); err != nil {
				return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
			}
			message := firstNonEmpty(result.Summary, result.Stderr, fmt.Sprintf("Worker agent %s", result.Status))
			kind := runpipe.FailureRetryableTransient
			if agent.IsAgentSetupFailureMessage(message) {
				kind = runpipe.FailureRetryableTransient
			}
			return checkpoint, &runpipe.LoopError{Message: message, Kind: kind}
		}
		if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
			return checkpoint, err
		} else if held {
			return checkpoint, &runpipe.HoldSkipError{Summary: summary}
		}
		// HITL (gated): the resumed turn completed without asking again, so the human
		// answer that seeded it has been acted on. Flip it to "consumed" now — after
		// the turn, never before — so a failed/timed-out turn re-reads the answer on
		// retry, while a successful one never re-injects it on a later run.
		r.acknowledgePostTurnMetadata(ctx, &input.Loop, includedHumanInbox)
		if err := validateCompletedExecutionCheckpoint(&checkpointExecution{Status: result.Status, Summary: result.Summary, ParseStatus: result.ParseStatus}); err != nil {
			return checkpoint, err
		}
		checkpoint.Execution = checkpointExecutionFromAgentResult(result)
		checkpoint.ensureLifecycle("worker", worktree.Branch, worktree.BaseBranch, work.ExecutionMode == "create-pr")
		if err := verifyWorkerReproduction(checkpoint, worktree.Path); err != nil {
			return checkpoint, err
		}
		if result.Lifecycle != nil {
			checkpoint.Lifecycle.MergeAgent(result.Lifecycle, r.nowISO())
		} else if len(result.Commits) > 0 {
			checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, result.Commits...)
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceAgent
		}
	}
	checkpoint.ensureLifecycle("worker", worktree.Branch, worktree.BaseBranch, work.ExecutionMode == "create-pr")
	if err := r.persistCheckpoint(ctx, input.Run.ID, checkpoint); err != nil {
		return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	if _, err := r.reconcileWorkerGitState(ctx, &checkpoint, input.Project, work, worktree, input.Run); err != nil {
		return checkpoint, err
	}
	if err := verifyWorkerReproduction(checkpoint, worktree.Path); err != nil {
		return checkpoint, err
	}
	checkpoint.Execution.GitReconciled = true
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) captureWorktreeProgress(ctx context.Context, project storage.ProjectRecord, work workerInput, worktree checkpointWorktree, observation agent.TimeoutObservation, compare *worktreeProgress) (worktreeProgress, error) {
	if r.git == nil {
		return worktreeProgress{}, fmt.Errorf("worker git gateway is not configured")
	}
	worktreeRoot, err := workerWorktreeRoot(project)
	if err != nil {
		return worktreeProgress{}, err
	}
	contentPaths := []string(nil)
	compareHeadSHA := ""
	if compare != nil {
		contentPaths = append(contentPaths, compare.ChangedFiles...)
		compareHeadSHA = compare.HeadSHA
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: firstNonEmpty(worktree.HeadSHA, worktree.BaseBranch, work.BaseBranch), ContentPaths: contentPaths, CompareHeadSHA: compareHeadSHA})
	if err != nil {
		return worktreeProgress{}, err
	}
	return worktreeProgress{
		HeadSHA:            inspect.HeadSHA,
		WorktreeID:         worktree.ID,
		Branch:             inspect.Branch,
		ChangedFiles:       boundPathList(inspect.ChangedFiles, worktreeProgressPathCap),
		ChangedFileCount:   len(inspect.ChangedFiles),
		StagedFiles:        boundPathList(inspect.StagedFiles, worktreeProgressPathCap),
		StagedFileCount:    len(inspect.StagedFiles),
		UntrackedFiles:     boundPathList(inspect.UntrackedFiles, worktreeProgressPathCap),
		UntrackedFileCount: len(inspect.UntrackedFiles),
		DiffFingerprint:    inspect.DiffFingerprint, ContentFingerprint: inspect.ContentFingerprint, HeadDescendsFromCompare: inspect.HeadDescendsFromCompare,
		TimeoutType:        observation.TimeoutType,
		LastProgressAt:     observation.LastProgressAt,
		CapturedAt:         eventlog.FormatJavaScriptISOString(r.now().UTC()),
	}, nil
}

// worktreeProgressPathCap bounds path lists stored in checkpoint_json. Counts
// still reflect the full InspectHead result; only the sample is truncated.
const worktreeProgressPathCap = 64

func boundPathList(paths []string, capN int) []string {
	if capN <= 0 || len(paths) <= capN {
		return append([]string(nil), paths...)
	}
	return append([]string(nil), paths[:capN]...)
}

func (r *Runner) verifyTimeoutProgressAfterTermination(ctx context.Context, project storage.ProjectRecord, work workerInput, worktree checkpointWorktree, before worktreeProgress) error {
	after, err := r.captureWorktreeProgress(ctx, project, work, worktree, agent.TimeoutObservation{}, &before)
	if err != nil {
		return fmt.Errorf("worker timeout progress verification after termination failed: %w", err)
	}
	if !preservesWorktreeProgress(before, after) {
		return fmt.Errorf("worker timeout progress changed after termination; operator action is required before retry")
	}
	return nil
}

func (r *Runner) verifyTimeoutProgressBeforeReplacement(ctx context.Context, project storage.ProjectRecord, runID string, work workerInput, worktree checkpointWorktree, checkpoint *workerCheckpoint) error {
	if checkpoint.Execution == nil || checkpoint.Execution.Status != "timeout" || checkpoint.Execution.ProgressBeforeTimeout == nil {
		return nil
	}
	current, err := r.captureWorktreeProgress(ctx, project, work, worktree, agent.TimeoutObservation{}, checkpoint.Execution.ProgressBeforeTimeout)
	if err != nil {
		return &runpipe.LoopError{Message: fmt.Sprintf("worker timeout progress verification before retry failed: %v", err), Kind: runpipe.FailureManualIntervention}
	}
	if preservesWorktreeProgress(*checkpoint.Execution.ProgressBeforeTimeout, current) {
		return nil
	}
	checkpoint.Execution.ProgressSnapshotError = "worker timeout progress changed before replacement; operator action is required before retry"
	checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
	if err := r.persistCheckpoint(ctx, runID, *checkpoint); err != nil {
		return &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	return &runpipe.LoopError{Message: checkpoint.Execution.ProgressSnapshotError, Kind: runpipe.FailureManualIntervention}
}

func sameWorktreeProgress(before, after worktreeProgress) bool {
	return before.HeadSHA == after.HeadSHA && before.Branch == after.Branch && before.DiffFingerprint == after.DiffFingerprint
}

func preservesWorktreeProgress(before, after worktreeProgress) bool {
	if before.ChangedFileCount == 0 {
		return true
	}
	if sameWorktreeProgress(before, after) {
		return true
	}
	return before.HeadSHA != "" && after.HeadSHA != "" && before.HeadSHA != after.HeadSHA && after.ChangedFileCount == 0 && after.HeadDescendsFromCompare && before.ContentFingerprint == after.ContentFingerprint
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}


func (r *Runner) reconcileWorkerGitState(ctx context.Context, checkpoint *workerCheckpoint, project storage.ProjectRecord, work workerInput, worktree checkpointWorktree, run storage.RunRecord) (bool, error) {
	checkpoint.ensureLifecycle("worker", worktree.Branch, worktree.BaseBranch, work.ExecutionMode == "create-pr")
	if r.git == nil {
		return false, nil
	}
	baseRef := firstNonEmpty(worktree.HeadSHA, worktree.BaseBranch)
	worktreeRoot, rootErr := workerWorktreeRoot(project)
	if rootErr != nil {
		return false, &runpipe.LoopError{Message: rootErr.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: baseRef})
	if err != nil {
		checkpoint.Lifecycle.LastError = err.Error()
		return false, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	if !inspect.HasUncommittedChanges {
		knownCommitCount := len(checkpoint.Lifecycle.CommitSHAs)
		checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, inspect.NewCommitSHAs...)
		observedNewCommits := len(checkpoint.Lifecycle.CommitSHAs) > knownCommitCount
		if len(inspect.NewCommitSHAs) > 0 && checkpoint.Lifecycle.Actions.Commit == lifecycle.ActionSourceNone {
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceAgent
		}
		checkpoint.Lifecycle.ReconciledAt = r.nowISO()
		checkpoint.Lifecycle.ReconciledBy = "worker"
		checkpoint.Lifecycle.LastError = ""
		checkpoint.Lifecycle.Normalize()
		return observedNewCommits, nil
	}
	if !r.allowAutoCommit {
		message := fmt.Sprintf("Worker worktree has uncommitted changes before PR push for branch %s", firstNonEmpty(work.Branch, worktree.Branch))
		checkpoint.Lifecycle.LastError = message
		return false, &runpipe.LoopError{Message: message, Kind: runpipe.FailureManualIntervention}
	}
	agent, model := r.disclosureIdentity(run)
	committed, err := r.git.Commit(ctx, CommitInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Message: buildWorkerFallbackCommitMessage(work), DisclosureAgent: agent, DisclosureModel: model})
	if err != nil {
		checkpoint.Lifecycle.LastError = err.Error()
		return false, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	finalInspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: baseRef})
	if err != nil {
		checkpoint.Lifecycle.LastError = err.Error()
		return false, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	if finalInspect.HasUncommittedChanges {
		message := fmt.Sprintf("Worker fallback commit left uncommitted changes in branch %s", firstNonEmpty(work.Branch, worktree.Branch))
		checkpoint.Lifecycle.LastError = message
		return false, &runpipe.LoopError{Message: message, Kind: runpipe.FailureManualIntervention}
	}
	checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, inspect.NewCommitSHAs...)
	checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, finalInspect.NewCommitSHAs...)
	if committed.CommitSHA != "" {
		checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, committed.CommitSHA)
	}
	checkpoint.Lifecycle.Pushed = false
	checkpoint.Lifecycle.Actions.Push = lifecycle.ActionSourceNone
	checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceFallback
	checkpoint.Lifecycle.ReconciledAt = r.nowISO()
	checkpoint.Lifecycle.ReconciledBy = "worker"
	checkpoint.Lifecycle.LastError = ""
	checkpoint.Lifecycle.Normalize()
	return true, nil
}

func (r *Runner) runValidateStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	var validationCommands []string
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err = r.ensureWorkerWorktreeUsable(ctx, input, &checkpoint, work, worktree)
	if err != nil {
		return checkpoint, err
	}
	worktreeRoot, err := workerWorktreeRoot(input.Project)
	if err != nil {
		return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	if err := captureWorkerReproduction(&checkpoint, worktree.Path); err != nil {
		return checkpoint, err
	}
	baselineWasPresent := checkpoint.ReproductionBaseline != nil
	if err := r.ensureWorkerReproductionBaseline(ctx, &checkpoint, worktree.Path, input.Project.RepoPath, worktreeRoot, reproductionBaselineScopeReuseOnly); err != nil {
		return checkpoint, err
	}
	if err := r.persistWorkerReproductionBaseline(ctx, input, checkpoint, baselineWasPresent); err != nil {
		return checkpoint, err
	}
	work = *checkpoint.Work
	validationCommands = workerValidationCommands(r.validationCommandsForProject(input.Project.ID), work)
	result, err := r.runValidation(ctx, ValidationInput{CWD: worktree.Path, Commands: validationCommands})
	if err != nil {
		return checkpoint, err
	}
	if err := verifyWorkerReproduction(checkpoint, worktree.Path); err != nil {
		return checkpoint, err
	}
	checkpoint.Validation = &result
	if !result.Passed {
		failure := classifyValidationFailure(result)
		checkpoint.ResumePolicy = failure.resumePolicy
		return checkpoint, &runpipe.LoopError{Message: failure.message, Kind: failure.kind}
	}
	reconciled, err := r.reconcileWorkerGitState(ctx, &checkpoint, input.Project, work, worktree, input.Run)
	if err != nil {
		return checkpoint, err
	}
	if reconciled {
		result, err = r.runValidation(ctx, ValidationInput{CWD: worktree.Path, Commands: validationCommands})
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Validation = &result
		if !result.Passed {
			failure := classifyValidationFailure(result)
			checkpoint.ResumePolicy = failure.resumePolicy
			return checkpoint, &runpipe.LoopError{Message: failure.message, Kind: failure.kind}
		}
		if err := verifyWorkerReproduction(checkpoint, worktree.Path); err != nil {
			return checkpoint, err
		}
		worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		inspect, inspectErr := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: firstNonEmpty(worktree.HeadSHA, worktree.BaseBranch)})
		if inspectErr != nil {
			return checkpoint, inspectErr
		}
		if inspect.HasUncommittedChanges {
			checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
			return checkpoint, &runpipe.LoopError{Message: "Validation keeps producing uncommitted changes after an extra reconcile pass", Kind: runpipe.FailureManualIntervention}
		}
	}
	worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	inspect, inspectErr := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: firstNonEmpty(worktree.HeadSHA, worktree.BaseBranch)})
	if inspectErr != nil {
		return checkpoint, inspectErr
	}
	if inspect.HasUncommittedChanges {
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &runpipe.LoopError{Message: "Validation left uncommitted changes after reconcile", Kind: runpipe.FailureManualIntervention}
	}
	result.HeadSHA = inspect.HeadSHA
	checkpoint.Validation = &result
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

func (r *Runner) runOpenPRStep(ctx context.Context, input stepInput) (workerCheckpoint, error) {
	checkpoint := input.Checkpoint
	var validationCommands []string
	work, err := requireWork(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	holdWork, retargetedToPullRequest := workerHoldInputForTarget(work, input.Loop, input.QueueItem)
	if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
		return checkpoint, err
	} else if held {
		return checkpoint, &runpipe.HoldSkipError{Summary: summary}
	}
	if checkpoint.Validation != nil && !checkpoint.Validation.Passed {
		failure := classifyValidationFailure(*checkpoint.Validation)
		checkpoint.ResumePolicy = failure.resumePolicy
		return checkpoint, &runpipe.LoopError{Message: failure.message, Kind: failure.kind}
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err = r.ensureWorkerWorktreeUsable(ctx, input, &checkpoint, work, worktree)
	if err != nil {
		return checkpoint, err
	}
	worktreeRoot, err := workerWorktreeRoot(input.Project)
	if err != nil {
		return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	if err := captureWorkerReproduction(&checkpoint, worktree.Path); err != nil {
		return checkpoint, err
	}
	baselineWasPresent := checkpoint.ReproductionBaseline != nil
	if err := r.ensureWorkerReproductionBaseline(ctx, &checkpoint, worktree.Path, input.Project.RepoPath, worktreeRoot, reproductionBaselineScopeReuseOnly); err != nil {
		return checkpoint, err
	}
	if err := r.persistWorkerReproductionBaseline(ctx, input, checkpoint, baselineWasPresent); err != nil {
		return checkpoint, err
	}
	work = *checkpoint.Work
	validationCommands = workerValidationCommands(r.validationCommandsForProject(input.Project.ID), work)
	if err := verifyWorkerReproduction(checkpoint, worktree.Path); err != nil {
		return checkpoint, err
	}
	if err := r.validateWorkerIssueStillOpen(ctx, input.Project.RepoPath, work); err != nil {
		if isWorkerIssueTargetObsolete(err) {
			checkpoint.SkipReason = fmt.Sprintf("Worker stopped because %s is no longer an open issue", formatIssueReference(issueLookupRepo(work), work.IssueNumber))
			checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
			return checkpoint, nil
		}
		return checkpoint, err
	}
	reconciled, err := r.reconcileWorkerGitState(ctx, &checkpoint, input.Project, work, worktree, input.Run)
	if err != nil {
		return checkpoint, err
	}
	if reconciled && len(validationCommands) > 0 {
		result, validationErr := r.runValidation(ctx, ValidationInput{CWD: worktree.Path, Commands: validationCommands})
		if validationErr != nil {
			return checkpoint, validationErr
		}
		checkpoint.Validation = &result
		if !result.Passed {
			failure := classifyValidationFailure(result)
			checkpoint.ResumePolicy = failure.resumePolicy
			return checkpoint, &runpipe.LoopError{Message: failure.message, Kind: failure.kind}
		}
		worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		inspect, inspectErr := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: firstNonEmpty(worktree.HeadSHA, worktree.BaseBranch)})
		if inspectErr != nil {
			return checkpoint, inspectErr
		}
		if inspect.HasUncommittedChanges {
			checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
			return checkpoint, &runpipe.LoopError{Message: "Validation produced uncommitted changes immediately before publishing", Kind: runpipe.FailureManualIntervention}
		}
		result.HeadSHA = inspect.HeadSHA
		checkpoint.Validation = &result
		// Validation can rewrite reproduction files; re-check integrity at the
		// head that is about to be published.
		if err := verifyWorkerReproduction(checkpoint, worktree.Path); err != nil {
			return checkpoint, err
		}
	}
	if len(validationCommands) > 0 {
		worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		inspect, inspectErr := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: firstNonEmpty(worktree.HeadSHA, worktree.BaseBranch)})
		if inspectErr != nil {
			return checkpoint, inspectErr
		}
		if checkpoint.Validation == nil || !checkpoint.Validation.Passed || checkpoint.Validation.HeadSHA != inspect.HeadSHA || inspect.HasUncommittedChanges {
			input.Checkpoint = checkpoint
			checkpoint, err = r.runValidateStep(ctx, input)
			if err != nil {
				return checkpoint, err
			}
		}
	}
	if err := r.persistCheckpoint(ctx, input.Run.ID, checkpoint); err != nil {
		return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	if work.ExecutionMode == "create-pr" && checkpoint.PullRequest == nil {
		pr, branch, ok, err := r.lifecycleAgentCreatedPullRequest(ctx, input.Loop.ID, work.Repo, checkpoint.Lifecycle, worktree.Branch, work.BaseBranch, input.Project.RepoPath)
		if err != nil {
			var loopErr *runpipe.LoopError
			if errors.As(err, &loopErr) {
				if input.Loop.PRNumber != nil {
					loopErr = nil
				} else {
					if loopErr.Kind == runpipe.FailureManualIntervention {
						checkpoint.SkipReason = loopErr.Message
						checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
					}
					return checkpoint, loopErr
				}
			}
			if err != nil && input.Loop.PRNumber == nil {
				return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
			}
		}
		if ok {
			checkpoint.PullRequest = &pr
			checkpoint.markLifecycleAgentPullRequest(branch, work.BaseBranch, pr)
		}
	}
	if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
		return checkpoint, err
	} else if held {
		return checkpoint, &runpipe.HoldSkipError{Summary: summary}
	}
	if work.ExecutionMode == "create-pr" && (checkpoint.PullRequest != nil || input.Loop.PRNumber != nil) {
		if checkpoint.PullRequest == nil {
			checkpoint.PullRequest = &checkpointPullPR{Number: derefInt64(input.Loop.PRNumber), URL: stringFromAnyDefault(parseJSONObject(input.Loop.MetadataJSON)["prUrl"])}
		}
		validatedHeadSHA := workerValidatedHeadSHA(checkpoint, validationCommands)
		publishedHeadMismatch := validatedHeadSHA != "" && strings.TrimSpace(checkpoint.PullRequest.HeadSHA) != validatedHeadSHA
		pushedByFallback := false
		if !checkpoint.Lifecycle.Pushed || publishedHeadMismatch {
			if !r.allowAutoPush {
				message := fmt.Sprintf("Auto push disabled; manual PR opening required for worker %s", input.Loop.ID)
				checkpoint.SkipReason = message
				checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
				return checkpoint, &runpipe.LoopError{Message: message, Kind: runpipe.FailureManualIntervention}
			}
			worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
			if rootErr != nil {
				return checkpoint, rootErr
			}
			publishBranch := worktree.Branch
			if checkpoint.Lifecycle != nil {
				publishBranch = firstNonEmpty(checkpoint.Lifecycle.ActiveBranch, checkpoint.Lifecycle.AgentBranch, publishBranch)
			}
			expectedRemoteHeadSHA := ""
			if publishedHeadMismatch {
				expectedRemoteHeadSHA = strings.TrimSpace(checkpoint.PullRequest.HeadSHA)
			}
			if err := r.git.Push(ctx, PushInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Branch: publishBranch, LocalHeadSHA: validatedHeadSHA, ExpectedRemoteHeadSHA: expectedRemoteHeadSHA, ProtectedBranches: compactStrings([]string{work.BaseBranch})}); err != nil {
				return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
			}
			checkpoint.PullRequest.HeadSHA = validatedHeadSHA
			pushedByFallback = true
		}
		checkpoint.markLifecyclePushAndPR(worktree.Branch, work.BaseBranch, checkpoint.PullRequest.Number, checkpoint.PullRequest.URL, pushedByFallback, input.Loop.PRNumber != nil)
		holdWork := work
		holdWork.PRNumber = checkpoint.PullRequest.Number
		if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
			return checkpoint, err
		} else if held {
			return checkpoint, &runpipe.HoldSkipError{Summary: summary}
		}
		if shouldPersistPullRequestReference(input.Loop, *checkpoint.PullRequest) {
			if err := r.persistPullRequestReference(ctx, input.Loop, input.QueueItem, work.Repo, *checkpoint.PullRequest); err != nil {
				return checkpoint, err
			}
		}
		if err := r.normalizePullRequestDisclosure(ctx, input.Run, work.Repo, checkpoint.PullRequest.Number, input.Project.RepoPath, checkpoint.Lifecycle != nil && checkpoint.Lifecycle.Actions.PR == lifecycle.ActionSourceAgent); err != nil {
			return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		r.syncIssueClaim(ctx, input, &checkpoint, issueClaimStatusPRLinked, "")
		return checkpoint, nil
	}
	if work.ExecutionMode == "push-existing" {
		if !r.allowAutoPush {
			message := fmt.Sprintf("Auto push disabled; manual PR opening required for worker %s", input.Loop.ID)
			checkpoint.SkipReason = message
			checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
			return checkpoint, &runpipe.LoopError{Message: message, Kind: runpipe.FailureManualIntervention}
		}
		worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		if err := r.git.Push(ctx, PushInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Branch: firstNonEmpty(work.Branch, worktree.Branch), LocalHeadSHA: workerValidatedHeadSHA(checkpoint, validationCommands), ProtectedBranches: compactStrings([]string{work.BaseBranch})}); err != nil {
			return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
		prURL := stringFromAnyDefault(parseJSONObject(input.Loop.MetadataJSON)["prUrl"])
		checkpoint.PullRequest = &checkpointPullPR{Number: work.PRNumber, URL: prURL}
		checkpoint.markLifecyclePushAndPR(firstNonEmpty(work.Branch, worktree.Branch), work.BaseBranch, work.PRNumber, prURL, true, false)
		if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
			return checkpoint, err
		} else if held {
			return checkpoint, &runpipe.HoldSkipError{Summary: summary}
		}
		_ = r.renamePlannerSpecPullRequestAfterTakeover(ctx, work, input.Project.RepoPath)
		if err := r.normalizePullRequestDisclosure(ctx, input.Run, work.Repo, work.PRNumber, input.Project.RepoPath, false); err != nil {
			return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
		if len(work.Reviewers) > 0 && work.PRNumber > 0 && r.github != nil {
			_ = r.github.AddPullRequestReviewers(ctx, PullRequestReviewersInput{Repo: work.Repo, PRNumber: work.PRNumber, Reviewers: append([]string(nil), work.Reviewers...), CWD: input.Project.RepoPath})
		}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		r.syncIssueClaim(ctx, input, &checkpoint, issueClaimStatusPRLinked, "")
		return checkpoint, nil
	}
	if r.openPRStrategy == config.OpenPRStrategyManual {
		checkpoint.SkipReason = fmt.Sprintf("Worker completed; PR opening is manual for %s", input.Loop.ID)
		checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
		return checkpoint, nil
	}
	if !r.githubCLIAutoPROpeningAvailable(ctx, work.Repo, input.Project.RepoPath) {
		message := fmt.Sprintf("GitHub CLI unavailable; PR opening is manual for worker %s", input.Loop.ID)
		checkpoint.SkipReason = message
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &runpipe.LoopError{Message: message, Kind: runpipe.FailureManualIntervention}
	}
	if !r.allowAutoPush {
		message := fmt.Sprintf("Auto push disabled; manual PR opening required for worker %s", input.Loop.ID)
		checkpoint.SkipReason = message
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &runpipe.LoopError{Message: message, Kind: runpipe.FailureManualIntervention}
	}
	aliases := buildWorkerBranchAliases(work, input.Loop.ID)
	worktreeRoot, rootErr := workerWorktreeRoot(input.Project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	if existing, err := r.findOpenPullRequestForBranch(ctx, work.Repo, aliases, work.BaseBranch, input.Project.RepoPath); err == nil && existing != nil {
		if err := r.git.Push(ctx, PushInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Branch: firstNonEmpty(existing.HeadRefName, worktree.Branch), LocalHeadSHA: workerValidatedHeadSHA(checkpoint, validationCommands), ProtectedBranches: compactStrings([]string{work.BaseBranch})}); err != nil {
			if shouldRestartWorkerFromDiscoverAfterPushFailure(err) {
				checkpoint.ResumePolicy = loops.ResumePolicyRestartFromDiscover
			}
			return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
		checkpoint.PullRequest = &checkpointPullPR{Number: existing.Number, URL: existing.URL}
		checkpoint.markLifecyclePushAndPR(firstNonEmpty(existing.HeadRefName, worktree.Branch), work.BaseBranch, existing.Number, existing.URL, true, true)
		adoptedWork := workerWorkForPullRequest(work, *existing)
		if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, adoptedWork, true); err != nil {
			return checkpoint, err
		} else if held {
			return checkpoint, &runpipe.HoldSkipError{Summary: summary}
		}
		_ = r.assignReviewersIfNeeded(ctx, work, existing.Number, input.Project.RepoPath)
		if err := r.normalizePullRequestDisclosure(ctx, input.Run, work.Repo, existing.Number, input.Project.RepoPath, true); err != nil {
			return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
		if err := r.persistPullRequestReference(ctx, input.Loop, input.QueueItem, work.Repo, checkpointPullPR{Number: existing.Number, URL: existing.URL}); err != nil {
			return checkpoint, err
		}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		r.syncIssueClaim(ctx, input, &checkpoint, issueClaimStatusPRLinked, "")
		return checkpoint, nil
	}
	if err := r.git.Push(ctx, PushInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Branch: worktree.Branch, LocalHeadSHA: workerValidatedHeadSHA(checkpoint, validationCommands), ProtectedBranches: compactStrings([]string{work.BaseBranch})}); err != nil {
		if shouldRestartWorkerFromDiscoverAfterPushFailure(err) {
			checkpoint.ResumePolicy = loops.ResumePolicyRestartFromDiscover
		}
		return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	checkpoint.markLifecyclePushAndPR(worktree.Branch, work.BaseBranch, 0, "", true, false)
	if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, holdWork, retargetedToPullRequest); err != nil {
		return checkpoint, err
	} else if held {
		return checkpoint, &runpipe.HoldSkipError{Summary: summary}
	}
	ahead, err := r.workerBranchAheadOfBase(ctx, input.Project, work, worktree)
	if err != nil {
		return checkpoint, err
	}
	if !ahead {
		checkpoint.SkipReason = fmt.Sprintf("Worker stopped because branch %s has no commits ahead of %s", worktree.Branch, work.BaseBranch)
		checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
		return checkpoint, nil
	}
	if existing, err := r.findOpenPullRequestForBranch(ctx, work.Repo, aliases, work.BaseBranch, input.Project.RepoPath); err == nil && existing != nil {
		adoptedWork := workerWorkForPullRequest(work, *existing)
		if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, adoptedWork, true); err != nil {
			return checkpoint, err
		} else if held {
			checkpoint.PullRequest = &checkpointPullPR{Number: existing.Number, URL: existing.URL}
			checkpoint.markLifecyclePushAndPR(firstNonEmpty(existing.HeadRefName, worktree.Branch), work.BaseBranch, existing.Number, existing.URL, true, true)
			return checkpoint, &runpipe.HoldSkipError{Summary: summary}
		}
		_ = r.assignReviewersIfNeeded(ctx, work, existing.Number, input.Project.RepoPath)
		if err := r.normalizePullRequestDisclosure(ctx, input.Run, work.Repo, existing.Number, input.Project.RepoPath, true); err != nil {
			return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
		}
		if err := r.persistPullRequestReference(ctx, input.Loop, input.QueueItem, work.Repo, checkpointPullPR{Number: existing.Number, URL: existing.URL}); err != nil {
			return checkpoint, err
		}
		checkpoint.PullRequest = &checkpointPullPR{Number: existing.Number, URL: existing.URL}
		checkpoint.markLifecyclePushAndPR(firstNonEmpty(existing.HeadRefName, worktree.Branch), work.BaseBranch, existing.Number, existing.URL, true, true)
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		r.syncIssueClaim(ctx, input, &checkpoint, issueClaimStatusPRLinked, "")
		return checkpoint, nil
	}
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	created, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{
		Repo: work.Repo, HeadBranch: worktree.Branch, BaseBranch: work.BaseBranch, Title: work.Title,
		Body: r.stampPullRequestDisclosure(input.Run, buildPullRequestBody(work, checkpoint.Plan, checkpoint.Execution)),
		CWD:  input.Project.RepoPath, DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel,
	})
	if err != nil {
		return checkpoint, &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	if created.Number <= 0 {
		return checkpoint, &runpipe.LoopError{Message: "Worker create-pr requires a pull request number", Kind: runpipe.FailureRetryableAfterResume}
	}
	pr := checkpointPullPR{Number: created.Number, URL: created.URL}
	createdWork := workerWorkForPullRequest(work, PullRequestSummary{Number: created.Number, URL: created.URL, HeadRefName: worktree.Branch, BaseRefName: work.BaseBranch})
	if held, summary, err := r.workerHoldSummaryForWork(ctx, input.Project, createdWork); err != nil {
		return checkpoint, err
	} else if held {
		checkpoint.PullRequest = &pr
		checkpoint.markLifecyclePushAndPR(worktree.Branch, work.BaseBranch, created.Number, created.URL, true, false)
		return checkpoint, &runpipe.HoldSkipError{Summary: summary}
	}
	_ = r.assignReviewersIfNeeded(ctx, work, created.Number, input.Project.RepoPath)
	if err := r.persistPullRequestReference(ctx, input.Loop, input.QueueItem, work.Repo, pr); err != nil {
		return checkpoint, err
	}
	checkpoint.PullRequest = &pr
	checkpoint.markLifecyclePushAndPR(worktree.Branch, work.BaseBranch, created.Number, created.URL, true, false)
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	r.syncIssueClaim(ctx, input, &checkpoint, issueClaimStatusPRLinked, "")
	return checkpoint, nil
}

func workerValidatedHeadSHA(checkpoint workerCheckpoint, validationCommands []string) string {
	if len(validationCommands) == 0 || checkpoint.Validation == nil || !checkpoint.Validation.Passed {
		return ""
	}
	return strings.TrimSpace(checkpoint.Validation.HeadSHA)
}

func (r *Runner) workerHoldSummary(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint workerCheckpoint) (bool, string, error) {
	work, err := r.resolveWorkerInput(ctx, project, loop, queueItem, checkpoint)
	if err != nil {
		return false, "", err
	}
	holdWork, retargetedToPullRequest := workerHoldInputForTarget(work, loop, queueItem)
	return r.workerHoldSummaryForWork(ctx, project, holdWork, retargetedToPullRequest)
}

func workerTargetIsPullRequest(loop storage.LoopRecord, queueItem storage.QueueItemRecord) bool {
	return loop.TargetType == "pull_request" || queueItem.TargetType == "pull_request"
}

func workerHoldInputForTarget(work workerInput, loop storage.LoopRecord, queueItem storage.QueueItemRecord) (workerInput, bool) {
	if !workerTargetIsPullRequest(loop, queueItem) {
		return work, false
	}
	work.Repo = firstNonEmpty(work.Repo, derefString(loop.Repo), derefString(queueItem.Repo))
	if work.PRNumber == 0 {
		work.PRNumber = firstNonZero(derefInt64(loop.PRNumber), derefInt64(queueItem.PRNumber))
	}
	if work.PRNumber == 0 {
		if loop.TargetType == "pull_request" {
			work.PRNumber = parsePullRequestNumberFromTargetID(derefString(loop.TargetID))
		}
		if work.PRNumber == 0 && queueItem.TargetType == "pull_request" {
			work.PRNumber = parsePullRequestNumberFromTargetID(queueItem.TargetID)
		}
	}
	return work, true
}

func (r *Runner) workerHoldSummaryForWork(ctx context.Context, project storage.ProjectRecord, work workerInput, retargetedToPullRequest ...bool) (bool, string, error) {
	if r.github == nil {
		return false, "", nil
	}
	if !work.AutoDiscovered {
		return false, "", nil
	}
	if work.PRNumber > 0 {
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: work.Repo, PRNumber: work.PRNumber, CWD: project.RepoPath})
		if err != nil {
			return false, "", err
		}
		if domain.IsAutoLaneHeld(domain.LoopTypeWorker, detail.Labels) {
			return true, fmt.Sprintf("Worker stopped because %s#%d is currently held", work.Repo, work.PRNumber), nil
		}
		if len(retargetedToPullRequest) > 0 && retargetedToPullRequest[0] {
			return false, "", nil
		}
	}
	if len(retargetedToPullRequest) > 0 && retargetedToPullRequest[0] {
		return false, "", nil
	}
	if work.IssueNumber > 0 {
		detail, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: issueLookupRepo(work), IssueNumber: work.IssueNumber, CWD: project.RepoPath})
		if err != nil {
			return false, "", err
		}
		if domain.IsAutoLaneHeld(domain.LoopTypeWorker, detail.Labels) {
			return true, fmt.Sprintf("Worker stopped because %s is currently held", formatIssueReference(issueLookupRepo(work), work.IssueNumber)), nil
		}
	}
	return false, "", nil
}

func workerWorkForPullRequest(work workerInput, pr PullRequestSummary) workerInput {
	work.PRNumber = pr.Number
	work.Branch = firstNonEmpty(pr.HeadRefName, work.Branch)
	work.BaseBranch = firstNonEmpty(pr.BaseRefName, work.BaseBranch)
	return work
}

func (r *Runner) finishHeldWorkerQueueItem(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, run *storage.RunRecord, queueItem storage.QueueItemRecord, checkpoint workerCheckpoint, summary string) (runpipe.ProcessResult, error) {
	checkpoint.SkipReason = summary
	checkpoint.ResumePolicy = loops.ResumePolicyAdvanceFromCheckpoint
	if graphQueueItem(queueItem) && queueItem.LoopID != nil {
		if err := r.workGraphs.ReleaseWorkerNode(ctx, *queueItem.LoopID); err != nil {
			return runpipe.ProcessResult{}, err
		}
	}
	if run != nil {
		finishedAt := r.nowISO()
		completed := completedRunRecord(*run, "success", summary, "", checkpoint, finishedAt)
		if err := storage.FinalizeWorkerSuccess(ctx, r.db, storage.WorkerSuccessFinalizationInput{Run: completed, QueueItemID: queueItem.ID, LoopID: loop.ID, LoopStatus: "queued", FinishedAt: finishedAt}); err != nil {
			return runpipe.ProcessResult{}, err
		}
	} else {
		if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil && !errors.Is(err, storage.ErrQueueItemNotActive) {
			return runpipe.ProcessResult{}, err
		}
		if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
			updated.Status = "queued"
			updated.LastRunAt = runpipe.StringPtr(r.nowISO())
			updated.NextRunAt = nil
		}); err != nil {
			return runpipe.ProcessResult{}, err
		}
	}
	result := runpipe.ProcessResult{LoopID: loop.ID, QueueItemID: queueItem.ID, Status: "skipped", Summary: summary}
	if run != nil {
		result.RunID = run.ID
	}
	return result, nil
}

func (r *Runner) resolveWorkerInput(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, queueItem storage.QueueItemRecord, checkpoint workerCheckpoint) (workerInput, error) {
	if checkpoint.Work != nil {
		return *checkpoint.Work, nil
	}
	payload := parseJSONObject(queueItem.PayloadJSON)
	metadata := parseJSONObject(loop.MetadataJSON)
	workerMeta, _ := metadata["worker"].(map[string]any)
	source := map[string]any{}
	for k, v := range workerMeta {
		source[k] = v
	}
	for k, v := range payload {
		source[k] = v
	}
	executionMode := stringFromAnyDefault(source["executionMode"])
	if executionMode == "" {
		if loop.TargetType == "pull_request" {
			executionMode = "push-existing"
		} else {
			executionMode = "create-pr"
		}
	}
	projectMetadata := parseJSONObject(project.MetadataJSON)
	repo := firstNonEmpty(stringFromAnyDefault(source["repo"]), derefString(loop.Repo), stringFromAnyDefault(projectMetadata["repo"]))
	baseBranch := firstNonEmpty(stringFromAnyDefault(source["baseBranch"]), stringFromAnyDefault(metadata["baseBranch"]), derefString(project.BaseBranch), "main")
	work := workerInput{Title: firstNonEmpty(stringFromAnyDefault(source["title"]), "Worker run"), Prompt: stringFromAnyDefault(source["prompt"]), SpecPath: stringFromAnyDefault(source["specPath"]), Repo: repo, IssueRepo: stringFromAnyDefault(source["issueRepo"]), BaseBranch: baseBranch, ExecutionMode: executionMode, IssueNumber: int64FromAny(source["issueNumber"]), IssueURL: stringFromAnyDefault(source["issueUrl"]), TriggerLogin: stringFromAnyDefault(source["triggerLogin"]), PRNumber: int64FromAny(source["prNumber"]), Branch: stringFromAnyDefault(source["branch"]), HeadSHA: stringFromAnyDefault(source["headSha"]), AutoDiscovered: boolFromAny(source["autoDiscovered"]), IssueClaimOverride: boolFromAny(source["issueClaimOverride"]), Reviewers: stringSliceFromAny(source["reviewers"])}
	if work.IssueNumber == 0 && loop.TargetType == "issue" {
		work.IssueNumber = parseIssueNumberFromTargetID(derefString(loop.TargetID))
	}
	if work.Repo == "" {
		return workerInput{}, &runpipe.LoopError{Message: "worker.repo is required", Kind: runpipe.FailureNonRetryable}
	}
	if work.BaseBranch == "" {
		return workerInput{}, &runpipe.LoopError{Message: "worker.baseBranch is required", Kind: runpipe.FailureNonRetryable}
	}
	if work.ExecutionMode == "create-pr" && work.Prompt == "" && work.SpecPath == "" && work.IssueNumber == 0 {
		return workerInput{}, &runpipe.LoopError{Message: "worker.prompt or worker.specPath is required", Kind: runpipe.FailureNonRetryable}
	}
	if work.ExecutionMode == "create-pr" && work.IssueNumber > 0 && r.github != nil {
		lookupRepo := issueLookupRepo(work)
		issue, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: lookupRepo, IssueNumber: work.IssueNumber, CWD: project.RepoPath})
		if err != nil {
			return workerInput{}, err
		}
		policy := r.discoveryPolicyForProject(project.ID)
		if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
			decision := networkpolicy.EvaluateWorker(policy.RoutedClaimPolicy, issue.Labels, issue.AssigneeUsers)
			if !decision.Allowed {
				return workerInput{}, &runpipe.LoopError{Message: fmt.Sprintf("Skipped routed worker claim for %s#%d: %s", lookupRepo, work.IssueNumber, decision.Reason), Kind: runpipe.FailureManualIntervention}
			}
			work.RoutedClaimMatchMode = string(decision.MatchMode)
		}
		if err := validateWorkerIssueTarget(lookupRepo, work.IssueNumber, issue); err != nil {
			return workerInput{}, err
		}
		if work.Prompt == "" && work.SpecPath == "" {
			work = hydrateWorkerInputFromIssue(work, issue)
		}
	}
	if loop.TargetType == "pull_request" {
		repo := firstNonEmpty(derefString(loop.Repo), work.Repo)
		prNumber := firstNonZero(derefInt64(loop.PRNumber), work.PRNumber)
		if repo == "" || prNumber == 0 {
			return workerInput{}, &runpipe.LoopError{Message: "pull_request worker loop requires repo and prNumber", Kind: runpipe.FailureNonRetryable}
		}
		detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: project.RepoPath})
		if err != nil {
			return workerInput{}, err
		}
		work.PRTitle = detail.Title
		work.Repo = repo
		work.PRNumber = prNumber
		work.BaseBranch = firstNonEmpty(detail.BaseRefName, work.BaseBranch)
		work.Branch = firstNonEmpty(detail.HeadRefName, work.Branch)
		work.HeadSHA = firstNonEmpty(detail.HeadSHA, work.HeadSHA)
		work.ExecutionMode = "push-existing"
		work.SpecPath = firstNonEmpty(work.SpecPath, specpr.ParseSpecPathFromPullRequestBody(detail.Body))
		work.Reviewers = append([]string(nil), detail.ReviewRequests...)
		// A spec path is the usual input for a PR-target worker, but an
		// operator-supplied prompt is also a complete instruction: the API
		// accepts prNumber+prompt, and buildWorkerPromptWithInstructions reads
		// the prompt directly when no spec path is present. Stop for manual
		// intervention only when neither is available.
		if work.SpecPath == "" && strings.TrimSpace(work.Prompt) == "" {
			return workerInput{}, &runpipe.LoopError{Message: fmt.Sprintf("No explicit spec path or prompt found for %s#%d", repo, prNumber), Kind: runpipe.FailureManualIntervention}
		}
	}
	return work, nil
}

func parseIssueNumberFromTargetID(targetID string) int64 {
	parts := strings.Split(strings.TrimSpace(targetID), ":")
	if len(parts) != 3 || parts[0] != "issue" {
		return 0
	}
	value, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func (r *Runner) createRunContext(ctx context.Context, loop storage.LoopRecord) (resumedRunContext, error) {
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return resumedRunContext{}, err
	}
	checkpoint := workerCheckpoint{}
	var lastCompletedStep WorkerStep
	var failedStep WorkerStep
	restartFromDiscover := false
	if latestRun != nil {
		checkpoint, err = parseCheckpoint(latestRun.CheckpointJSON)
		if err != nil {
			return resumedRunContext{}, err
		}
		lastCompletedStep = workflow.Parse(derefString(latestRun.LastCompletedStep))
		if derefString(latestRun.LastCompletedStep) != "" && lastCompletedStep == "" {
			return resumedRunContext{}, fmt.Errorf("unknown worker last completed step %q", derefString(latestRun.LastCompletedStep))
		}
		failedStep = workflow.Parse(derefString(latestRun.CurrentStep))
		restartFromDiscover = loops.ShouldRestartFromDiscover(latestRun.Status, checkpoint.ResumePolicy)
	}
	status := ""
	replayExecute := false
	if latestRun != nil {
		status = latestRun.Status
		replayExecute = shouldReplayExecuteOnResume(latestRun.Status, failedStep, checkpoint)
	}
	decision := workflow.DecideResume(status, lastCompletedStep, loops.IsManualHoldResumePolicy(checkpoint.ResumePolicy), restartFromDiscover, replayExecute)
	startStep := decision.StartStep
	resumed := decision.Resumed
	stickySnapshot := decision.StickyAgentSnapshot
	resumedCheckpoint := checkpoint
	// A successful worker normally has no follow-up turn. A survivor in the
	// human inbox is the exception: keep its work/session context but clear the
	// completed execution so the next queued claim actually starts the agent.
	if latestRun != nil && latestRun.Status == "success" && r.hitlEnabled && len(loops.ReadHumanInbox(loop.MetadataJSON)) > 0 {
		resumedCheckpoint.Execution = nil
		resumedCheckpoint.Validation = nil
	}
	switch decision.Mode {
	case workflow.ResumeModeRestart:
		resumedCheckpoint = workerCheckpoint{ResumePolicy: loops.ResumePolicyReplayStep}
	case workflow.ResumeModeReplayExecute:
		resumedCheckpoint = rewindCheckpointForExecuteRetry(checkpoint)
	}
	nowISO := r.nowISO()
	encoded := runpipe.MustMarshalJSON(workerCheckpoint{ResumePolicy: ternary(resumed, "advance_from_checkpoint", "replay_step"), Work: resumedCheckpoint.Work, ClaimedLockKey: resumedCheckpoint.ClaimedLockKey, Worktree: resumedCheckpoint.Worktree, Plan: resumedCheckpoint.Plan, Execution: resumedCheckpoint.Execution, Lifecycle: resumedCheckpoint.Lifecycle, ReproductionBaseline: resumedCheckpoint.ReproductionBaseline, Validation: resumedCheckpoint.Validation, PullRequest: resumedCheckpoint.PullRequest, SkipReason: resumedCheckpoint.SkipReason})
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: runpipe.StringPtr(string(startStep)), LastCompletedStep: nil, CheckpointJSON: &encoded, StartedAt: nowISO, LastHeartbeatAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	// replaysAgentStep must reflect whether an agent process will actually
	// run under the gate, not just whether the workflow reaches execute. A
	// manual-hold resume (failed + manual_intervention policy) restarts at
	// prepare-work while retaining a completed execution checkpoint, so
	// runExecuteStep is a no-op — no Codex agent launches even though
	// Reaches(prepare-work, execute) is true. Refreshing the snapshot in
	// that case would only rewrite disclosure identity, misattributing the
	// predecessor's changes. Gate the refresh on the retained execution
	// state of the checkpoint the new run will actually carry.
	replaysAgentStep := workflow.Reaches(startStep, stepExecute) && !executeStepAlreadyCompleted(resumedCheckpoint)
	snapshotJSON, err := r.agentSnapshotJSONForNewRun(latestRun, stickySnapshot, len(r.validationCommandsForProject(loop.ProjectID)) > 0, replaysAgentStep)
	if err != nil {
		return resumedRunContext{}, err
	}
	if snapshotJSON == nil && strings.TrimSpace(r.agentRuntime) != "" {
		return resumedRunContext{}, fmt.Errorf("agent snapshot required for vendor %q but was not produced", r.agentRuntime)
	}
	run.AgentSnapshotJSON = snapshotJSON
	if decision.LastCompletedStep != "" {
		value := string(decision.LastCompletedStep)
		run.LastCompletedStep = &value
	}
	if err := r.repos.Runs.Upsert(ctx, run); err != nil {
		return resumedRunContext{}, err
	}
	parsedCheckpoint, err := parseCheckpoint(run.CheckpointJSON)
	if err != nil {
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: parsedCheckpoint, Resumed: resumed}, nil
}

func (r *Runner) persistStepStarted(ctx context.Context, run storage.RunRecord, step WorkerStep, checkpoint workerCheckpoint) (storage.RunRecord, error) {
	return runpipe.PersistStepStarted(ctx, r.repos, r.nowISO(), run, string(step), checkpoint)
}

func (r *Runner) persistStepCompleted(ctx context.Context, run storage.RunRecord, step WorkerStep, checkpoint workerCheckpoint) (storage.RunRecord, error) {
	next := workflow.Next(step)
	return runpipe.PersistStepCompleted(ctx, r.repos, r.nowISO(), run, string(step), string(next), checkpoint)
}

func (r *Runner) persistCheckpoint(ctx context.Context, runID string, checkpoint workerCheckpoint) error {
	if r.repos == nil || r.repos.Runs == nil {
		return fmt.Errorf("runs repository is not configured")
	}
	run, err := r.repos.Runs.GetByID(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("run not found: %s", runID)
	}
	nowISO := r.nowISO()
	encoded := runpipe.MustMarshalJSON(checkpoint)
	run.CheckpointJSON = &encoded
	run.LastHeartbeatAt = &nowISO
	run.UpdatedAt = nowISO
	return r.repos.Runs.Upsert(ctx, *run)
}

func (r *Runner) completeRun(ctx context.Context, run storage.RunRecord, status, summary, errorMessage string, checkpoint workerCheckpoint) (storage.RunRecord, error) {
	return runpipe.CompleteRun(ctx, r.repos, r.nowISO(), run, status, summary, errorMessage, checkpoint)
}

func completedRunRecord(run storage.RunRecord, status, summary, errorMessage string, checkpoint workerCheckpoint, endedAt string) storage.RunRecord {
	updated := run
	updated.Status = status
	if summary != "" {
		updated.Summary = runpipe.StringPtr(summary)
	}
	if errorMessage != "" {
		updated.ErrorMessage = runpipe.StringPtr(errorMessage)
	}
	encoded := runpipe.MustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.EndedAt = &endedAt
	updated.LastHeartbeatAt = &endedAt
	updated.UpdatedAt = endedAt
	return updated
}

// IsSuccessfulClaimFinalizationCandidate reports durable evidence that worker
// execution is already complete and only terminal storage finalization remains.
func IsSuccessfulClaimFinalizationCandidate(run storage.RunRecord) bool {
	if run.Status == "success" {
		return derefString(run.LastCompletedStep) == string(stepOpenPR)
	}
	return (run.Status == "running" || run.Status == "interrupted") && derefString(run.LastCompletedStep) == string(stepOpenPR)
}

func (r *Runner) finalizeSuccessfulClaim(ctx context.Context, run storage.RunRecord, queueItem storage.QueueItemRecord, loop storage.LoopRecord, checkpoint workerCheckpoint, summary string) (storage.RunRecord, error) {
	unlock := loops.LockLoopRequeue(loop.ID)
	defer unlock()
	requeueForHumanInbox := false
	if r.hitlEnabled && r.repos != nil && r.repos.Loops != nil {
		fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return storage.RunRecord{}, err
		}
		requeueForHumanInbox = fresh != nil && fresh.Status == "running" && len(loops.ReadHumanInbox(fresh.MetadataJSON)) > 0
	}
	finishedAt := r.nowISO()
	completed := completedRunRecord(run, "success", summary, "", checkpoint, finishedAt)
	unlocked := false
	finalizeInput := storage.WorkerSuccessFinalizationInput{Run: completed, QueueItemID: queueItem.ID, LoopID: loop.ID, LoopStatus: "completed", FinishedAt: finishedAt, RequeueForHumanInbox: requeueForHumanInbox}
	if checkpoint.SkipReason == "" {
		finalizeInput.AfterFinalize = r.workGraphFinalizationHook(loop.ID, checkpoint, &unlocked)
	} else if graphQueueItem(queueItem) {
		if err := r.workGraphs.SettleSkippedWorkerNode(ctx, loop.ID, checkpoint.SkipReason); err != nil {
			return storage.RunRecord{}, err
		}
	}
	if err := storage.FinalizeWorkerSuccess(ctx, r.db, finalizeInput); err != nil {
		return storage.RunRecord{}, errors.Join(ErrSuccessfulClaimFinalization, err)
	}
	if unlocked {
		r.workGraphs.Wake()
	}
	return completed, nil
}

func (r *Runner) workGraphFinalizationHook(loopID string, checkpoint workerCheckpoint, unlocked *bool) func(context.Context, *storage.Repositories) error {
	return func(ctx context.Context, repos *storage.Repositories) error {
		queued, err := r.workGraphs.CompleteWorkerNodeInTransaction(ctx, repos, loopID, graphWorkerCompletedBranch(checkpoint))
		if err == nil && unlocked != nil && len(queued) > 0 {
			*unlocked = true
		}
		return err
	}
}

func graphWorkerCompletedBranch(checkpoint workerCheckpoint) string {
	if checkpoint.Lifecycle != nil {
		if branch := firstNonEmpty(checkpoint.Lifecycle.ActiveBranch, checkpoint.Lifecycle.AgentBranch, checkpoint.Lifecycle.Branch); branch != "" {
			return branch
		}
	}
	if checkpoint.Work != nil && strings.TrimSpace(checkpoint.Work.Branch) != "" {
		return checkpoint.Work.Branch
	}
	if checkpoint.Worktree != nil && strings.TrimSpace(checkpoint.Worktree.Branch) != "" {
		return checkpoint.Worktree.Branch
	}
	return ""
}

func (r *Runner) finalizePriorSuccessfulClaim(ctx context.Context, loop storage.LoopRecord, queueItem storage.QueueItemRecord) (runpipe.ProcessResult, bool, error) {
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || latestRun == nil || !IsSuccessfulClaimFinalizationCandidate(*latestRun) || !CanFinalizeSuccessfulClaim(queueItem, *latestRun, loop.Status) {
		return runpipe.ProcessResult{}, false, err
	}
	checkpoint, err := parseCheckpoint(latestRun.CheckpointJSON)
	if err != nil {
		return runpipe.ProcessResult{}, false, err
	}
	summary := derefString(latestRun.Summary)
	if summary == "" {
		summary = r.buildSuccessSummary(loop, checkpoint)
	}
	completed, err := r.finalizeSuccessfulClaim(ctx, *latestRun, queueItem, loop, checkpoint, summary)
	if err != nil {
		return runpipe.ProcessResult{}, true, err
	}
	status := statusForCheckpoint(checkpoint)
	return runpipe.ProcessResult{LoopID: loop.ID, RunID: completed.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary, PullRequestNumber: pullRequestNumber(checkpoint.PullRequest)}, true, nil
}

func (r *Runner) finishDuplicateGraphQueueItem(ctx context.Context, loop storage.LoopRecord, queueItem storage.QueueItemRecord) (runpipe.ProcessResult, error) {
	finishedAt := r.nowISO()
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, finishedAt); err != nil && !errors.Is(err, storage.ErrQueueItemNotActive) {
		return runpipe.ProcessResult{}, err
	}
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		updated.Status = "completed"
		updated.LastRunAt = &finishedAt
		updated.NextRunAt = nil
	}); err != nil {
		return runpipe.ProcessResult{}, err
	}
	return runpipe.ProcessResult{LoopID: loop.ID, QueueItemID: queueItem.ID, Status: "skipped", Summary: "Work graph node already claimed or settled"}, nil
}

func graphQueueItem(queueItem storage.QueueItemRecord) bool {
	payload := parseJSONObject(queueItem.PayloadJSON)
	_, ok := payload["workGraphID"].(string)
	return ok
}

// CanFinalizeSuccessfulClaim prevents a later queue generation from being
// consumed as recovery for an older completed run.
func CanFinalizeSuccessfulClaim(queueItem storage.QueueItemRecord, run storage.RunRecord, loopStatus string) bool {
	queueCreatedAt, queueErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(queueItem.CreatedAt))
	runStartedAt, runErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(run.StartedAt))
	if queueErr != nil || runErr != nil || queueCreatedAt.After(runStartedAt) {
		return false
	}
	if run.Status != "success" {
		return true
	}
	if run.EndedAt == nil {
		return false
	}
	runEndedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*run.EndedAt))
	if err != nil || queueCreatedAt.After(runEndedAt) {
		return false
	}
	if queueItem.Status == "running" {
		return loopStatus == "running"
	}
	return queueItem.Status == "queued" && queueItem.LastError != nil && strings.TrimSpace(*queueItem.LastError) != ""
}

func (r *Runner) getLatestCheckpoint(ctx context.Context, run storage.RunRecord, fallback workerCheckpoint) workerCheckpoint {
	persisted, err := r.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || persisted == nil {
		return fallback
	}
	checkpoint, err := parseCheckpoint(persisted.CheckpointJSON)
	if err != nil {
		return fallback
	}
	if fallback.ResumePolicy != "" {
		checkpoint.ResumePolicy = fallback.ResumePolicy
	}
	if fallback.Work != nil {
		checkpoint.Work = fallback.Work
	}
	if fallback.Worktree != nil {
		checkpoint.Worktree = fallback.Worktree
	}
	if fallback.Plan != nil {
		checkpoint.Plan = fallback.Plan
	}
	if fallback.Execution != nil {
		checkpoint.Execution = fallback.Execution
	}
	if fallback.Lifecycle != nil {
		checkpoint.Lifecycle = fallback.Lifecycle
	}
	if fallback.Validation != nil {
		checkpoint.Validation = fallback.Validation
	}
	if fallback.ReproductionBaseline != nil {
		checkpoint.ReproductionBaseline = fallback.ReproductionBaseline
	}
	if fallback.PullRequest != nil {
		checkpoint.PullRequest = fallback.PullRequest
	}
	if fallback.SkipReason != "" {
		checkpoint.SkipReason = fallback.SkipReason
	}
	if fallback.ClaimedLockKey != "" {
		checkpoint.ClaimedLockKey = fallback.ClaimedLockKey
	}
	return checkpoint
}

func (r *Runner) runValidation(ctx context.Context, input ValidationInput) (ValidationResult, error) {
	if r.validationRunner != nil {
		return r.validationRunner(ctx, input)
	}

	vresult, err := validation.RunCommands(ctx, validation.Input{
		Commands:       input.Commands,
		CommandTimeout: r.agentTimeout,
	}, &validation.Options{
		CWD:     input.CWD,
		Tracker: r.containmentTracker,
	})
	if err != nil {
		return ValidationResult{}, err
	}

	return ValidationResult{
		Passed:          vresult.Passed,
		Summary:         vresult.Summary,
		Output:          vresult.Output,
		FailureCategory: vresult.FailureCategory,
	}, nil
}

type validationFailure struct {
	message      string
	kind         runpipe.QueueFailureKind
	resumePolicy string
}

func classifyValidationFailure(result ValidationResult) validationFailure {
	if result.FailureCategory != "" {
		policy := validation.PolicyFor(result.FailureCategory)
		return validationFailure{
			message:      firstNonEmpty(strings.TrimSpace(result.Summary), "Validation failed"),
			kind:         runpipe.QueueFailureKind(policy.FailureKind),
			resumePolicy: policy.ResumePolicy,
		}
	}

	message := firstNonEmpty(strings.TrimSpace(result.Summary), "Validation failed")
	summary := strings.ToLower(strings.TrimSpace(result.Summary))
	if containsAnyValidationHint(summary, []string{"dirty worktree", "uncommitted changes", "merge conflict", "conflict markers", "ambiguous repo", "unsafe repo"}) {
		return validationFailure{message: message, kind: runpipe.FailureManualIntervention, resumePolicy: loops.ResumePolicyManualIntervention}
	}
	if containsAnyValidationHint(summary, []string{"stale checkpoint", "stale repo", "stale repo context", "stale worktree", "head changed", "base changed", "branch changed", "out of date", "no longer matches"}) {
		return validationFailure{message: message, kind: runpipe.FailureRetryableAfterResume, resumePolicy: loops.ResumePolicyRestartFromDiscover}
	}
	if strings.HasPrefix(result.Summary, "Validation timed out:") {
		return validationFailure{message: message, kind: runpipe.FailureRetryableTransient, resumePolicy: loops.ResumePolicyReplayStep}
	}
	if containsAnyValidationHint(summary, []string{"command not found", "executable file not found", "connection reset", "connection refused", "temporary failure", "service unavailable", "network is unreachable", "transport error"}) {
		return validationFailure{message: message, kind: runpipe.FailureRetryableTransient, resumePolicy: loops.ResumePolicyReplayStep}
	}
	return validationFailure{message: message, kind: runpipe.FailureManualIntervention, resumePolicy: loops.ResumePolicyManualIntervention}
}

func containsAnyValidationHint(message string, hints []string) bool {
	for _, hint := range hints {
		if strings.Contains(message, hint) {
			return true
		}
	}
	return false
}

func (r *Runner) findOpenPullRequestForBranch(ctx context.Context, repo string, branches []string, baseBranch, cwd string) (*PullRequestSummary, error) {
	if r.github == nil {
		return nil, nil
	}
	pullRequests, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: workerPRDedupeLookupLimit})
	if err != nil {
		return nil, err
	}
	for _, pr := range pullRequests {
		if strings.EqualFold(pr.State, "open") && containsString(branches, pr.HeadRefName) && pr.BaseRefName == baseBranch {
			candidate := pr
			return &candidate, nil
		}
	}
	return nil, nil
}

func (r *Runner) activeWorkerLoopClaimingPullRequest(ctx context.Context, currentLoopID, repo string, prNumber int64) (string, error) {
	if r.repos == nil || prNumber <= 0 {
		return "", nil
	}
	loopsList, err := r.repos.Loops.List(ctx)
	if err != nil {
		return "", &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableAfterResume}
	}
	for _, loop := range loopsList {
		if loop.ID == currentLoopID || loop.Type != "worker" {
			continue
		}
		if loop.Status != "queued" && loop.Status != "running" && loop.Status != "paused" {
			continue
		}
		if !strings.EqualFold(derefString(loop.Repo), repo) {
			continue
		}
		candidatePR := derefInt64(loop.PRNumber)
		if candidatePR == 0 && loop.TargetType == "pull_request" {
			candidatePR = parsePullRequestNumberFromTargetID(derefString(loop.TargetID))
		}
		if candidatePR == prNumber {
			return fmt.Sprintf("pull request is already claimed by active worker loop %s", loop.ID), nil
		}
	}
	return "", nil
}

func parsePullRequestNumberFromTargetID(targetID string) int64 {
	parts := strings.Split(strings.TrimSpace(targetID), ":")
	if len(parts) != 3 || parts[0] != "pr" {
		return 0
	}
	prNumber, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0
	}
	return prNumber
}

func (r *Runner) assignReviewersIfNeeded(ctx context.Context, work workerInput, prNumber int64, cwd string) error {
	if r.github == nil || prNumber == 0 || len(work.Reviewers) == 0 {
		return nil
	}
	return r.github.AddPullRequestReviewers(ctx, PullRequestReviewersInput{Repo: work.Repo, PRNumber: prNumber, Reviewers: append([]string(nil), work.Reviewers...), CWD: cwd})
}

func (r *Runner) persistPullRequestReference(ctx context.Context, loop storage.LoopRecord, queueItem storage.QueueItemRecord, repo string, pr checkpointPullPR) error {
	if r.db == nil {
		return fmt.Errorf("worker runner database is not configured")
	}
	targetID := fmt.Sprintf("pr:%s:%d", repo, pr.Number)
	updatedQueue := queueItem
	updatedQueue.TargetType = "pull_request"
	updatedQueue.TargetID = targetID
	updatedQueue.Repo = runpipe.StringPtr(repo)
	updatedQueue.PRNumber = int64Ptr(pr.Number)
	if !strings.HasPrefix(derefString(queueItem.LockKey), "issue:") {
		updatedQueue.LockKey = runpipe.StringPtr(storage.PullRequestLockKey(derefString(queueItem.ProjectID), repo, pr.Number))
	}
	projectID := derefString(queueItem.ProjectID)
	if projectID != "" {
		updatedQueue.DedupeKey = fmt.Sprintf("worker:%s:%s:%d", projectID, repo, pr.Number)
	}
	nowISO := r.nowISO()
	updatedQueue.UpdatedAt = nowISO

	return storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
		repos := storage.NewRepositories(tx)
		current, err := repos.Loops.GetByID(ctx, loop.ID)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("loop not found: %s", loop.ID)
		}
		// Merge the PR fields into the FRESH metadata read inside this transaction,
		// not the stale passed-in loop copy. Otherwise concurrent metadata writes made
		// earlier in this same run — e.g. the HITL answer marked "consumed" by
		// markHumanAnswerConsumed — are silently clobbered back to their pre-run value,
		// which would re-inject an already-consumed human answer on any later re-run.
		metadataJSON, err := mergeLoopMetadataJSON(current.MetadataJSON, map[string]any{"prUrl": pr.URL, "prNumber": pr.Number, "repo": repo})
		if err != nil {
			return err
		}
		updatedLoop := *current
		updatedLoop.Repo = runpipe.StringPtr(repo)
		updatedLoop.TargetType = "pull_request"
		updatedLoop.TargetID = runpipe.StringPtr(targetID)
		updatedLoop.PRNumber = int64Ptr(pr.Number)
		updatedLoop.MetadataJSON = runpipe.StringPtr(metadataJSON)
		updatedLoop.UpdatedAt = nowISO
		if err := repos.Loops.AssertIssueClaimAdmission(ctx, updatedLoop, storage.LoopForcesIssueClaimAdmission(updatedLoop)); err != nil {
			return err
		}
		if err := repos.Loops.Upsert(ctx, updatedLoop); err != nil {
			return err
		}
		return repos.Queue.Upsert(ctx, updatedQueue)
	})
}

func (r *Runner) ensureLoopForDiscoveredIssue(ctx context.Context, project storage.ProjectRecord, repo string, issue IssueSummary, currentFingerprint string) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	targetID := buildIssueTargetID(repo, issue.Number)
	baseBranch := firstNonEmpty(derefString(project.BaseBranch), "main")
	work := workerInput{Title: firstNonEmpty(issue.Title, buildDefaultIssueWorkerTitle(repo, issue.Number)), Repo: repo, BaseBranch: baseBranch, ExecutionMode: "create-pr", IssueNumber: issue.Number, IssueURL: issue.URL, TriggerLogin: issue.Author, AutoDiscovered: true, PersonalAssigneeAutoAssigned: issue.PersonalAssigneeAutoAssigned}
	workerMeta := map[string]any{"worker": mergeWorkerMetadata(parseJSONObject(nil), work)}
	if issue.PersonalAssigneeAutoAssigned {
		workerMeta["personalProjectAutoAssigned"] = true
	}
	existingLoops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	for _, existing := range existingLoops {
		if workerLoopTracksIssue(existing, project.ID, repo, issue.Number) {
			pausedOrCompleted := existing.Status == "paused" || existing.Status == "human_takeover" || existing.Status == "completed" || existing.Status == "awaiting_human"
			prLinked := existing.TargetType == "pull_request" || derefInt64(existing.PRNumber) > 0
			if prLinked {
				return loopUpsertResult{record: existing, skipEnqueue: true}, nil
			}
			if existing.Status == "human_takeover" {
				return loopUpsertResult{}, fmt.Errorf("cannot create worker loop: issue #%d has an active human_takeover loop (%s)", issue.Number, existing.ID)
			}
			updated := existing
			updated.Repo = &repo
			suppressFailedRevival := loops.ShouldSuppressFailedRediscovery(existing.Status, loops.LastFailedDiscoveryFingerprint(existing.MetadataJSON), currentFingerprint)
			if !pausedOrCompleted && !suppressFailedRevival && updated.Status != "running" {
				updated.Status = "queued"
				updated.NextRunAt = &nowISO
			}
			updates := map[string]any{"worker": mergeWorkerMetadata(parseJSONObject(existing.MetadataJSON), work)}
			if issue.PersonalAssigneeAutoAssigned {
				updates["personalProjectAutoAssigned"] = true
			}
			metadataJSON, err := mergeLoopMetadataJSON(existing.MetadataJSON, updates)
			if err != nil {
				// Do not revive/requeue a loop whose durable state could not
				// be recorded.
				return loopUpsertResult{}, fmt.Errorf("merge worker discovery metadata: %w", err)
			}
			updated.MetadataJSON = &metadataJSON
			updated.UpdatedAt = nowISO
			published, err := r.publishDiscoveredIssueLoop(ctx, updated)
			if err != nil {
				return loopUpsertResult{}, err
			}
			return loopUpsertResult{record: published}, nil
		}
	}
	for _, existing := range existingLoops {
		if existing.ProjectID == project.ID && derefString(existing.Repo) == repo && existing.TargetType == "issue" && derefString(existing.TargetID) == targetID && existing.Status == "human_takeover" {
			return loopUpsertResult{}, fmt.Errorf("cannot create worker loop: issue #%d has an active human_takeover loop (%s)", issue.Number, existing.ID)
		}
	}
	metadataJSON := runpipe.MustMarshalJSON(workerMeta)
	loop := storage.LoopRecord{ID: eventlog.NewEventID("loop"), ProjectID: project.ID, Type: "worker", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "queued", MetadataJSON: &metadataJSON, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	published, err := r.publishDiscoveredIssueLoop(ctx, loop)
	if err != nil {
		return loopUpsertResult{}, err
	}
	return loopUpsertResult{record: published, created: true}, nil
}

// publishDiscoveredIssueLoop shares the source-issue publication authority
// with manual dispatch. Allocating a new loop sequence, asserting admission,
// and exposing an active worker happen in one transaction; rediscovery follows
// the same path when it changes an inactive worker back to queued.
func (r *Runner) publishDiscoveredIssueLoop(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	if r.db == nil {
		return storage.LoopRecord{}, fmt.Errorf("worker runner database is not configured")
	}
	return storage.WithTransactionValue(ctx, r.db, nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)
		if loop.Seq == 0 {
			seq, err := repos.Loops.AllocateSeq(ctx)
			if err != nil {
				return storage.LoopRecord{}, err
			}
			loop.Seq = seq
		}
		if err := repos.Loops.AssertIssueClaimAdmission(ctx, loop, false); err != nil {
			return storage.LoopRecord{}, err
		}
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			return storage.LoopRecord{}, err
		}
		return loop, nil
	})
}

func (r *Runner) enqueueDiscoveredIssue(ctx context.Context, project storage.ProjectRecord, loop storage.LoopRecord, repo string, issue IssueSummary, fingerprint string) (storage.QueueItemRecord, error) {
	dedupeKey := buildWorkerIssueDedupeKey(project.ID, repo, issue.Number)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	nowISO := r.nowISO()
	baseBranch := firstNonEmpty(derefString(project.BaseBranch), "main")
	payloadMap := map[string]any{"title": firstNonEmpty(issue.Title, buildDefaultIssueWorkerTitle(repo, issue.Number)), "repo": repo, "baseBranch": baseBranch, "executionMode": "create-pr", "issueNumber": issue.Number, "issueUrl": issue.URL, "triggerLogin": issue.Author, "autoDiscovered": true, "discoveryFingerprint": fingerprint}
	if issue.PersonalAssigneeAutoAssigned {
		payloadMap["personalProjectAutoAssigned"] = true
	}
	payload := runpipe.MustMarshalJSON(payloadMap)
	targetID := buildIssueTargetID(repo, issue.Number)
	lockKey := storage.IssueLockKey(project.ID, repo, issue.Number)
	projectID := project.ID
	loopID := loop.ID
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "issue", TargetID: targetID, Repo: &repo, DedupeKey: dedupeKey, Priority: storage.QueuePriorityWorker, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
	persisted, created, err := r.repos.Queue.CreateOrGetActiveByDedupe(ctx, queueItem)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if created {
		r.wakeSchedulerAfterEnqueue()
	}
	return persisted, nil
}

func (r *Runner) wakeSchedulerAfterEnqueue() {
	if r.onQueueItemEnqueued != nil {
		r.onQueueItemEnqueued()
	}
}

func (r *Runner) failQueueItem(ctx context.Context, queueItem storage.QueueItemRecord, kind runpipe.QueueFailureKind, message string) (*storage.QueueItemRecord, error) {
	// Recovery can be entered after ProcessClaimedItem has already committed
	// the queue transition but failed while reconciling the loop. Treat a
	// terminal row as the durable result instead of trying to resurrect it (or
	// surfacing ErrQueueItemNotActive from a stricter repository implementation).
	// The queue row, rather than the stale claimed value passed by the caller,
	// is the authority for whether this transition still needs to run.
	current, err := r.repos.Queue.GetByID(ctx, queueItem.ID)
	if err != nil {
		return nil, err
	}
	// Cancellation is deliberately excluded: runner finalization owns the
	// right to requeue a claim cancelled concurrently by a pause (see
	// Queue.MarkRetry's contract). The already-finalized statuses below must
	// remain terminal, while a cancelled row can still be reconciled according
	// to the failure kind being finalized.
	if current != nil && (current.Status == "manual_intervention" || current.Status == "completed" || current.Status == "failed") {
		return current, nil
	}
	nextAttempts := queueItem.Attempts + 1
	nowISO := r.nowISO()
	isGraph := graphQueueItem(queueItem) && queueItem.LoopID != nil
	if !runpipe.ShouldRetryQueueFailure(kind, nextAttempts, queueItem.MaxAttempts) {
		if isGraph {
			replanned := false
			err := storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
				repos := storage.NewRepositories(tx)
				if err := repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: runpipe.OptionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
					return err
				}
				var failErr error
				replanned, failErr = r.workGraphs.FailWorkerNodeInTransaction(ctx, repos, *queueItem.LoopID, message)
				return failErr
			})
			if err != nil {
				if recovered, getErr := r.repos.Queue.GetByID(ctx, queueItem.ID); getErr == nil && recovered != nil && (recovered.Status == "manual_intervention" || recovered.Status == "completed" || recovered.Status == "failed") {
					return recovered, nil
				}
				return nil, err
			}
			if replanned {
				r.workGraphs.Wake()
			}
			return r.repos.Queue.GetByID(ctx, queueItem.ID)
		}
		if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: runpipe.OptionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			if recovered, getErr := r.repos.Queue.GetByID(ctx, queueItem.ID); getErr == nil && recovered != nil && (recovered.Status == "manual_intervention" || recovered.Status == "completed" || recovered.Status == "failed") {
				return recovered, nil
			}
			return nil, err
		}
		return r.repos.Queue.GetByID(ctx, queueItem.ID)
	}
	retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(runpipe.BackoffDelayLinear(r.retryBaseDelay, runpipe.CappedRetryDelayAttempt(nextAttempts, queueItem.MaxAttempts))))
	if isGraph {
		err := storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
			repos := storage.NewRepositories(tx)
			if err := repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: runpipe.OptionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
				return err
			}
			return r.workGraphs.ReleaseWorkerNodeInTransaction(ctx, repos, *queueItem.LoopID)
		})
		if err != nil {
			if recovered, getErr := r.repos.Queue.GetByID(ctx, queueItem.ID); getErr == nil && recovered != nil && (recovered.Status == "manual_intervention" || recovered.Status == "completed" || recovered.Status == "failed") {
				return recovered, nil
			}
			return nil, err
		}
		return r.repos.Queue.GetByID(ctx, queueItem.ID)
	}
	if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: runpipe.OptionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
		if recovered, getErr := r.repos.Queue.GetByID(ctx, queueItem.ID); getErr == nil && recovered != nil && (recovered.Status == "manual_intervention" || recovered.Status == "completed" || recovered.Status == "failed") {
			return recovered, nil
		}
		return nil, err
	}
	return r.repos.Queue.GetByID(ctx, queueItem.ID)
}

func (r *Runner) updateLoop(ctx context.Context, loop storage.LoopRecord, mutate func(*storage.LoopRecord)) (storage.LoopRecord, error) {
	return runpipe.UpdateLoop(ctx, r.repos, r.now, loop, runpipe.UpdateLoopOptions{}, mutate)
}

func (r *Runner) buildSuccessSummary(loop storage.LoopRecord, checkpoint workerCheckpoint) string {
	if checkpoint.SkipReason != "" {
		return checkpoint.SkipReason
	}
	if checkpoint.PullRequest != nil && checkpoint.PullRequest.URL != "" {
		return fmt.Sprintf("Opened pull request for worker %s: %s", loop.ID, checkpoint.PullRequest.URL)
	}
	return fmt.Sprintf("Completed worker %s", loop.ID)
}

const (
	issueClaimStatusRunning  = "running"
	issueClaimStatusPRLinked = "pr_linked"
	issueClaimStatusSuccess  = "success"
	issueClaimStatusFailed   = "failed"
	issueClaimStatusPaused   = "paused"
)

func (r *Runner) syncIssueClaim(ctx context.Context, input stepInput, checkpoint *workerCheckpoint, status, summary string) {
	if checkpoint == nil || checkpoint.Work == nil || checkpoint.Work.IssueNumber <= 0 || r.github == nil {
		return
	}
	repo := issueLookupRepo(*checkpoint.Work)
	if repo == "" {
		return
	}
	body := buildIssueClaimCommentBody(input.Loop.ID, input.Run.ID, *checkpoint.Work, status, checkpoint.PullRequest, summary)
	claim := checkpoint.IssueClaim
	if claim == nil {
		claim = r.findPreviousIssueClaim(ctx, input.Loop.ID, input.Run.ID, repo, checkpoint.Work.IssueNumber)
		if claim != nil {
			checkpoint.IssueClaim = claim
		}
	}
	if claim == nil {
		claim = &checkpointIssueClaim{Repo: repo, IssueNumber: checkpoint.Work.IssueNumber}
		checkpoint.IssueClaim = claim
	}
	claim.Repo = repo
	claim.IssueNumber = checkpoint.Work.IssueNumber
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	if claim.CommentID == 0 {
		created, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: checkpoint.Work.IssueNumber, Body: body, CWD: input.Project.RepoPath, DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel})
		if err != nil {
			r.logWarn("worker issue claim comment create failed", map[string]any{"loopId": input.Loop.ID, "repo": repo, "issueNumber": checkpoint.Work.IssueNumber, "error": err.Error()})
			return
		}
		claim.CommentID = created.ID
		claim.CommentURL = created.URL
		claim.Status = status
		if err := r.persistCheckpoint(ctx, input.Run.ID, *checkpoint); err != nil {
			r.logWarn("worker issue claim checkpoint persist failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
		}
		return
	}
	if err := r.github.UpdateIssueComment(ctx, UpdateIssueCommentInput{Repo: repo, CommentID: claim.CommentID, Body: body, CWD: input.Project.RepoPath, DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel}); err != nil {
		r.logWarn("worker issue claim comment update failed", map[string]any{"loopId": input.Loop.ID, "repo": repo, "issueNumber": checkpoint.Work.IssueNumber, "commentId": claim.CommentID, "error": err.Error()})
		return
	}
	claim.Status = status
	if err := r.persistCheckpoint(ctx, input.Run.ID, *checkpoint); err != nil {
		r.logWarn("worker issue claim checkpoint persist failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
	}
}

func (r *Runner) findPreviousIssueClaim(ctx context.Context, loopID, currentRunID, repo string, issueNumber int64) *checkpointIssueClaim {
	if r.repos == nil || r.repos.Runs == nil {
		return nil
	}
	runs, err := r.repos.Runs.ListByLoop(ctx, loopID)
	if err != nil {
		return nil
	}
	for i := 0; i < len(runs); i++ {
		if runs[i].ID == currentRunID {
			continue
		}
		checkpoint, err := parseCheckpoint(runs[i].CheckpointJSON)
		if err != nil || checkpoint.IssueClaim == nil || checkpoint.IssueClaim.CommentID == 0 {
			continue
		}
		if checkpoint.IssueClaim.IssueNumber != issueNumber || !strings.EqualFold(checkpoint.IssueClaim.Repo, repo) {
			continue
		}
		claim := *checkpoint.IssueClaim
		return &claim
	}
	return nil
}

func buildIssueClaimCommentBody(loopID, runID string, work workerInput, status string, pr *checkpointPullPR, summary string) string {
	lines := []string{
		fmt.Sprintf("<!-- looper:issue-claim loop:%s run:%s issue:%s -->", loopID, runID, formatIssueReference(issueLookupRepo(work), work.IssueNumber)),
	}
	switch status {
	case issueClaimStatusPRLinked:
		lines = append(lines, "Looper is working on this issue.")
		if pr != nil && pr.URL != "" {
			lines = append(lines, "", "Linked pull request: "+pr.URL)
		}
	case issueClaimStatusSuccess:
		lines = append(lines, "Looper finished work on this issue.")
		if pr != nil && pr.URL != "" {
			lines = append(lines, "", "Pull request: "+pr.URL)
		}
	case issueClaimStatusFailed:
		lines = append(lines, "Looper stopped work on this issue with a failure.")
		if summary = sanitizePublicIssueClaimSummary(summary); summary != "" {
			lines = append(lines, "", "Latest status: "+summary)
		}
	case issueClaimStatusPaused:
		lines = append(lines, "Looper paused work on this issue.")
		if summary = sanitizePublicPausedIssueClaimSummary(summary); summary != "" {
			lines = append(lines, "", "Latest status: "+summary)
		}
	default:
		lines = append(lines, "Looper has claimed this issue and started work.")
	}
	return strings.Join(lines, "\n")
}

func (r *Runner) logWarn(message string, payload map[string]any) {
	if r.logger != nil {
		r.logger.Warn(message, payload)
	}
}

func (r *Runner) notifyRunCompleted(ctx context.Context, input RunCompletedInput) {
	if r.onRunCompleted != nil {
		if err := r.onRunCompleted(ctx, input); err != nil && r.logger != nil {
			r.logger.Warn("worker completion notification failed", map[string]any{"loopId": input.LoopID, "runId": input.RunID, "error": err.Error()})
		}
	}
}

func (r *Runner) notifyRecoveredRunCompleted(ctx context.Context, queueItem storage.QueueItemRecord, failure *runpipe.LoopError) {
	if queueItem.LoopID == nil {
		return
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil || loop == nil {
		return
	}
	runID := ""
	checkpoint := workerCheckpoint{}
	if latestRun, runErr := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID); runErr == nil && latestRun != nil {
		if runMatchesQueueAttempt(queueItem, *latestRun) {
			runID = latestRun.ID
			if parsed, parseErr := parseCheckpoint(latestRun.CheckpointJSON); parseErr == nil {
				checkpoint = parsed
			}
		}
	}
	r.notifyRunCompleted(ctx, RunCompletedInput{
		ProjectID:         loop.ProjectID,
		LoopID:            loop.ID,
		RunID:             runID,
		Subtitle:          runNotificationSubtitle(*loop, checkpoint),
		Status:            "failed",
		Summary:           failure.Message,
		FailureKind:       failure.Kind,
		PullRequestNumber: pullRequestNumber(checkpoint.PullRequest),
		PullRequestURL:    pullRequestURL(checkpoint.PullRequest),
	})
}

func runMatchesQueueAttempt(queueItem storage.QueueItemRecord, run storage.RunRecord) bool {
	if queueItem.ClaimedAt == nil || *queueItem.ClaimedAt == "" {
		return run.Status == "running"
	}
	return run.StartedAt >= *queueItem.ClaimedAt
}

func buildRunCompletedInput(project storage.ProjectRecord, loop storage.LoopRecord, run storage.RunRecord, checkpoint workerCheckpoint, status string, failureKind runpipe.QueueFailureKind, summary string) RunCompletedInput {
	return RunCompletedInput{
		ProjectID:         project.ID,
		LoopID:            loop.ID,
		RunID:             run.ID,
		Subtitle:          runNotificationSubtitle(loop, checkpoint),
		Status:            status,
		Summary:           summary,
		FailureKind:       failureKind,
		PullRequestNumber: pullRequestNumber(checkpoint.PullRequest),
		PullRequestURL:    pullRequestURL(checkpoint.PullRequest),
	}
}

func runNotificationSubtitle(loop storage.LoopRecord, checkpoint workerCheckpoint) string {
	if checkpoint.Work != nil && strings.TrimSpace(checkpoint.Work.Title) != "" {
		return checkpoint.Work.Title
	}
	if loop.TargetType != "" && loop.TargetID != nil && strings.TrimSpace(*loop.TargetID) != "" {
		return *loop.TargetID
	}
	return loop.ID
}

func shouldNotifyCompletedRun(kind runpipe.QueueFailureKind, failedQueue *storage.QueueItemRecord) bool {
	if kind == runpipe.FailureManualIntervention {
		return true
	}
	return failedQueue != nil && failedQueue.Status != "queued" && failedQueue.Status != "cancelled"
}

// shouldRetryQueueFailure applies the two-tier retry rule from #508.
//
// With an infinite bound (scheduler.retryMaxAttempts = -1, the default)
// nothing else ever stops a retry, so non_retryable is the only brake and is
// honoured strictly. With a positive bound the cap is already the brake, so
// the kind is deliberately ignored and even a non_retryable failure gets its
// attempts: the classifications feeding this are heuristics over agent output,
// and a wrong one should not park a loop permanently when the bound would have
// ended it anyway.
//
// The asymmetry looks like an oversight and is not. Reintroducing the kind
// check in the bounded branch breaks TestShouldRetryQueueFailureRespectsMaxAttempts
// here and in the fixer, reviewer and planner copies. The leading guard is
// inlined rather than shared with those copies, which name it
// isQueueRetryEligible; only manual_intervention is excluded, because it has
// left the automated lane and is waiting on a human.
func issueClaimStatusForFailure(checkpoint workerCheckpoint, failedQueue *storage.QueueItemRecord, kind runpipe.QueueFailureKind) string {
	if failedQueue != nil && failedQueue.Status == "queued" {
		if checkpoint.PullRequest != nil && strings.TrimSpace(checkpoint.PullRequest.URL) != "" {
			return issueClaimStatusPRLinked
		}
		return issueClaimStatusRunning
	}
	if kind == runpipe.FailureManualIntervention {
		return issueClaimStatusPaused
	}
	return issueClaimStatusFailed
}

func statusForCheckpoint(checkpoint workerCheckpoint) string {
	if checkpoint.SkipReason != "" {
		return "skipped"
	}
	return "success"
}

func pullRequestURL(pr *checkpointPullPR) string {
	if pr == nil {
		return ""
	}
	return strings.TrimSpace(pr.URL)
}

func (r *Runner) canAgentCreatePR(ctx context.Context, projectID string, work workerInput, cwd string) bool {
	return work.ExecutionMode == "create-pr" &&
		r.openPRStrategy != config.OpenPRStrategyManual &&
		r.allowAutoPush &&
		r.githubCLIAutoPROpeningAvailable(ctx, work.Repo, cwd) &&
		len(r.validationCommandsForProject(projectID)) == 0 &&
		r.validationRunner == nil
}

func (r *Runner) githubCLIAutoPROpeningAvailable(ctx context.Context, repo, cwd string) bool {
	if r.githubCLICheck != nil {
		return r.githubCLICheck(ctx, repo, cwd)
	}
	return r.githubCLIAvailable
}

func (r *Runner) classifyFailure(err error) *runpipe.LoopError {
	return r.classifyFailureWithBoundary(err, failureclass.BoundaryUnknown)
}

func (r *Runner) classifyFailureWithBoundary(err error, boundary failureclass.Boundary) *runpipe.LoopError {
	var typed *runpipe.LoopError
	if errors.As(err, &typed) {
		return typed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	if githubinfra.IsTransientError(err) {
		return &runpipe.LoopError{Message: err.Error(), Kind: runpipe.FailureRetryableTransient}
	}
	return &runpipe.LoopError{Message: err.Error(), Kind: workerFailureKind(failureclass.Classify(err, failureclass.Context{Runner: failureclass.RunnerWorker, Boundary: boundary}))}
}

func workerFailureBoundaryForStep(step WorkerStep) failureclass.Boundary {
	switch step {
	case stepPrepareWork, stepOpenPR:
		return failureclass.BoundaryGitHubAPI
	case stepPrepareWorktree:
		return failureclass.BoundaryGitRemote
	case stepPlan, stepExecute:
		return failureclass.BoundaryModelProvider
	case stepValidate:
		return failureclass.BoundaryAgentProcess
	default:
		return failureclass.BoundaryUnknown
	}
}

func workerFailureKind(kind failureclass.Kind) runpipe.QueueFailureKind {
	switch kind {
	case failureclass.RetryableTransient:
		return runpipe.FailureRetryableTransient
	case failureclass.RetryableAfterResume:
		return runpipe.FailureRetryableAfterResume
	case failureclass.ManualIntervention:
		return runpipe.FailureManualIntervention
	default:
		return runpipe.FailureNonRetryable
	}
}

func (r *Runner) nowISO() string { return eventlog.FormatJavaScriptISOString(r.now()) }

func validateWorkerResumeCheckpoint(startStep WorkerStep, checkpoint workerCheckpoint) error {
	switch startStep {
	case stepValidate, stepOpenPR:
		return validateCompletedExecutionCheckpoint(checkpoint.Execution)
	default:
		return nil
	}
}

func parseCheckpoint(value *string) (workerCheckpoint, error) {
	if value == nil || *value == "" {
		return workerCheckpoint{}, nil
	}
	var checkpoint workerCheckpoint
	if err := json.Unmarshal([]byte(*value), &checkpoint); err != nil {
		return workerCheckpoint{}, fmt.Errorf("parse worker checkpoint: %w", err)
	}
	return checkpoint, nil
}

func requireWork(checkpoint workerCheckpoint) (workerInput, error) {
	if checkpoint.Work == nil {
		return workerInput{}, &runpipe.LoopError{Message: "missing worker input checkpoint", Kind: runpipe.FailureRetryableTransient}
	}
	return *checkpoint.Work, nil
}

func shouldReplayExecuteOnResume(status string, failedStep WorkerStep, checkpoint workerCheckpoint) bool {
	if status != "failed" && status != "interrupted" {
		return false
	}
	switch failedStep {
	case stepValidate, stepOpenPR:
	default:
		return false
	}
	return validateCompletedExecutionCheckpoint(checkpoint.Execution) != nil
}

// executeStepAlreadyCompleted reports whether the retained execution checkpoint
// makes runExecuteStep skip launching an agent. runExecuteStep never starts an
// agent process when Execution.Status == "completed": at GitReconciled it
// returns immediately, and without GitReconciled it only re-reconciles git
// state. On such a resume the agent step does not replay, so the predecessor
// author snapshot must be preserved instead of refreshed — otherwise the
// predecessor's changes get re-stamped as the current vendor's. The argument
// is the checkpoint the new run will actually carry (after any rewind), not
// the raw predecessor checkpoint, so a replay-execute rewind (Execution=nil)
// correctly reports false.
func executeStepAlreadyCompleted(checkpoint workerCheckpoint) bool {
	return checkpoint.Execution != nil && checkpoint.Execution.Status == "completed"
}

func rewindCheckpointForExecuteRetry(checkpoint workerCheckpoint) workerCheckpoint {
	if checkpoint.Execution == nil || checkpoint.Execution.Status != "timeout" || checkpoint.Execution.ProgressBeforeTimeout == nil {
		checkpoint.Execution = nil
	}
	checkpoint.Validation = nil
	checkpoint.PullRequest = nil
	checkpoint.SkipReason = ""
	return checkpoint
}

func requireWorktree(checkpoint workerCheckpoint) (checkpointWorktree, error) {
	if checkpoint.Worktree == nil {
		return checkpointWorktree{}, &runpipe.LoopError{Message: "missing worker worktree checkpoint", Kind: runpipe.FailureRetryableTransient}
	}
	return *checkpoint.Worktree, nil
}

func (c *workerCheckpoint) ensureLifecycle(runner, branch, baseBranch string, expectPR bool) {
	if c.Lifecycle == nil {
		c.Lifecycle = lifecycle.NewState(lifecycle.AgentManagedWithFallbackPolicy(runner, expectPR), branch, baseBranch)
		return
	}
	c.Lifecycle.Normalize()
	if c.Lifecycle.Branch == "" {
		c.Lifecycle.Branch = strings.TrimSpace(branch)
	}
	if c.Lifecycle.BaseBranch == "" {
		c.Lifecycle.BaseBranch = strings.TrimSpace(baseBranch)
	}
}

func (c *workerCheckpoint) markLifecyclePushAndPR(branch, baseBranch string, prNumber int64, prURL string, pushed, adopted bool) {
	c.ensureLifecycle("worker", branch, baseBranch, true)
	activeBranch := branch
	activeBaseBranch := baseBranch
	provenance := lifecycle.BranchProvenancePlanned
	if c.Lifecycle.BranchProvenance == lifecycle.BranchProvenanceAgentMigrated && strings.TrimSpace(c.Lifecycle.ActiveBranch) != "" {
		activeBranch = c.Lifecycle.ActiveBranch
		activeBaseBranch = firstNonEmpty(c.Lifecycle.ActiveBaseBranch, baseBranch)
		provenance = lifecycle.BranchProvenanceAgentMigrated
	}
	c.Lifecycle.SetActiveBranch(activeBranch, activeBaseBranch, provenance)
	c.Lifecycle.Pushed = c.Lifecycle.Pushed || pushed
	if pushed {
		c.Lifecycle.Actions.Push = lifecycle.ActionSourceFallback
	}
	if prNumber > 0 {
		c.Lifecycle.PRNumber = prNumber
	}
	if prURL != "" {
		c.Lifecycle.PRURL = prURL
	}
	if adopted {
		c.Lifecycle.PRAdopted = true
	}
	if (prNumber > 0 || prURL != "") && c.Lifecycle.Actions.PR == lifecycle.ActionSourceNone {
		c.Lifecycle.Actions.PR = lifecycle.ActionSourceFallback
	}
	c.Lifecycle.Normalize()
}

func (c *workerCheckpoint) markLifecycleAgentPullRequest(branch, baseBranch string, pr checkpointPullPR) {
	c.ensureLifecycle("worker", branch, baseBranch, true)
	c.Lifecycle.RecordAgentBranch(branch, baseBranch)
	provenance := lifecycle.BranchProvenancePlanned
	if plannedBranch := strings.TrimSpace(c.Lifecycle.PlannedBranch); plannedBranch != "" && !strings.EqualFold(plannedBranch, strings.TrimSpace(branch)) {
		provenance = lifecycle.BranchProvenanceAgentMigrated
	}
	c.Lifecycle.SetActiveBranch(branch, baseBranch, provenance)
	c.Lifecycle.Pushed = true
	c.Lifecycle.Actions.Push = lifecycle.ActionSourceAgent
	c.Lifecycle.PRNumber = pr.Number
	c.Lifecycle.PRURL = pr.URL
	c.Lifecycle.PRAdopted = true
	c.Lifecycle.Actions.PR = lifecycle.ActionSourceAgent
	c.Lifecycle.Normalize()
}

func buildWorkerFallbackCommitMessage(work workerInput) string {
	title := strings.TrimSpace(work.Title)
	if title == "" {
		title = "worker run"
	}
	return fmt.Sprintf("worker: %s", title)
}

func (r *Runner) renamePlannerSpecPullRequestAfterTakeover(ctx context.Context, work workerInput, cwd string) error {
	if r.github == nil || work.ExecutionMode != "push-existing" || work.PRNumber <= 0 {
		return nil
	}
	current, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: work.Repo, PRNumber: work.PRNumber, CWD: cwd})
	if err != nil {
		return err
	}
	if !isPlannerSpecPullRequestTitle(current.Title) {
		return nil
	}
	if work.PRTitle != "" && strings.TrimSpace(current.Title) != strings.TrimSpace(work.PRTitle) {
		return nil
	}
	work.PRTitle = current.Title
	title := implementationPullRequestTitle(work)
	if title == "" || title == strings.TrimSpace(current.Title) {
		return nil
	}
	return r.github.UpdatePullRequestTitle(ctx, UpdatePullRequestTitleInput{Repo: work.Repo, PRNumber: work.PRNumber, Title: title, CWD: cwd})
}

func isPlannerSpecPullRequestTitle(title string) bool {
	title = strings.TrimSpace(title)
	return strings.HasPrefix(strings.ToLower(title), "spec:")
}

func implementationPullRequestTitle(work workerInput) string {
	title := strings.TrimSpace(work.Title)
	if title != "" && !isPlannerSpecPullRequestTitle(title) && title != "Worker run" {
		return title
	}
	prTitle := strings.TrimSpace(work.PRTitle)
	if len(prTitle) >= len("Spec:") && strings.EqualFold(prTitle[:len("Spec:")], "Spec:") {
		if stripped := strings.TrimSpace(prTitle[len("Spec:"):]); stripped != "" {
			return stripped
		}
	}
	if stripped := strings.TrimSpace(strings.TrimPrefix(prTitle, "Spec:")); stripped != "" {
		return stripped
	}
	return title
}

func (r *Runner) normalizePullRequestDisclosure(ctx context.Context, run storage.RunRecord, repo string, prNumber int64, cwd string, force bool) error {
	if r.github == nil || prNumber <= 0 || !r.disclosure.Enabled || !r.disclosure.Channels.PullRequest {
		return nil
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		return err
	}
	if !force && !disclosure.HasMarkdownStamp(detail.Body) {
		return nil
	}
	body := r.stampPullRequestDisclosure(run, detail.Body)
	if body == detail.Body {
		return nil
	}
	agent, model := r.disclosureIdentity(run)
	return r.github.UpdatePullRequestBody(ctx, UpdatePullRequestBodyInput{Repo: repo, PRNumber: prNumber, Body: body, CWD: cwd, DisclosureAgent: agent, DisclosureModel: model})
}

func (r *Runner) lifecycleAgentCreatedPullRequest(ctx context.Context, currentLoopID, repo string, state *lifecycle.State, expectedBranch, expectedBaseBranch, cwd string) (checkpointPullPR, string, bool, error) {
	if r.github == nil || state == nil || state.Actions.PR != lifecycle.ActionSourceAgent || state.PRNumber <= 0 {
		return checkpointPullPR{}, "", false, nil
	}
	expectedBranch = strings.TrimSpace(expectedBranch)
	expectedBaseBranch = strings.TrimSpace(expectedBaseBranch)
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: repo, PRNumber: state.PRNumber, CWD: cwd})
	if err != nil {
		return checkpointPullPR{}, "", false, err
	}
	prNumber := firstNonZero(detail.Number, state.PRNumber)
	headBranch := strings.TrimSpace(detail.HeadRefName)
	baseBranch := strings.TrimSpace(detail.BaseRefName)
	agentBranch := firstNonEmpty(state.AgentBranch, headBranch)
	migratedBranch := expectedBranch != "" && headBranch != "" && !strings.EqualFold(headBranch, expectedBranch)
	reject := func(reason string) (checkpointPullPR, string, bool, error) {
		message := fmt.Sprintf("Agent created PR #%d on branch %s but worker could not adopt it: %s", prNumber, firstNonEmpty(headBranch, agentBranch, "unknown"), reason)
		return checkpointPullPR{}, "", false, &runpipe.LoopError{Message: message, Kind: runpipe.FailureManualIntervention}
	}
	if !migratedBranch {
		if prState := strings.TrimSpace(detail.State); prState != "" && !strings.EqualFold(prState, "open") {
			return checkpointPullPR{}, "", false, nil
		}
		if expectedBaseBranch != "" {
			if reportedBase := strings.TrimSpace(state.AgentBaseBranch); reportedBase != "" && !strings.EqualFold(reportedBase, expectedBaseBranch) {
				return checkpointPullPR{}, "", false, nil
			}
			if baseBranch != "" && !strings.EqualFold(baseBranch, expectedBaseBranch) {
				return checkpointPullPR{}, "", false, nil
			}
		}
		return checkpointPullPR{Number: prNumber, URL: firstNonEmpty(strings.TrimSpace(detail.URL), strings.TrimSpace(state.PRURL)), HeadSHA: strings.TrimSpace(detail.HeadSHA)}, firstNonEmpty(headBranch, expectedBranch), true, nil
	}
	if prState := strings.TrimSpace(detail.State); prState != "" && !strings.EqualFold(prState, "open") {
		return reject(fmt.Sprintf("PR is %s", prState))
	}
	if agentBranch != "" && strings.EqualFold(agentBranch, expectedBranch) && headBranch != "" && !strings.EqualFold(headBranch, expectedBranch) {
		return reject(fmt.Sprintf("expected head branch %s, got %s", expectedBranch, firstNonEmpty(headBranch, "unknown")))
	}
	if expectedBaseBranch != "" {
		if reportedBase := strings.TrimSpace(state.AgentBaseBranch); reportedBase != "" && !strings.EqualFold(reportedBase, expectedBaseBranch) {
			return reject(fmt.Sprintf("expected base %s, got %s", expectedBaseBranch, reportedBase))
		}
		if baseBranch != "" && !strings.EqualFold(baseBranch, expectedBaseBranch) {
			return reject(fmt.Sprintf("expected base %s, got %s", expectedBaseBranch, firstNonEmpty(baseBranch, "unknown")))
		}
	}
	if len(state.CommitSHAs) == 0 {
		return reject("missing lifecycle commit evidence")
	}
	headSHA := strings.TrimSpace(detail.HeadSHA)
	if headSHA == "" {
		return reject("missing PR head SHA")
	}
	if !containsString(state.CommitSHAs, headSHA) {
		return reject(fmt.Sprintf("PR head %s does not match lifecycle commits", headSHA))
	}
	if conflict, err := r.activeWorkerLoopClaimingPullRequest(ctx, currentLoopID, repo, prNumber); err != nil {
		return checkpointPullPR{}, "", false, err
	} else if conflict != "" {
		return reject(conflict)
	}
	return checkpointPullPR{Number: prNumber, URL: firstNonEmpty(strings.TrimSpace(detail.URL), strings.TrimSpace(state.PRURL)), HeadSHA: headSHA}, headBranch, true, nil
}

func shouldPersistPullRequestReference(loop storage.LoopRecord, pr checkpointPullPR) bool {
	if pr.Number <= 0 {
		return false
	}
	if derefInt64(loop.PRNumber) != pr.Number {
		return true
	}
	if stringFromAnyDefault(parseJSONObject(loop.MetadataJSON)["prUrl"]) != pr.URL {
		return true
	}
	return derefString(loop.TargetID) != fmt.Sprintf("pr:%s:%d", derefString(loop.Repo), pr.Number)
}

func (r *Runner) stampPullRequestDisclosure(run storage.RunRecord, body string) string {
	if !r.disclosure.Enabled || !r.disclosure.Channels.PullRequest {
		return body
	}
	agent, model := r.disclosureIdentity(run)
	stamper := disclosure.Stamper{Config: r.disclosure, Agent: agent, Model: model}
	return stamper.Markdown(body, "worker", disclosure.ChannelPullRequest)
}

// disclosureIdentity returns agent/model for disclosure stamps from the run
// snapshot when present; falls back to runner identity on empty snapshot or
// parse errors (stamp-only paths must not fail the run).
func (r *Runner) disclosureIdentity(run storage.RunRecord) (agent, model string) {
	vendor, modelPtr, _, _, err := config.IdentityFromRunSnapshot(run.AgentSnapshotJSON, r.agentRuntime, r.agentModel, r.agentProfileID)
	if err != nil {
		// Present but invalid snapshot must not fall back to live runner identity.
		return "", ""
	}
	return vendor, derefString(modelPtr)
}

func buildWorkerPromptWithInstructions(repoRootPath string, projectID string, instructionConfig config.Config, work workerInput, plan *checkpointPlan, allowAgentPRCreation bool, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) (string, config.CustomInstructionBlock, error) {
	parts := []string{}
	if work.ExecutionMode == "push-existing" {
		parts = append(parts, fmt.Sprintf("Continue implementing on existing pull request %s#%d.", work.Repo, work.PRNumber))
	} else {
		parts = append(parts, fmt.Sprintf("Create a pull request for: %s", work.Title))
	}
	if work.Prompt != "" {
		parts = append(parts, "User prompt:\n"+work.Prompt)
	}
	parts = append(parts, fmt.Sprintf("Repository: %s", work.Repo), fmt.Sprintf("Base branch: %s", work.BaseBranch))
	specBlock, err := readSpecBlock(repoRootPath, work.SpecPath)
	if err != nil {
		return "", config.CustomInstructionBlock{}, err
	}
	if specBlock != "" {
		if work.ExecutionMode == "push-existing" {
			parts = append(parts, fmt.Sprintf("Do not modify the spec file at %s.", work.SpecPath))
		}
		parts = append(parts, specBlock)
	}
	if plan != nil && len(plan.Items) > 0 {
		lines := []string{"Execution plan:"}
		for _, item := range plan.Items {
			lines = append(lines, "- "+item)
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	parts = append(parts, testFirstDeliveryPrompt)
	instructionBlock := config.BuildCustomInstructionBlock(instructionConfig, projectID, "worker")
	if instructionBlock.Text != "" {
		parts = append(parts, instructionBlock.Text)
	}
	if allowAgentPRCreation {
		parts = append(parts, buildAgentPullRequestInstruction(work))
		parts = append(parts, "Make the necessary code changes, validate them, and ensure the branch and pull request are left in a consistent state.")
		parts = append(parts, lifecycle.PromptInstruction("worker", work.Branch, work.BaseBranch, true, true, disclosureCfg, agentRuntime, agentModel))
	} else {
		parts = append(parts, "Make the necessary code changes, validate them, and leave the branch ready for PR creation.")
		parts = append(parts, noRemoteLifecyclePromptInstruction("worker", work.Branch, work.BaseBranch, disclosureCfg, agentRuntime, agentModel))
	}
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n")), instructionBlock, nil
}

func customInstructionConfig(value *config.Config) config.Config {
	if value == nil {
		cfg, _ := config.Normalize("")
		cfg.Instructions.Enabled = false
		return cfg
	}
	return *value
}

func noRemoteLifecyclePromptInstruction(runner, branch, baseBranch string, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	return strings.Join([]string{
		"Agent-managed git/PR lifecycle policy: remote actions disabled by Looper configuration.",
		"Before finishing: inspect git status, staged and unstaged diffs, untracked files, and recent commit style; commit only relevant non-secret changes if needed; do not push branches, create pull requests, update pull request metadata, or otherwise change remote review state.",
		lifecycle.DisclosurePromptInstruction(runner, disclosureCfg, agentRuntime, agentModel),
		"Because remote PR actions are disabled for this run, do not create or update PR bodies; any PR disclosure stamping can only happen during a later Looper-managed remote reconciliation step.",
		"Include a git_pr_lifecycle object in the final " + "__LOOPER_RESULT__" + " JSON with branch, baseBranch, commitShas, pushed, prNumber, prUrl, prAdopted, and actions {commit,push,pr}; set each action to a plain string source like \"agent\" or \"none\", not a nested object.",
		fmt.Sprintf("Expected lifecycle runner=%q branch=%q baseBranch=%q expectPush=%t expectPR=%t fallbackAllowed=%t.", runner, branch, baseBranch, false, false, true),
	}, "\n")
}

func buildAgentPullRequestInstruction(work workerInput) string {
	parts := []string{
		"When the implementation is ready and validation passes, use the GitHub CLI (`gh`) to create the pull request yourself.",
		"Before creating a PR, check whether one already exists for the current branch and avoid duplicates.",
		"Write a concise, accurate PR title and a structured body that explains the actual changes and why they were made.",
		fmt.Sprintf("Target base branch: %s.", work.BaseBranch),
	}
	if work.IssueNumber > 0 {
		parts = append(parts, fmt.Sprintf("Include `Closes %s` in the PR body.", formatIssueClosingReference(work.Repo, work.IssueRepo, work.IssueNumber)))
	}
	if work.ExecutionMode == "push-existing" {
		parts = append(parts, "If the existing PR title still has the planner/spec-generated `Spec: ...` format after implementation is pushed, rename it to an implementation-oriented title; preserve human-edited titles.")
	}
	return strings.Join(parts, "\n")
}

func formatIssueClosingReference(prRepo, issueRepo string, issueNumber int64) string {
	if issueNumber <= 0 {
		return ""
	}
	issueRepo = strings.TrimSpace(issueRepo)
	if issueRepo == "" || strings.EqualFold(strings.TrimSpace(prRepo), issueRepo) {
		return fmt.Sprintf("#%d", issueNumber)
	}
	return fmt.Sprintf("%s#%d", issueRepo, issueNumber)
}

func formatIssueReference(issueRepo string, issueNumber int64) string {
	if issueNumber <= 0 {
		return ""
	}
	issueRepo = strings.TrimSpace(issueRepo)
	if issueRepo == "" {
		return fmt.Sprintf("#%d", issueNumber)
	}
	return fmt.Sprintf("%s#%d", issueRepo, issueNumber)
}

func issueRepoFromURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if parsed.Host == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || !strings.EqualFold(parts[2], "issues") {
		return ""
	}
	if _, err := strconv.ParseInt(parts[3], 10, 64); err != nil {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func issueLookupRepo(work workerInput) string {
	return firstNonEmpty(strings.TrimSpace(work.IssueRepo), issueRepoFromURL(work.IssueURL), work.Repo)
}

func resolvedIssueRepo(work workerInput, issue IssueDetail) string {
	return firstNonEmpty(strings.TrimSpace(work.IssueRepo), issueRepoFromURL(issue.URL), issueRepoFromURL(work.IssueURL), work.Repo)
}

// maxSpecFileBytes bounds the spec content embedded into a worker prompt.
const maxSpecFileBytes = 256 << 10

func readSpecBlock(projectRepoPath, specPath string) (string, error) {
	if specPath == "" {
		return "", nil
	}
	content, err := readSpecFile(projectRepoPath, specPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read worker spec: %w", err)
	}
	return fmt.Sprintf("Spec (%s):\n%s", specPath, string(content)), nil
}

// readSpecFile reads a repository-relative spec file under projectRepoPath.
// Absolute paths, paths escaping the repo root, symlinks, non-regular files,
// and files larger than maxSpecFileBytes are rejected so worker metadata
// cannot make the daemon embed arbitrary local files into a model prompt.
func readSpecFile(projectRepoPath, specPath string) ([]byte, error) {
	if filepath.IsAbs(specPath) {
		return nil, fmt.Errorf("spec path must be repository-relative: %s", specPath)
	}
	cleanPath := filepath.Clean(specPath)
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("spec path escapes repository root: %s", specPath)
	}
	file, err := openSpecFileBeneath(projectRepoPath, cleanPath)
	if err != nil {
		return nil, fmt.Errorf("open spec path %s: %w", specPath, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("spec path is not a regular file: %s", specPath)
	}
	if info.Size() > maxSpecFileBytes {
		return nil, fmt.Errorf("spec file exceeds %d bytes: %s", maxSpecFileBytes, specPath)
	}
	content, err := readBoundedSpec(file)
	if err != nil {
		return nil, fmt.Errorf("read spec file %s: %w", specPath, err)
	}
	return content, nil
}

func readBoundedSpec(reader io.Reader) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, maxSpecFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxSpecFileBytes {
		return nil, fmt.Errorf("spec content exceeds %d bytes", maxSpecFileBytes)
	}
	return content, nil
}

func buildPullRequestBody(work workerInput, plan *checkpointPlan, execution *checkpointExecution) string {
	lines := []string{"## Summary"}
	if plan != nil {
		for _, item := range plan.Items {
			lines = append(lines, "- "+item)
		}
	}
	if execution != nil && execution.Summary != "" {
		lines = append(lines, "", "## Verification", execution.Summary)
	} else {
		lines = append(lines, "", "## Verification", "No agent-supplied verification summary was recorded; document automated coverage or explain why it is not feasible before merge.")
	}
	if work.IssueNumber > 0 {
		lines = append(lines, "", "Issue: "+formatIssueReference(firstNonEmpty(work.IssueRepo, work.Repo), work.IssueNumber))
	}
	if work.IssueURL != "" {
		lines = append(lines, "", fmt.Sprintf("Issue URL: %s", work.IssueURL))
	}
	if work.SpecPath != "" {
		lines = append(lines, "", fmt.Sprintf("Spec: %s", work.SpecPath))
	}
	if work.Reproduction != nil {
		lines = append(lines, "", "Reproduction manifest: "+reproducer.ManifestPath, "Reproduction test: "+work.Reproduction.TestName+" ("+work.Reproduction.TestPath+")")
	}
	if work.Prompt != "" {
		lines = append(lines, "", fmt.Sprintf("Prompt: %s", work.Prompt))
	}
	if work.IssueNumber > 0 {
		lines = append(lines, "", "Closes "+formatIssueClosingReference(work.Repo, work.IssueRepo, work.IssueNumber))
	}
	return strings.Join(lines, "\n")
}

func hydrateWorkerInputFromIssue(work workerInput, issue IssueDetail) workerInput {
	fallbackTitle := buildDefaultIssueWorkerTitle(work.Repo, issue.Number)
	if work.Title == "Worker run" || work.Title == fallbackTitle {
		work.Title = issue.Title
	}
	work.IssueRepo = resolvedIssueRepo(work, issue)
	work.Prompt = buildIssuePrompt(work.IssueRepo, issue)
	work.IssueURL = firstNonEmpty(issue.URL, work.IssueURL)
	return work
}

func validateWorkerIssueTarget(repo string, issueNumber int64, issue IssueDetail) error {
	reference := formatIssueReference(repo, issueNumber)
	if issue.IsPullRequest {
		return &runpipe.LoopError{Message: fmt.Sprintf("%s is a pull request, not an issue; worker issue targets must reference an open GitHub issue", reference), Kind: runpipe.FailureNonRetryable}
	}
	if issue.State != "" && !strings.EqualFold(issue.State, "OPEN") {
		return &runpipe.LoopError{Message: fmt.Sprintf("%s is %s; worker issue targets must reference an open GitHub issue", reference, strings.ToLower(issue.State)), Kind: runpipe.FailureNonRetryable}
	}
	return nil
}

func buildIssuePrompt(repo string, issue IssueDetail) string {
	parts := []string{fmt.Sprintf("Implement issue %s#%d: %s", repo, issue.Number, issue.Title)}
	if issue.Body != "" {
		parts = append(parts, "Issue body:\n"+issue.Body)
	}
	if issue.URL != "" {
		parts = append(parts, "Issue URL: "+issue.URL)
	}
	return strings.Join(parts, "\n\n")
}

func buildWorkerBranchName(work workerInput, loopID string) string {
	loopHash := buildWorkerLoopHash(loopID)
	if work.IssueNumber > 0 {
		return fmt.Sprintf("looper/%d-%s-%s", work.IssueNumber, buildWorkerSlug(work.Title), loopHash)
	}
	return "looper/" + loopHash
}

func buildWorkerBranchAliases(work workerInput, loopID string) []string {
	branch := buildWorkerBranchName(work, loopID)
	legacy := strings.Replace(branch, "looper/", "looper/worker/", 1)
	if legacy == branch {
		return []string{branch}
	}
	return []string{branch, legacy}
}

func buildWorkerSlug(title string) string {
	words := strings.Split(slugify(title), "-")
	filtered := []string{}
	for _, word := range words {
		if word != "" {
			filtered = append(filtered, word)
		}
	}
	if len(filtered) > workerBranchSlugMaxWords {
		filtered = filtered[:workerBranchSlugMaxWords]
	}
	value := strings.TrimRight(strings.Join(filtered, "-"), "-")
	if len(value) > workerBranchSlugMaxLength {
		value = strings.TrimRight(value[:workerBranchSlugMaxLength], "-")
	}
	if value == "" {
		return "update"
	}
	return value
}

func shouldRestartWorkerFromDiscoverAfterPushFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "non-fast-forward") || strings.Contains(message, "remote head changed") || strings.Contains(message, "fetch first")
}

func buildWorkerLoopHash(loopID string) string {
	sum := sha1.Sum([]byte(loopID))
	value := hex.EncodeToString(sum[:])
	if len(value) > workerBranchHashLength {
		value = value[:workerBranchHashLength]
	}
	if value == "" {
		return "worker"
	}
	return value
}

func buildDefaultIssueWorkerTitle(repo string, issueNumber int64) string {
	return fmt.Sprintf("Implement %s#%d", repo, issueNumber)
}

func buildIssueTargetID(repo string, issueNumber int64) string {
	return fmt.Sprintf("issue:%s:%d", repo, issueNumber)
}

func buildWorkerIssueDedupeKey(projectID, repo string, issueNumber int64) string {
	return fmt.Sprintf("worker:%s:%s:%d", projectID, repo, issueNumber)
}

func workerLoopTracksIssue(loop storage.LoopRecord, projectID, repo string, issueNumber int64) bool {
	if loop.Type != "worker" || loop.ProjectID != projectID {
		return false
	}
	if loop.TargetType == "issue" && derefString(loop.TargetID) == buildIssueTargetID(repo, issueNumber) {
		return true
	}
	workerMeta, _ := parseJSONObject(loop.MetadataJSON)["worker"].(map[string]any)
	if int64FromAny(workerMeta["issueNumber"]) != issueNumber {
		return false
	}
	trackedRepo := firstNonEmpty(stringFromAnyDefault(workerMeta["issueRepo"]), stringFromAnyDefault(workerMeta["repo"]), derefString(loop.Repo))
	return strings.EqualFold(trackedRepo, repo)
}

func buildWorkerDiscoveryFingerprint(repo, baseBranch string, issue IssueSummary) string {
	return loops.ComputeDiscoveryFingerprint(
		"worker",
		repo,
		fmt.Sprintf("%d", issue.Number),
		strings.TrimSpace(baseBranch),
		strings.TrimSpace(issue.Title),
		strings.TrimSpace(issue.Body),
		strings.TrimSpace(issue.URL),
		strings.Join(loops.CanonicalSortedStrings(issue.Labels), ","),
		strings.Join(loops.CanonicalSortedStrings(issue.Assignees), ","),
	)
}

func stampWorkerFailedDiscoveryFingerprint(updated *storage.LoopRecord, queueItem storage.QueueItemRecord) {
	metadata := parseJSONObject(queueItem.PayloadJSON)
	fingerprint := stringFromAnyDefault(metadata["discoveryFingerprint"])
	if strings.TrimSpace(fingerprint) == "" {
		return
	}
	merged, err := loops.MergeLastFailedDiscoveryFingerprint(updated.MetadataJSON, fingerprint)
	if err != nil {
		return
	}
	updated.MetadataJSON = runpipe.StringPtr(merged)
}

func normalizeLogin(login string) string { return strings.ToLower(strings.TrimSpace(login)) }

func includesLogin(values []string, target string) bool {
	target = normalizeLogin(target)
	for _, value := range values {
		if normalizeLogin(value) == target {
			return true
		}
	}
	return false
}

func shouldClaimWorkerIssue(issue IssueSummary, login string, policy DiscoveryPolicy) bool {
	if networkpolicy.IsRouted(policy.RoutedClaimPolicy) {
		if !config.LabelsMatch(issue.Labels, policy.Labels, policy.LabelMode) {
			return false
		}
		decision := networkpolicy.EvaluateWorker(policy.RoutedClaimPolicy, issue.Labels, issue.AssigneeUsers)
		return decision.Allowed
	}
	if policy.RequireAssigneeCurrentUser && !includesLogin(issue.Assignees, login) {
		return false
	}
	return config.LabelsMatch(issue.Labels, policy.Labels, policy.LabelMode)
}

func safeIssueQueryLabel(labels []string) string {
	for _, label := range labels {
		if strings.TrimSpace(label) != "" {
			return label
		}
	}
	return ""
}

func (r *Runner) listOpenIssuesForDiscovery(ctx context.Context, input ListOpenIssuesInput, policy DiscoveryPolicy, requiredTargetLabel string) ([]IssueSummary, error) {
	if strings.TrimSpace(requiredTargetLabel) != "" {
		return r.listOpenIssuesForTargetedDiscovery(ctx, input, policy, requiredTargetLabel)
	}
	if policy.LabelMode != config.LabelModeAny {
		input.Labels = uniqueNonEmptyLabels(policy.Labels)
		input.Label = safeIssueQueryLabel(input.Labels)
		return r.github.ListOpenIssues(ctx, input)
	}
	queryLabels := uniqueNonEmptyLabels(policy.Labels)
	if len(queryLabels) == 0 {
		return r.github.ListOpenIssues(ctx, input)
	}
	issuePages := make([][]IssueSummary, 0, len(queryLabels))
	for _, label := range queryLabels {
		queryInput := input
		queryInput.Label = label
		issues, err := r.github.ListOpenIssues(ctx, queryInput)
		if err != nil {
			return nil, err
		}
		issuePages = append(issuePages, issues)
	}
	return mergeIssuePages(issuePages, effectiveIssueLimit(input.Limit)), nil
}

func (r *Runner) listOpenIssuesForTargetedDiscovery(ctx context.Context, input ListOpenIssuesInput, policy DiscoveryPolicy, requiredTargetLabel string) ([]IssueSummary, error) {
	targetLabel := strings.TrimSpace(requiredTargetLabel)
	if targetLabel == "" {
		return r.github.ListOpenIssues(ctx, input)
	}
	queryLabels := uniqueNonEmptyLabels(policy.Labels)
	if len(queryLabels) == 0 {
		queryInput := input
		queryInput.Labels = []string{targetLabel}
		queryInput.Label = targetLabel
		return r.github.ListOpenIssues(ctx, queryInput)
	}
	if policy.LabelMode != config.LabelModeAny {
		queryInput := input
		queryInput.Labels = append([]string{targetLabel}, queryLabels...)
		queryInput.Label = safeIssueQueryLabel(queryInput.Labels)
		return r.github.ListOpenIssues(ctx, queryInput)
	}
	issuePages := make([][]IssueSummary, 0, len(queryLabels))
	for _, label := range queryLabels {
		queryInput := input
		queryInput.Labels = []string{targetLabel, label}
		queryInput.Label = safeIssueQueryLabel(queryInput.Labels)
		issues, err := r.github.ListOpenIssues(ctx, queryInput)
		if err != nil {
			return nil, err
		}
		issuePages = append(issuePages, issues)
	}
	return mergeIssuePages(issuePages, effectiveIssueLimit(input.Limit)), nil
}

func mergeIssuePages(pages [][]IssueSummary, limit int) []IssueSummary {
	seenIssues := map[int64]struct{}{}
	merged := []IssueSummary{}
	for index := 0; len(merged) < limit; index++ {
		anyPageHasIndex := false
		for _, page := range pages {
			if index >= len(page) {
				continue
			}
			anyPageHasIndex = true
			issue := page[index]
			if _, ok := seenIssues[issue.Number]; ok {
				continue
			}
			seenIssues[issue.Number] = struct{}{}
			merged = append(merged, issue)
			if len(merged) >= limit {
				break
			}
		}
		if !anyPageHasIndex {
			break
		}
	}
	return merged
}

func effectiveIssueLimit(limit int) int {
	if limit <= 0 {
		return defaultIssueLimit
	}
	return limit
}

func uniqueNonEmptyLabels(labels []string) []string {
	seen := map[string]struct{}{}
	result := []string{}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, label)
	}
	return result
}

func mergeLoopMetadataJSON(current *string, updates map[string]any) (string, error) {
	// Loop metadata mutations share the strict decoder: a malformed stored
	// value blocks the merge instead of being replaced with only the updates.
	metadata, err := loops.DecodeMetadataObjectForWrite(current)
	if err != nil {
		return "", err
	}
	for key, value := range updates {
		metadata[key] = value
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func parseJSONObject(raw *string) map[string]any {
	if raw == nil || *raw == "" {
		return map[string]any{}
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(*raw), &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

func mergeWorkerMetadata(metadata map[string]any, work workerInput) map[string]any {
	workerMeta := map[string]any{}
	if existing, ok := metadata["worker"].(map[string]any); ok {
		for key, value := range existing {
			workerMeta[key] = value
		}
	}
	workJSON := parseJSONObject(runpipe.StringPtr(runpipe.MustMarshalJSON(work)))
	for key, value := range workJSON {
		workerMeta[key] = value
	}
	return workerMeta
}

func slugify(value string) string {
	value = strings.ToLower(value)
	parts := make([]rune, 0, len(value))
	dash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			parts = append(parts, r)
			dash = false
			continue
		}
		if !dash {
			parts = append(parts, '-')
			dash = true
		}
	}
	return strings.Trim(string(parts), "-")
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, strings.TrimSpace(value))
		}
	}
	return result
}

func appendUniqueStrings(dst []string, values ...string) []string {
	seen := map[string]bool{}
	for _, value := range dst {
		seen[value] = true
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		dst = append(dst, value)
	}
	return dst
}

func stringSliceFromAny(value any) []string {
	array, ok := value.([]any)
	if !ok {
		stringsValue, ok := value.([]string)
		if ok {
			return append([]string(nil), stringsValue...)
		}
		return nil
	}
	result := make([]string, 0, len(array))
	for _, item := range array {
		if text, ok := item.(string); ok && text != "" {
			result = append(result, text)
		}
	}
	return result
}

func int64FromAny(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}

func stringFromAnyDefault(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func boolFromAny(value any) bool {
	if flag, ok := value.(bool); ok {
		return flag
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func prefixIfPresent(prefix, value string) string {
	if value == "" {
		return ""
	}
	return prefix + value
}

func pullRequestNumber(pr *checkpointPullPR) int64 {
	if pr == nil {
		return 0
	}
	return pr.Number
}

func (r *Runner) agentSnapshotJSONForNewRun(previous *storage.RunRecord, sticky, requireToolNetworkDenial, replaysAgentStep bool) (*string, error) {
	var previousSnapshot *string
	if previous != nil {
		previousSnapshot = previous.AgentSnapshotJSON
	}
	snapshotJSON, refreshed, legacyResume, err := config.ResolveRunAgentSnapshotJSONForValidationGate(previousSnapshot, sticky, requireToolNetworkDenial, replaysAgentStep, r.agentRuntime, r.agentModel, r.agentProfileID)
	if err != nil {
		return nil, err
	}
	if refreshed && r.logger != nil && previous != nil {
		r.logger.Warn("refreshed stale agent snapshot whose vendor cannot serve the validation gate; using current runner agent identity", map[string]any{
			"loopId":   previous.LoopID,
			"runId":    previous.ID,
			"vendor":   r.agentRuntime,
			"model":    derefString(r.agentModel),
			"previous": derefString(previousSnapshot),
		})
	}
	if legacyResume && r.logger != nil && previous != nil {
		r.logger.Warn("resuming run without agent_snapshot_json; using current runner agent identity", map[string]any{
			"loopId": previous.LoopID,
			"runId":  previous.ID,
			"vendor": r.agentRuntime,
			"model":  derefString(r.agentModel),
		})
	}
	return snapshotJSON, nil
}

// identityFromRun returns the vendor/model/profile that must drive this run.
// When the run has AgentSnapshotJSON, that identity is execution authority.
// model is a pointer so nil (unset) and non-nil empty (suppress) stay distinct.
func (r *Runner) identityFromRun(run storage.RunRecord) (vendor string, model *string, profile string, useSnapshot bool, err error) {
	return config.IdentityFromRunSnapshot(run.AgentSnapshotJSON, r.agentRuntime, r.agentModel, r.agentProfileID)
}

func agentRunSnapshotFields(vendor string, model *string, useSnapshot bool) (bool, string, *string) {
	if !useSnapshot {
		return false, "", nil
	}
	// Pass through including non-nil empty suppress so SnapshotModel stays
	// distinct from unset and ParamsForRoleVendor can strip params --model/-m.
	return true, vendor, model
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func int64Ptr(value int64) *int64 { return &value }

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func ternary[T any](condition bool, whenTrue, whenFalse T) T {
	if condition {
		return whenTrue
	}
	return whenFalse
}
