package planner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/agent"
	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/loops/failureclass"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

const (
	stepDiscoverIssues  PlannerStep = "discover-issues"
	stepPrepareWorktree PlannerStep = "prepare-worktree"
	stepWriteSpec       PlannerStep = "write-spec"
	stepPublish         PlannerStep = "publish"
	// Plane node H: grill (fresh adversarial pass) → review (independent verdict). Both
	// are no-ops for GitHub/forgejo projects (their spec is the PR, reviewed there).
	stepGrill  PlannerStep = "grill"
	stepReview PlannerStep = "review"
	stepNotify PlannerStep = "notify"

	discoveryLabel             = "looper:plan"
	plannerPRDedupeLookupLimit = 1000

	defaultAgentTimeout = 30 * time.Minute
	defaultClaimTTL     = 10 * time.Minute
	defaultRetryDelay   = 5 * time.Second
	maxRetryDelay       = 300 * time.Second
	defaultRetryMax     = 3
	defaultIssueLimit   = 30
)

var plannerStepSequence = []PlannerStep{stepDiscoverIssues, stepPrepareWorktree, stepWriteSpec, stepPublish, stepGrill, stepReview, stepNotify}

type PlannerStep string

type QueueFailureKind string

const (
	FailureRetryableTransient   QueueFailureKind = "retryable_transient"
	FailureRetryableAfterResume QueueFailureKind = "retryable_after_resume"
	FailureRecoverableInfra     QueueFailureKind = "recoverable_infra"
	FailureNonRetryable         QueueFailureKind = "non_retryable"
	FailureManualIntervention   QueueFailureKind = "manual_intervention"
	// FailureHITLInterrupt marks a step whose agent was deliberately killed to answer a
	// human's mid-run @bot (强操控). It is NOT a real failure: the run completes as
	// "interrupted" and is re-dispatched immediately so follow-up-resume answers them
	// from the MAIN session, without counting against the retry cap.
	FailureHITLInterrupt QueueFailureKind = "hitl_interrupt"
)

// plannerInterruptError signals that the step's agent was killed by a human mid-run
// @bot (result.Interrupted). handled specially in the run loop (interrupted, not failed).
func plannerInterruptError() *loopError {
	return &loopError{message: "agent interrupted by a human mid-run message", kind: FailureHITLInterrupt}
}

type IssueSummary struct {
	Number    int64
	Title     string
	Body      string
	URL       string
	Assignees []string
	Labels    []string
}

type IssueDetail struct {
	Number    int64
	Title     string
	Body      string
	URL       string
	Assignees []string
	Labels    []string
}

type PullRequestSummary struct {
	Number      int64
	URL         string
	State       string
	HeadRefName string
	BaseRefName string
}

type ListOpenPullRequestsInput struct {
	Repo  string
	CWD   string
	Limit int
}

type ListOpenIssuesInput struct {
	Repo     string
	CWD      string
	Limit    int
	Assignee string
	Label    string
	Labels   []string
}

type ViewIssueInput struct {
	Repo        string
	IssueNumber int64
	CWD         string
}

type CreatePullRequestInput struct {
	Repo       string
	HeadBranch string
	BaseBranch string
	Title      string
	Body       string
	CWD        string
}

type CreatePullRequestResult struct {
	Number int64
	URL    string
}

type ViewPullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type PullRequestDetail struct {
	Number      int64
	Title       string
	Body        string
	URL         string
	State       string
	HeadRefName string
	BaseRefName string
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

type UpdatePullRequestBodyInput struct {
	Repo     string
	PRNumber int64
	Body     string
	CWD      string
}

type IssueAssigneesInput struct {
	Repo        string
	IssueNumber int64
	Assignees   []string
	CWD         string
}

type GitHubGateway interface {
	ListOpenIssues(context.Context, ListOpenIssuesInput) ([]IssueSummary, error)
	ViewIssue(context.Context, ViewIssueInput) (IssueDetail, error)
	GetCurrentUserLogin(context.Context, string) (string, error)
	AddIssueAssignees(context.Context, IssueAssigneesInput) error
	ListOpenPullRequests(context.Context, ListOpenPullRequestsInput) ([]PullRequestSummary, error)
	ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error)
	CreatePullRequest(context.Context, CreatePullRequestInput) (CreatePullRequestResult, error)
	UpdatePullRequestBody(context.Context, UpdatePullRequestBodyInput) error
	AddPullRequestLabels(context.Context, PullRequestLabelsInput) error
	AddPullRequestReviewers(context.Context, PullRequestReviewersInput) error
	ClosePullRequest(context.Context, ClosePullRequestInput) error
}

// ClosePullRequestInput closes a pull request (used to retire a stray PR an agent
// opened on a Plane project, where the spec lives on a Plane page not a PR).
type ClosePullRequestInput struct {
	Repo     string
	PRNumber int64
	CWD      string
}

type CreateWorktreeInput struct {
	ProjectID         string
	RepoPath          string
	WorktreeRoot      string
	Branch            string
	BaseBranch        string
	ProtectedBranches []string
}

type CreateWorktreeResult struct {
	ID           string
	WorktreePath string
	Branch       string
	BaseBranch   string
}

type PushInput struct {
	RepoPath          string
	WorktreeRoot      string
	WorktreePath      string
	Branch            string
	Remote            string
	ProtectedBranches []string
}

type InspectHeadInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	BaseRef      string
}

type InspectHeadResult struct {
	HeadSHA               string
	NewCommitSHAs         []string
	HasUncommittedChanges bool
	ChangedFiles          []string
}

type CommitInput struct {
	RepoPath     string
	WorktreeRoot string
	WorktreePath string
	Message      string
}

type CommitResult struct{ CommitSHA string }

type GitGateway interface {
	CreateWorktree(context.Context, CreateWorktreeInput) (CreateWorktreeResult, error)
	InspectHead(context.Context, InspectHeadInput) (InspectHeadResult, error)
	Commit(context.Context, CommitInput) (CommitResult, error)
	Push(context.Context, PushInput) error
}

type AgentRunInput struct {
	ExecutionID string
	ProjectID   string
	LoopID      string
	RunID       string
	Prompt      string
	// NativeResumePrompt + NativeSessionID drive native agent-session resume: when a
	// finished planner loop is reactivated for a follow-up turn (线程永远可追问), the
	// write-spec step resumes the SAME planner session so it retains the full
	// spec-authoring history and revises the spec incrementally rather than replanning.
	NativeResumePrompt string
	NativeSessionID    string
	WorkingDirectory   string
	Timeout            time.Duration
	HeartbeatTimeout   time.Duration
	Metadata           map[string]any
	IdempotencyKey     string
}

type AgentResult struct {
	Status                       string
	Summary                      string
	ProductAsk                   string
	Stdout                       string
	Stderr                       string
	Commits                      []string
	Lifecycle                    *lifecycle.State
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
	// Interrupted is true when the agent was killed by a human's mid-run @bot (强操控),
	// so the step returns an interrupt (not a failure) and the run re-dispatches to
	// answer them from the same session.
	Interrupted bool
}

type AgentExecution interface {
	Wait(context.Context) (AgentResult, error)
}

type AgentExecutor interface {
	Start(context.Context, AgentRunInput) (AgentExecution, error)
}

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

type Options struct {
	DB                      *sql.DB
	Repos                   *storage.Repositories
	GitHub                  GitHubGateway
	Git                     GitGateway
	AgentExecutor           AgentExecutor
	Logger                  bootstrap.Logger
	Now                     func() time.Time
	AgentTimeout            time.Duration
	AgentIdleTimeout        time.Duration
	ClaimTTL                time.Duration
	AllowAutoPush           *bool
	Disclosure              *config.DisclosureConfig
	AgentRuntime            string
	CustomInstructions      *config.Config
	AgentModel              *string
	RetryBaseDelay          time.Duration
	RetryMaxAttempts        int64
	OnAgentExecutionStarted AgentExecutionStartedFunc
	OnQueueItemEnqueued     func()
	// PostThreadNote posts a plain-text reply into a loop's Feishu thread (node H
	// touchpoints: spec-draft FYI, grill transcript), optionally @-mentioning open_ids.
	PostThreadNote func(ctx context.Context, loopID, text string, mentionOpenIDs []string) error
	// PostThreadCard posts a header-less interactive card into a loop's Feishu thread —
	// used for the node-H product-decision ask so the product-language body renders with
	// structure (bold sub-headers, line breaks) instead of a flat text wall. Falls back
	// to PostThreadNote when unset (e.g. tests).
	PostThreadCard  func(ctx context.Context, loopID, body string, mentionOpenIDs []string) error
	DiscoveryPolicy DiscoveryPolicy
	// PlaneDoc resolves a Plane spec-document gateway + Plane project UUID for a
	// project whose task source is Plane (§8 flowchart). nil / (…,false) → the
	// project keeps the repo-file spec path (github/forgejo). Set for plane projects.
	PlaneDoc PlaneDocResolver
}

// PlaneDocResolver returns a project's Plane spec-document gateway + Plane project
// UUID, or ok=false for non-Plane projects.
type PlaneDocResolver func(projectID string) (*planedoc.Gateway, string, bool)

type DiscoveryPolicy struct {
	AutoDiscovery              bool
	Labels                     []string
	LabelMode                  config.LabelMode
	RequireAssigneeCurrentUser bool
}

type Runner struct {
	db                      *sql.DB
	repos                   *storage.Repositories
	github                  GitHubGateway
	git                     GitGateway
	agentExecutor           AgentExecutor
	logger                  bootstrap.Logger
	now                     func() time.Time
	agentTimeout            time.Duration
	agentIdleTimeout        time.Duration
	claimTTL                time.Duration
	allowAutoPush           bool
	disclosure              config.DisclosureConfig
	agentRuntime            string
	customInstructions      config.Config
	projectRoleConfig       *config.Config
	agentModel              string
	retryBaseDelay          time.Duration
	retryMaxAttempts        int64
	onAgentExecutionStarted AgentExecutionStartedFunc
	onQueueItemEnqueued     func()
	postThreadNote          func(ctx context.Context, loopID, text string, mentionOpenIDs []string) error
	postThreadCard          func(ctx context.Context, loopID, body string, mentionOpenIDs []string) error
	discoveryPolicy         DiscoveryPolicy
	planeDoc                PlaneDocResolver
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Limit     int
	Snapshot  *githubinfra.DiscoverySnapshot
}

type DiscoveryResult struct {
	QueueItems     []storage.QueueItemRecord
	CreatedLoopIDs []string
	Skipped        int
}

type ProcessResult struct {
	LoopID            string
	RunID             string
	QueueItemID       string
	Status            string
	Summary           string
	FailureKind       QueueFailureKind
	PullRequestNumber int64
}

type plannerCheckpoint struct {
	ResumePolicy   string                  `json:"resumePolicy,omitempty"`
	Issue          *checkpointIssue        `json:"issue,omitempty"`
	ClaimedLockKey string                  `json:"claimedLockKey,omitempty"`
	Worktree       *checkpointWorktree     `json:"worktree,omitempty"`
	WriteSpec      *checkpointWriteSpec    `json:"writeSpec,omitempty"`
	Lifecycle      *lifecycle.State        `json:"gitPrLifecycle,omitempty"`
	Publish        *checkpointPublishState `json:"publish,omitempty"`
	Notify         *checkpointNotify       `json:"notify,omitempty"`
	SkipReason     string                  `json:"skipReason,omitempty"`
}

type checkpointIssue struct {
	Repo               string   `json:"repo,omitempty"`
	IssueNumber        int64    `json:"issueNumber,omitempty"`
	Title              string   `json:"title,omitempty"`
	Body               string   `json:"body,omitempty"`
	URL                string   `json:"url,omitempty"`
	Assignees          []string `json:"assignees,omitempty"`
	Labels             []string `json:"labels,omitempty"`
	CurrentUserLogin   string   `json:"currentUserLogin,omitempty"`
	SpecPath           string   `json:"specPath,omitempty"`
	RequestedReviewers []string `json:"requestedReviewers,omitempty"`
}

type checkpointWorktree struct {
	ID         string `json:"id,omitempty"`
	Path       string `json:"path,omitempty"`
	Branch     string `json:"branch,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	SpecPath   string `json:"specPath,omitempty"`
}

type checkpointWriteSpec struct {
	Status                       string           `json:"status,omitempty"`
	Summary                      string           `json:"summary,omitempty"`
	Stdout                       string           `json:"stdout,omitempty"`
	Commits                      []string         `json:"commits,omitempty"`
	Lifecycle                    *lifecycle.State `json:"gitPrLifecycle,omitempty"`
	GitReconciled                bool             `json:"gitReconciled,omitempty"`
	TimeoutType                  string           `json:"timeoutType,omitempty"`
	ConfiguredIdleTimeoutSeconds int64            `json:"configuredIdleTimeoutSeconds,omitempty"`
	ConfiguredMaxRuntimeSeconds  int64            `json:"configuredMaxRuntimeSeconds,omitempty"`
	ElapsedRuntimeSeconds        int64            `json:"elapsedRuntimeSeconds,omitempty"`
	LastProgressAt               string           `json:"lastProgressAt,omitempty"`
}

type checkpointPullRequest struct {
	Number int64  `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	Body   string `json:"body,omitempty"`
}

type checkpointPublishState struct {
	Pushed         bool                   `json:"pushed,omitempty"`
	PullRequest    *checkpointPullRequest `json:"pullRequest,omitempty"`
	LabelsAdded    []string               `json:"labelsAdded,omitempty"`
	ReviewersAdded []string               `json:"reviewersAdded,omitempty"`
	// PlaneSpecReview marks the Plane-provider publish path: the tech spec was
	// written to a Plane page (node G) and is awaiting page-comment review (node H) —
	// there is no spec PR. Drives the completion summary + card wording.
	PlaneSpecReview bool `json:"planeSpecReview,omitempty"`
	// Grilled / Reviewed track node H's two mandatory agent gates (idempotent resume).
	Grilled  bool `json:"grilled,omitempty"`
	Reviewed bool `json:"reviewed,omitempty"`
}

type checkpointNotify struct {
	SentAt  string `json:"sentAt,omitempty"`
	Message string `json:"message,omitempty"`
}

func checkpointWriteSpecFromAgentResult(result AgentResult) *checkpointWriteSpec {
	return &checkpointWriteSpec{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Commits: append([]string(nil), result.Commits...), Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}
}

type resumedRunContext struct {
	Run        storage.RunRecord
	StartStep  PlannerStep
	Checkpoint plannerCheckpoint
	Resumed    bool
}

type stepInput struct {
	Project    storage.ProjectRecord
	Loop       storage.LoopRecord
	Run        storage.RunRecord
	QueueItem  storage.QueueItemRecord
	Checkpoint plannerCheckpoint
}

type loopError struct {
	message string
	kind    QueueFailureKind
}

func (e *loopError) Error() string { return e.message }

type transientFailure interface{ Temporary() bool }

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
		agentIdleTimeout = 10 * time.Minute
	}
	claimTTL := options.ClaimTTL
	if claimTTL <= 0 {
		claimTTL = defaultClaimTTL
	}
	retryBaseDelay := options.RetryBaseDelay
	if retryBaseDelay <= 0 {
		retryBaseDelay = defaultRetryDelay
	}
	retryMax := options.RetryMaxAttempts
	if retryMax == 0 {
		retryMax = defaultRetryMax
	}
	allowAutoPush := true
	if options.AllowAutoPush != nil {
		allowAutoPush = *options.AllowAutoPush
	}
	disclosureCfg := config.DefaultDisclosureConfig()
	if options.Disclosure != nil {
		disclosureCfg = *options.Disclosure
	}
	policy := options.DiscoveryPolicy
	if policy.LabelMode == "" {
		policy = DiscoveryPolicy{AutoDiscovery: true, Labels: []string{discoveryLabel}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: true}
	}
	return &Runner{db: options.DB, repos: options.Repos, github: options.GitHub, git: options.Git, agentExecutor: options.AgentExecutor, logger: options.Logger, now: now, agentTimeout: agentTimeout, agentIdleTimeout: agentIdleTimeout, claimTTL: claimTTL, allowAutoPush: allowAutoPush, disclosure: disclosureCfg, agentRuntime: strings.TrimSpace(options.AgentRuntime), customInstructions: customInstructionConfig(options.CustomInstructions), projectRoleConfig: options.CustomInstructions, agentModel: derefString(options.AgentModel), retryBaseDelay: retryBaseDelay, retryMaxAttempts: retryMax, onAgentExecutionStarted: options.OnAgentExecutionStarted, onQueueItemEnqueued: options.OnQueueItemEnqueued, postThreadNote: options.PostThreadNote, postThreadCard: options.PostThreadCard, discoveryPolicy: policy, planeDoc: options.PlaneDoc}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	if r.repos == nil || r.repos.Projects == nil || r.repos.Loops == nil || r.repos.Queue == nil || r.repos.Runs == nil {
		return DiscoveryResult{}, fmt.Errorf("planner repositories are not configured")
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
	if policy.RequireAssigneeCurrentUser {
		var err error
		login, err = r.github.GetCurrentUserLogin(ctx, project.RepoPath)
		if err != nil {
			return DiscoveryResult{}, err
		}
		login = normalizeLogin(login)
	}
	if policy.RequireAssigneeCurrentUser && login == "" {
		return DiscoveryResult{Skipped: 1}, nil
	}
	assigneeFilter := ""
	if policy.RequireAssigneeCurrentUser {
		assigneeFilter = login
	}
	issues, err := r.listOpenIssuesForDiscovery(ctx, ListOpenIssuesInput{Repo: input.Repo, CWD: project.RepoPath, Limit: input.Limit, Assignee: assigneeFilter}, policy)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	for _, issue := range issues {
		if !shouldClaimIssue(issue, login, policy) {
			result.Skipped++
			continue
		}
		fingerprint := buildPlannerDiscoveryFingerprint(input.Repo, r.now(), issue)
		loopResult, err := r.ensureLoopForIssue(ctx, *project, input.Repo, issue, fingerprint)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if loopResult.created {
			result.CreatedLoopIDs = append(result.CreatedLoopIDs, loopResult.record.ID)
		}
		if loopResult.record.Status == "paused" || loopResult.record.Status == "completed" || loopResult.record.Status == "awaiting_human" {
			result.Skipped++
			continue
		}
		// Anti-thrash: skip enqueue when ensureLoopForIssue left a previously
		// failed loop in place because its discovery inputs match the last
		// terminal failure.
		if loopResult.record.Status == "failed" {
			result.Skipped++
			continue
		}
		queueItem, err := r.enqueue(ctx, enqueueInput{ProjectID: project.ID, LoopID: loopResult.record.ID, Repo: input.Repo, IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "title": issue.Title, "body": issue.Body, "url": issue.URL, "assignees": issue.Assignees, "labels": issue.Labels, "currentUserLogin": login, plannerQueuePayloadFPKey: fingerprint}})
		if err != nil {
			return DiscoveryResult{}, err
		}
		result.QueueItems = append(result.QueueItems, queueItem)
	}
	return result, nil
}

func (r *Runner) discoveryPolicyForProject(projectID string) DiscoveryPolicy {
	if r.projectRoleConfig == nil {
		return r.discoveryPolicy
	}
	roles := config.ProjectRoleConfigs(*r.projectRoleConfig, projectID)
	return DiscoveryPolicy{AutoDiscovery: roles.Planner.AutoDiscovery, Labels: append([]string(nil), roles.Planner.Triggers.Labels...), LabelMode: roles.Planner.Triggers.LabelMode, RequireAssigneeCurrentUser: roles.Planner.Triggers.RequireAssigneeCurrentUser}
}

func (r *Runner) ProcessNext(ctx context.Context, claimedBy string) (*ProcessResult, error) {
	if r.repos == nil || r.repos.Queue == nil {
		return nil, fmt.Errorf("planner queue repository is not configured")
	}
	item, err := r.repos.Queue.ClaimNextOfType(ctx, r.nowISO(), claimedBy, "planner")
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, nil
	}
	return r.ProcessClaimedQueueItem(ctx, *item)
}

func (r *Runner) ProcessClaimedQueueItem(ctx context.Context, queueItem storage.QueueItemRecord) (*ProcessResult, error) {
	result, err := r.ProcessClaimedItem(ctx, queueItem)
	if err != nil {
		return r.recoverClaimedItem(ctx, queueItem, err)
	}
	return &result, nil
}

func (r *Runner) recoverClaimedItem(ctx context.Context, queueItem storage.QueueItemRecord, err error) (*ProcessResult, error) {
	failure := r.classifyFailure(err)
	failedQueue, failErr := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
	if failErr != nil {
		return nil, failErr
	}
	if err := r.reconcileRecoveredLoop(ctx, queueItem, failedQueue, failure.kind); err != nil {
		return nil, err
	}
	return &ProcessResult{LoopID: derefString(queueItem.LoopID), QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
}

func (r *Runner) reconcileRecoveredLoop(ctx context.Context, queueItem storage.QueueItemRecord, failedQueue *storage.QueueItemRecord, failureKind QueueFailureKind) error {
	if queueItem.LoopID == nil {
		return nil
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil {
		return err
	}
	if loop == nil || loop.Status != "running" {
		return nil
	}
	_, err = r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.LastRunAt = stringPtr(r.nowISO())
		if updated.Status == "paused" {
			updated.NextRunAt = nil
		} else if failedQueue != nil && failedQueue.Status == "queued" {
			updated.Status = "queued"
			updated.NextRunAt = stringPtr(failedQueue.AvailableAt)
		} else {
			updated.Status = "paused"
			r.stampFailedDiscoveryFingerprint(updated, queueItem)
			updated.NextRunAt = nil
		}
	})
	return err
}

func (r *Runner) ProcessClaimedItem(ctx context.Context, queueItem storage.QueueItemRecord) (ProcessResult, error) {
	if queueItem.Type != "planner" {
		return ProcessResult{}, fmt.Errorf("unsupported queue item type: %s", queueItem.Type)
	}
	if queueItem.LoopID == nil {
		return ProcessResult{}, fmt.Errorf("planner queue item requires loopId")
	}
	loop, err := r.repos.Loops.GetByID(ctx, *queueItem.LoopID)
	if err != nil {
		return ProcessResult{}, err
	}
	if loop == nil {
		return ProcessResult{}, fmt.Errorf("loop not found: %s", *queueItem.LoopID)
	}
	project, err := r.repos.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil {
		return ProcessResult{}, err
	}
	if project == nil {
		return ProcessResult{}, fmt.Errorf("project not found: %s", loop.ProjectID)
	}
	resumedRun, err := r.createRunContext(ctx, *loop)
	if err != nil {
		return ProcessResult{}, err
	}
	run := resumedRun.Run
	checkpoint := resumedRun.Checkpoint
	claimedLockKey := ""
	if resumedRun.StartStep != stepDiscoverIssues {
		claimedLockKey = checkpoint.ClaimedLockKey
	}
	acquiredClaimedLock := false
	if claimedLockKey != "" {
		reason := "planner-run-resume"
		nowISO := r.nowISO()
		acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: claimedLockKey, Owner: queueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
		if err != nil {
			return ProcessResult{}, err
		}
		if !acquired {
			return ProcessResult{}, &loopError{message: fmt.Sprintf("Issue lock is already held for %s", claimedLockKey), kind: FailureRetryableTransient}
		}
		acquiredClaimedLock = true
	}
	defer func() {
		if acquiredClaimedLock && claimedLockKey != "" {
			_ = r.repos.Locks.Release(context.Background(), claimedLockKey)
		}
	}()
	if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.Status = "running"
		updated.LastRunAt = stringPtr(run.StartedAt)
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"queueItemId": queueItem.ID, "resumed": resumedRun.Resumed, "startStep": string(resumedRun.StartStep)}})
	r.appendEvent(ctx, eventInput{eventType: "run.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"queueItemId": queueItem.ID, "currentStep": string(resumedRun.StartStep)}})

	for _, step := range stepsFrom(resumedRun.StartStep) {
		run, err = r.persistStepStarted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.started", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
		checkpoint, err = r.executeStep(ctx, step, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: queueItem, Checkpoint: checkpoint})
		if err != nil {
			failure := r.classifyFailureWithBoundary(err, plannerFailureBoundaryForStep(step))
			latest := r.getLatestCheckpoint(ctx, run, checkpoint)
			if failure.kind == FailureHITLInterrupt {
				// 强操控: a human's mid-run @bot killed the agent. Complete the run as
				// "interrupted" (NOT failed) and re-dispatch it IMMEDIATELY (no backoff,
				// attempts unchanged so it never hits the retry cap) so follow-up-resume
				// answers them from the MAIN write-spec session.
				latest.ResumePolicy = "advance_from_checkpoint"
				if _, err := r.completeRun(ctx, run, "interrupted", failure.message, "", latest); err != nil {
					return ProcessResult{}, err
				}
				r.appendEvent(ctx, eventInput{eventType: "run.interrupted", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"message": failure.message, "currentStep": derefString(run.CurrentStep)}})
				nowISO := r.nowISO()
				if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: nowISO, Attempts: queueItem.Attempts, ErrorMessage: optionalString(failure.message), ErrorKind: string(failure.kind), UpdatedAt: nowISO}); err != nil {
					return ProcessResult{}, err
				}
				if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
					updated.LastRunAt = stringPtr(nowISO)
					updated.Status = "queued"
					updated.NextRunAt = stringPtr(nowISO)
				}); err != nil {
					return ProcessResult{}, err
				}
				return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "interrupted", Summary: failure.message, FailureKind: failure.kind}, nil
			}
			latest.ResumePolicy = loops.NormalizeResumePolicy(string(failure.kind), latest.ResumePolicy)
			if _, err := r.completeRun(ctx, run, "failed", failure.message, failure.message, latest); err != nil {
				return ProcessResult{}, err
			}
			r.appendEvent(ctx, eventInput{eventType: "loop.step.failed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"message": failure.message, "failureKind": string(failure.kind), "currentStep": derefString(run.CurrentStep)}})
			r.appendEvent(ctx, eventInput{eventType: "run.failed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"summary": failure.message, "failureKind": string(failure.kind)}})
			failedQueue, err := r.failQueueItem(ctx, queueItem, failure.kind, failure.message)
			if err != nil {
				return ProcessResult{}, err
			}
			if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
				updated.LastRunAt = stringPtr(r.nowISO())
				if updated.Status == "paused" {
					updated.NextRunAt = nil
				} else if failedQueue != nil && failedQueue.Status == "queued" {
					updated.Status = "queued"
					updated.NextRunAt = stringPtr(failedQueue.AvailableAt)
				} else {
					updated.Status = "paused"
					r.stampFailedDiscoveryFingerprint(updated, queueItem)
					updated.NextRunAt = nil
				}
			}); err != nil {
				return ProcessResult{}, err
			}
			return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: "failed", Summary: failure.message, FailureKind: failure.kind}, nil
		}
		if step == stepDiscoverIssues {
			claimedLockKey = checkpoint.ClaimedLockKey
			acquiredClaimedLock = claimedLockKey != ""
		}
		run, err = r.persistStepCompleted(ctx, run, step, checkpoint)
		if err != nil {
			return ProcessResult{}, err
		}
		r.appendEvent(ctx, eventInput{eventType: "loop.step.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"step": string(step)}})
		if checkpoint.SkipReason != "" {
			break
		}
	}

	summary := checkpoint.SkipReason
	if summary == "" {
		issue := checkpoint.Issue
		if issue != nil {
			if checkpoint.Publish != nil && checkpoint.Publish.PlaneSpecReview {
				summary = fmt.Sprintf("Wrote tech spec to Plane for %s#%d — awaiting review (node H)", issue.Repo, issue.IssueNumber)
			} else {
				summary = fmt.Sprintf("Opened spec PR for %s#%d", issue.Repo, issue.IssueNumber)
			}
		} else {
			summary = "Completed planner run"
		}
	}
	if _, err := r.completeRun(ctx, run, "success", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "run.completed", projectID: loop.ProjectID, loopID: loop.ID, runID: run.ID, entityType: "run", entityID: run.ID, payload: map[string]any{"summary": summary}})
	status := "success"
	if checkpoint.SkipReason != "" {
		status = "skipped"
	}
	prNumber := int64(0)
	if checkpoint.Publish != nil && checkpoint.Publish.PullRequest != nil {
		prNumber = checkpoint.Publish.PullRequest.Number
	}
	if err := r.repos.Queue.Complete(ctx, queueItem.ID, r.nowISO()); err != nil {
		if errors.Is(err, storage.ErrQueueItemNotActive) {
			return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary, PullRequestNumber: prNumber}, nil
		}
		return ProcessResult{}, err
	}
	if _, err := r.updateLoop(ctx, *loop, func(updated *storage.LoopRecord) {
		updated.Status = "completed"
		updated.LastRunAt = stringPtr(r.nowISO())
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{LoopID: loop.ID, RunID: run.ID, QueueItemID: queueItem.ID, Status: status, Summary: summary, PullRequestNumber: prNumber}, nil
}

func (r *Runner) executeStep(ctx context.Context, step PlannerStep, input stepInput) (plannerCheckpoint, error) {
	switch step {
	case stepDiscoverIssues:
		return r.runDiscoverIssueStep(ctx, input)
	case stepPrepareWorktree:
		return r.runPrepareWorktreeStep(ctx, input)
	case stepWriteSpec:
		return r.runWriteSpecStep(ctx, input)
	case stepPublish:
		return r.runPublishStep(ctx, input)
	case stepGrill:
		return r.runGrillStep(ctx, input)
	case stepReview:
		return r.runReviewStep(ctx, input)
	case stepNotify:
		return r.runNotifyStep(input)
	default:
		return input.Checkpoint, fmt.Errorf("unsupported planner step: %s", step)
	}
}

func (r *Runner) runDiscoverIssueStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	payload := parseJSONObject(input.QueueItem.PayloadJSON)
	repo := firstNonEmpty(derefString(input.QueueItem.Repo), derefString(input.Loop.Repo), projectRepo(input.Project))
	issueNumber := int64FromAny(payload["issueNumber"])
	if issueNumber == 0 {
		issueNumber = parseIssueNumberFromTargetID(derefString(input.Loop.TargetID))
	}
	if repo == "" || issueNumber == 0 {
		return input.Checkpoint, &loopError{message: "Planner queue item requires repo and issue number", kind: FailureNonRetryable}
	}
	detail, err := r.github.ViewIssue(ctx, ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return input.Checkpoint, err
	}
	currentLogin := firstNonEmpty(normalizeLogin(stringFromAnyDefault(payload["currentUserLogin"])), input.CheckpointIssueLogin())
	lockKey := firstNonEmpty(derefString(input.QueueItem.LockKey), buildIssueLockKey(repo, issueNumber))
	nowISO := r.nowISO()
	reason := "planner-run"
	acquired, err := r.repos.Locks.Acquire(ctx, storage.LockRecord{Key: lockKey, Owner: input.QueueItem.ID, Reason: &reason, ExpiresAt: eventlog.FormatJavaScriptISOString(r.now().Add(r.claimTTL)), CreatedAt: nowISO, UpdatedAt: nowISO})
	if err != nil {
		return input.Checkpoint, err
	}
	if !acquired {
		return input.Checkpoint, &loopError{message: fmt.Sprintf("Issue lock is already held for %s", lockKey), kind: FailureRetryableTransient}
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = r.repos.Locks.Release(context.Background(), lockKey)
		}
	}()
	manual := isManualPlannerQueue(payload)
	policy := r.discoveryPolicyForProject(input.Project.ID)
	if currentLogin == "" && (manual || policy.RequireAssigneeCurrentUser || hasRequestedReviewerSources(input.Project, input.Loop, detail.Assignees)) {
		login, err := r.github.GetCurrentUserLogin(ctx, input.Project.RepoPath)
		if err != nil {
			return input.Checkpoint, &loopError{message: fmt.Sprintf("Unable to resolve GitHub login for planner issue %s#%d: %v", repo, issueNumber, err), kind: FailureRetryableAfterResume}
		}
		currentLogin = normalizeLogin(login)
		if currentLogin == "" {
			return input.Checkpoint, &loopError{message: fmt.Sprintf("Unable to resolve GitHub login for planner issue %s#%d", repo, issueNumber), kind: FailureRetryableAfterResume}
		}
	}
	if !manual && !labelsMatch(detail.Labels, policy.Labels, policy.LabelMode) {
		checkpoint := input.Checkpoint
		checkpoint.Issue = &checkpointIssue{Repo: repo, IssueNumber: issueNumber, Title: detail.Title, Body: detail.Body, URL: detail.URL, Assignees: cloneStrings(detail.Assignees), Labels: cloneStrings(detail.Labels), CurrentUserLogin: currentLogin, SpecPath: buildSpecPath(r.now(), issueNumber, detail.Title), RequestedReviewers: resolveRequestedReviewers(input.Project, input.Loop, detail.Assignees, currentLogin)}
		checkpoint.ClaimedLockKey = lockKey
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		checkpoint.SkipReason = fmt.Sprintf("Issue %s#%d no longer matches planner labels", repo, issueNumber)
		releaseOnError = false
		return checkpoint, nil
	}
	if !manual && policy.RequireAssigneeCurrentUser && currentLogin != "" && !includesLogin(detail.Assignees, currentLogin) {
		checkpoint := input.Checkpoint
		checkpoint.Issue = &checkpointIssue{Repo: repo, IssueNumber: issueNumber, Title: detail.Title, Body: detail.Body, URL: detail.URL, Assignees: cloneStrings(detail.Assignees), Labels: cloneStrings(detail.Labels), CurrentUserLogin: currentLogin, SpecPath: buildSpecPath(r.now(), issueNumber, detail.Title), RequestedReviewers: resolveRequestedReviewers(input.Project, input.Loop, detail.Assignees, currentLogin)}
		checkpoint.ClaimedLockKey = lockKey
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		checkpoint.SkipReason = fmt.Sprintf("Issue %s#%d is no longer assigned to %s", repo, issueNumber, currentLogin)
		releaseOnError = false
		return checkpoint, nil
	}
	if manual && currentLogin != "" && !includesLogin(detail.Assignees, currentLogin) {
		if err := r.github.AddIssueAssignees(ctx, IssueAssigneesInput{Repo: repo, IssueNumber: issueNumber, Assignees: []string{currentLogin}, CWD: input.Project.RepoPath}); err != nil {
			return input.Checkpoint, &loopError{message: fmt.Sprintf("Unable to assign issue %s#%d to %s: %v", repo, issueNumber, currentLogin, err), kind: FailureRetryableAfterResume}
		}
		detail.Assignees = appendUniqueStrings(detail.Assignees, currentLogin)
	}
	checkpoint := input.Checkpoint
	checkpoint.Issue = &checkpointIssue{Repo: repo, IssueNumber: issueNumber, Title: detail.Title, Body: detail.Body, URL: detail.URL, Assignees: cloneStrings(detail.Assignees), Labels: cloneStrings(detail.Labels), CurrentUserLogin: currentLogin, SpecPath: buildSpecPath(r.now(), issueNumber, detail.Title), RequestedReviewers: resolveRequestedReviewers(input.Project, input.Loop, detail.Assignees, currentLogin)}
	checkpoint.ClaimedLockKey = lockKey
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	checkpoint.SkipReason = ""
	releaseOnError = false
	return checkpoint, nil
}

func (input stepInput) CheckpointIssueLogin() string {
	if input.Checkpoint.Issue == nil {
		return ""
	}
	return input.Checkpoint.Issue.CurrentUserLogin
}

// productSpecGate implements flowchart node D/E for a Plane-provider feature: check
// whether the work item has a product spec; if not, ask product on the work item and
// return a hold (FailureManualIntervention) so the planner doesn't write a tech spec
// without one. A no-op for github/forgejo projects, non-features, or when the work
// item / spec status can't be resolved (fail open — don't wedge non-Plane flows).
func (r *Runner) productSpecGate(ctx context.Context, input stepInput, checkpoint plannerCheckpoint) *loopError {
	if r.planeDoc == nil || checkpoint.Issue == nil {
		return nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return nil
	}
	issue := checkpoint.Issue
	if !stringInSlice("kind/feature", issue.Labels) {
		return nil // bugs / refactors / perf don't need a product spec
	}
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return nil
	}
	present, _, err := gateway.HasProductSpec(ctx, planeProjectID, workItemID)
	if err != nil {
		return &loopError{message: fmt.Sprintf("check product spec on work item: %v", err), kind: FailureRetryableTransient}
	}
	if present {
		// A resumed planner passes here once product supplied the spec (node E2) —
		// clear the hold marker so the card leaves "⏸ 等待产品方案".
		r.setAwaitingProductSpecMarker(ctx, input.Loop, false)
		return nil // has a product spec → proceed to write the tech spec
	}
	if err := gateway.RequestProductSpec(ctx, planeProjectID, workItemID, "产品负责人", issue.Title); err != nil && r.logger != nil {
		r.logger.Warn("planner: request product spec failed", map[string]any{"projectId": input.Project.ID, "workItem": workItemID, "error": err.Error()})
	}
	// Also @-mention the product owner in the Feishu thread — not only the Plane
	// comment — so they see the ask where they actually watch. The open_id comes from
	// per-project config (ProjectProductOwner), never hardcoded. Best-effort.
	r.requestProductSpecInThread(ctx, input, issue.Title)
	// Mark the hold reason so the anchor card reads "⏸ 等待产品方案" (node E) rather
	// than the generic "⏸ 等你定夺" — the header alone tells you what's blocking.
	r.setAwaitingProductSpecMarker(ctx, input.Loop, true)
	return &loopError{message: "awaiting product spec — asked product to supply one on the work item", kind: FailureManualIntervention}
}

// requestProductSpecInThread @-mentions the project's product owner in the loop's
// Feishu thread to ask for a missing product spec (node E) — the thread mirror of the
// Plane comment RequestProductSpec posts, so the ask reaches them where they watch.
// The open_id is resolved per-project from config (never hardcoded); a missing
// transport or an unset owner just skips the ping. Best-effort.
func (r *Runner) requestProductSpecInThread(ctx context.Context, input stepInput, workItemTitle string) {
	if r.postThreadNote == nil || r.projectRoleConfig == nil {
		return
	}
	var mentions []string
	if openID := strings.TrimSpace(config.ProjectProductOwner(*r.projectRoleConfig, input.Project.ID).FeishuOpenID); openID != "" {
		mentions = []string{openID}
	}
	note := fmt.Sprintf("⏸ 需求「%s」还没有产品 spec —— 请补一份:把方案页链接或正文直接发在本 thread,looper 会自动关联并继续。", strings.TrimSpace(workItemTitle))
	if err := r.postThreadNote(ctx, input.Loop.ID, note, mentions); err != nil && r.logger != nil {
		r.logger.Warn("planner: post product-spec request note failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
	}
}

// setAwaitingProductSpecMarker records (or clears) the "awaiting product spec" hold
// flag in the loop metadata, so the anchor card can distinguish node E's product-spec
// wait from a generic HITL ask. Best-effort + guarded: a Runner without a loops repo
// (e.g. a unit/live test wiring only planeDoc) silently skips it.
func (r *Runner) setAwaitingProductSpecMarker(ctx context.Context, loop storage.LoopRecord, waiting bool) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"awaitingProductSpec": waiting})
	if err != nil {
		return
	}
	if waiting {
		metadataJSON, err = loopcondition.Set(&metadataJSON, loopcondition.Record{Kind: loopcondition.ProductSpec, Since: r.nowISO()})
	} else {
		metadataJSON, err = loopcondition.Clear(&metadataJSON)
	}
	if err != nil {
		return
	}
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil && r.logger != nil {
		r.logger.Warn("planner: mark awaiting product spec failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
	}
}

// requestProductDecisionInThread posts the agent's product-language question
// (productAsk, node H) into the loop's Feishu thread, @-mentioning the project's
// product owner so they see it where they watch. The message is already written in
// product language by the agent; this only frames + @-tags it. The open_id is
// resolved per-project from config (never hardcoded); a missing transport just skips
// and an unset owner posts without a mention. Best-effort.
func (r *Runner) requestProductDecisionInThread(ctx context.Context, input stepInput, productAsk string) {
	if r.postThreadCard == nil && r.postThreadNote == nil {
		return
	}
	var mentions []string
	if r.projectRoleConfig != nil {
		if openID := strings.TrimSpace(config.ProjectProductOwner(*r.projectRoleConfig, input.Project.ID).FeishuOpenID); openID != "" {
			mentions = []string{openID}
		}
	}
	// Prefer a header-less lark_md card so the product-language body keeps its structure
	// (bold sub-headers, blank lines, one option per line); a plain "text" message would
	// render none of it and collapse to a wall. Fall back to a text note only when the
	// card path isn't wired (e.g. tests).
	if r.postThreadCard != nil {
		if err := r.postThreadCard(ctx, input.Loop.ID, strings.TrimSpace(productAsk), mentions); err != nil && r.logger != nil {
			r.logger.Warn("planner: post product-decision card failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
		return
	}
	note := "🙋 这个需求有个地方需要你来拍板 —— 直接在本 thread 回复你的选择就行,我会据此更新方案:\n\n" + strings.TrimSpace(productAsk)
	if err := r.postThreadNote(ctx, input.Loop.ID, note, mentions); err != nil && r.logger != nil {
		r.logger.Warn("planner: post product-decision request note failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
	}
}

// setAwaitingProductAnswerMarker records (or clears) the "awaiting product answer"
// hold flag in the loop metadata (node H product-decision gate), distinct from the
// node-E "awaiting product spec" wait and the owner "awaiting spec approval" gate.
// Best-effort + guarded like setAwaitingProductSpecMarker.
func (r *Runner) setAwaitingProductAnswerMarker(ctx context.Context, loop storage.LoopRecord, waiting bool) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"awaitingProductAnswer": waiting})
	if err != nil {
		return
	}
	if _, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) }); err != nil && r.logger != nil {
		r.logger.Warn("planner: mark awaiting product answer failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
	}
}

func (r *Runner) runPrepareWorktreeStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	// Flowchart node D/E: on a Plane project, a feature can't be planned until it has
	// a product spec. If missing, ask product on the work item and hold (no worktree,
	// no tech spec) until it's supplied.
	if gateErr := r.productSpecGate(ctx, input, checkpoint); gateErr != nil {
		return checkpoint, gateErr
	}
	projectMetadata := parseJSONObject(input.Project.MetadataJSON)
	worktreeRoot := stringFromAnyDefault(projectMetadata["worktreeRoot"])
	var err error
	if worktreeRoot == "" {
		worktreeRoot, err = config.DefaultProjectWorktreeRoot(input.Project.ID, input.Project.RepoPath)
		if err != nil {
			return checkpoint, err
		}
	}
	if checkpoint.Worktree != nil {
		if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: checkpoint.Worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err == nil {
			return checkpoint, nil
		}
		checkpoint.Worktree = nil
		checkpoint.ResumePolicy = "advance_from_checkpoint"
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	baseBranch := firstNonEmpty(derefString(input.Project.BaseBranch), "main")
	branch := buildPlannerBranch(issue.IssueNumber, issue.Title)
	created, err := r.git.CreateWorktree(ctx, CreateWorktreeInput{ProjectID: input.Project.ID, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: baseBranch, ProtectedBranches: []string{baseBranch}})
	if err != nil {
		return checkpoint, err
	}
	checkpoint.Worktree = &checkpointWorktree{ID: created.ID, Path: created.WorktreePath, Branch: created.Branch, BaseBranch: firstNonEmpty(created.BaseBranch, baseBranch), SpecPath: issue.SpecPath}
	checkpoint.Lifecycle = lifecycle.NewState(lifecycle.AgentManagedWithFallbackPolicy("planner", true), created.Branch, firstNonEmpty(created.BaseBranch, baseBranch))
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// plannerProductAskInstruction tells the planner agent to escalate — in product
// language — the open questions in a spec that only the product owner can decide,
// via a `productAsk` field in its completion marker. Technical open questions are
// the agent's own to decide and must NOT be escalated. Kept Plane-node-H-scoped
// (added only for Plane projects) since that is where a human product owner reviews.
const plannerProductAskInstruction = `PRODUCT DECISION ESCALATION (only when the spec has a real product question):
As you write the spec, sort every open question into PRODUCT vs TECHNICAL.
- TECHNICAL (which library, how to structure the code, a data shape, error handling, an internal name, anything a competent engineer can just pick): DECIDE it yourself, note it in the spec, and do NOT escalate.
- PRODUCT: a decision only the product owner can make — it changes what the user sees or can do, the product's scope/behavior, or hinges on business intent, a user-facing tradeoff, or an unstated requirement.

ONLY when at least one real PRODUCT decision genuinely needs the product owner to choose, ALSO emit a "productAsk" field in your final __LOOPER_RESULT__ line: ONE message written FOR the product owner, in the product owner's own language (match the language of the issue/thread).

FORMATTING — this message is shown as a Feishu card, so it MUST be structured, never one long paragraph. Use lark-markdown: **bold** for every sub-header and for the recommended option; a BLANK LINE between sections; and put each option on ITS OWN line. Do NOT use "#" headers (use **bold** instead). Lay it out EXACTLY like this (keep the bold sub-headers verbatim):

**背景**
<one or two sentences: which user/customer asked for what, and why it matters>

**现状**
<what is already decided or known, so they don't re-litigate it — one short paragraph>

**需要你拍板**
<for EACH product decision, a block shaped like:>
**问题一:<the question in plain words>**
- 选项A:<plain-language option>
- 选项B:<plain-language option>
- 建议:**<your pick>** —— <one short line of why>
<blank line, then 问题二 the same way if there is a second decision>

Hard rules for productAsk:
  - Write like a product manager briefing a business stakeholder, NOT like an engineer. Someone who cannot read code must be able to decide from it alone.
  - NO engineering jargon of ANY kind: no API/endpoint/route, component/module/package/library names, field/column/schema names, CSS/z-index, file paths, or function names. Translate every technical thing into what the user actually experiences. (e.g. "read from the brand endpoint" → "where a brand's info is pulled from"; "the design-system package" → "the brand kit they imported".)
  - Put everything needed to decide INSIDE the message. Do NOT include a spec link or tell them to go read the spec.
  - If there is NO product decision (only technical questions, which you resolved yourself), set productAsk to "" — never invent one.

The productAsk value is a JSON string, so encode the line breaks as \n (a blank line between sections is \n\n). Your final marker line carries the extra field:
__LOOPER_RESULT__={"summary":"<one-sentence summary>","productAsk":"<the product-language message with \n line breaks, or empty>"}`

func (r *Runner) runWriteSpecStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	writeSpecCompleted := checkpoint.WriteSpec != nil && strings.EqualFold(checkpoint.WriteSpec.Status, "completed")
	productAsk := ""
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktreeRoot, rootErr := plannerWorktreeRoot(input.Project)
	if rootErr != nil {
		return checkpoint, rootErr
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
		checkpoint.Worktree = nil
		if checkpoint.WriteSpec != nil && !checkpoint.WriteSpec.GitReconciled {
			checkpoint.WriteSpec = nil
			writeSpecCompleted = false
		}
		checkpoint.ResumePolicy = "advance_from_checkpoint"
		input.Checkpoint = checkpoint
		checkpoint, err = r.runPrepareWorktreeStep(ctx, input)
		if err != nil {
			return checkpoint, err
		}
		worktree, err = requireWorktree(checkpoint)
		if err != nil {
			return checkpoint, err
		}
		if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: worktree.Path, RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot}); err != nil {
			return checkpoint, err
		}
	}
	if writeSpecCompleted && checkpoint.WriteSpec.GitReconciled {
		return checkpoint, nil
	}
	if !writeSpecCompleted {
		executionID := eventlog.NewEventID("agent")
		prompt, instructionBlock := buildPlannerPrompt(input.Project, r.customInstructions, issue, worktree, r.allowAutoPush, r.disclosure, r.agentRuntime, r.agentModel)
		if r.isPlaneProject(input.Project.ID) {
			// On a Plane project the spec lives on a Plane page, never a PR. Some agents
			// still reflexively run `gh pr create`; forbid it explicitly (looper closes a
			// stray one anyway, but the ask avoids the noise).
			prompt += "\n\nCRITICAL — This is a Plane project: the spec will be published to a Plane page by looper, NOT as a pull request. Write ONLY the spec file in the worktree. Do NOT run `gh pr create`, do NOT `git push`, do NOT open or modify any pull request. Any PR you open will be closed as a mistake."
			// Node H: let the agent escalate genuine product decisions to the product
			// owner in product language (via the productAsk marker field).
			prompt += "\n\n" + plannerProductAskInstruction
		}
		// Follow-up resume (线程永远可追问): drain any human thread messages queued while
		// the spec awaited review, and native-resume the SAME planner session so the
		// agent keeps its full spec-authoring history and revises the spec incrementally
		// (incorporating a product decision / answering a follow-up) — never replanning
		// from scratch. On a fresh first run the inbox is empty, so this is a no-op and
		// the agent starts normally.
		nativeResumePrompt := ""
		nativeSessionID := ""
		var drainedInbox []loops.HumanMessage
		if inbox := loops.ReadHumanInbox(input.Loop.MetadataJSON); len(inbox) > 0 {
			drainedInbox = inbox
			var msgs strings.Builder
			msgs.WriteString("While this spec was awaiting review, the human sent these messages in the task thread:")
			for _, m := range inbox {
				if t := strings.TrimSpace(m.Text); t != "" {
					msgs.WriteString("\n- ")
					msgs.WriteString(t)
				}
			}
			msgs.WriteString("\nRead them in context and update the spec accordingly — incorporate a product decision, answer a follow-up, or tighten an open question. Do NOT rewrite the spec from scratch: revise the existing spec file incrementally to reflect their input.")
			msgs.WriteString("\n\nThen DECIDE whether you can proceed or still need them, and LEAN TOWARD ASKING rather than silently continuing (强操控 — the human is driving). Unless they clearly gave the go-ahead (e.g. \"继续\" / \"go ahead\", or they answered the pending decision), put your reply to them in the productAsk field: briefly state how you understood/answered them, then explicitly ask \"可以继续吗?\" so they know the ball is in their court. In THIS follow-up turn the productAsk IS your conversational reply to them (an answer + a can-I-continue check, or the surfaced decision) — not necessarily a formal product decision, so the usual \"no decision → empty\" rule does not apply here. Only when they have clearly told you to proceed, set productAsk to \"\" so the spec moves on to review.")
			nativeResumePrompt = msgs.String()
			prompt += "\n\n" + nativeResumePrompt
			nativeSessionID = r.latestNativeSessionID(ctx, input.Loop.ID)
		}
		metadata := map[string]any{"loopType": "planner", "repo": issue.Repo, "issueNumber": issue.IssueNumber, "specPath": issue.SpecPath}
		for key, value := range config.CustomInstructionMetadata(instructionBlock, prompt) {
			metadata[key] = value
		}
		execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, NativeResumePrompt: nativeResumePrompt, NativeSessionID: nativeSessionID, WorkingDirectory: worktree.Path, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout, Metadata: metadata, IdempotencyKey: fmt.Sprintf("planner:%s", input.Loop.ID)})
		if err != nil {
			return checkpoint, err
		}
		if r.onAgentExecutionStarted != nil {
			if err := r.onAgentExecutionStarted(ctx, AgentExecutionStartedInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Subtitle: fmt.Sprintf("%s#%d", issue.Repo, issue.IssueNumber), Body: fmt.Sprintf("Planner started for %s", issue.Title), DedupeKey: "runtime.agent.started:planner:" + input.Run.ID}); err != nil && r.logger != nil {
				r.logger.Warn("planner agent start notification failed", map[string]any{"loopId": input.Loop.ID, "runId": input.Run.ID, "error": err.Error()})
			}
		}
		result, err := execution.Wait(ctx)
		if err != nil {
			return checkpoint, err
		}
		if !strings.EqualFold(result.Status, "completed") {
			if result.Interrupted {
				// A human's mid-run @bot killed this agent (强操控) — not a failure. Return
				// the interrupt signal so the run completes "interrupted" and re-dispatches
				// to answer them from the MAIN session (issue/worktree preserved).
				return checkpoint, plannerInterruptError()
			}
			checkpoint.WriteSpec = checkpointWriteSpecFromAgentResult(result)
			checkpoint.ResumePolicy = "retry_from_timeout_context"
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepWriteSpec, checkpoint); err != nil {
				return checkpoint, wrapRetryableAfterResume(err)
			}
			message := firstNonEmpty(result.Summary, result.Stderr, "Planner agent "+result.Status)
			kind := FailureRetryableTransient
			if agent.IsAgentSetupFailureMessage(message) {
				kind = FailureRetryableTransient
			}
			return checkpoint, &loopError{message: message, kind: kind}
		}
		checkpoint.WriteSpec = checkpointWriteSpecFromAgentResult(result)
		productAsk = strings.TrimSpace(result.ProductAsk)
		// Clear ONLY the messages this turn actually consumed (fed above). A message that
		// arrived mid-run — after the inbox was read — was never seen by the agent, so it
		// stays queued for a follow-up turn instead of being silently eaten here.
		r.clearHumanInbox(ctx, &input.Loop, drainedInbox)
		checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
		if result.Lifecycle != nil {
			checkpoint.Lifecycle.MergeAgent(result.Lifecycle, r.nowISO())
		} else if len(result.Commits) > 0 {
			checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, result.Commits...)
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceAgent
		}
	}
	checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
	if err := r.persistCheckpoint(ctx, input.Run.ID, stepWriteSpec, checkpoint); err != nil {
		return checkpoint, wrapRetryableAfterResume(err)
	}
	if r.git != nil {
		inspect, err := r.git.InspectHead(ctx, InspectHeadInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, BaseRef: worktree.BaseBranch})
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if inspect.HasUncommittedChanges {
			committed, err := r.git.Commit(ctx, CommitInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Message: buildPlannerFallbackCommitMessage(issue)})
			if err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			if committed.CommitSHA != "" {
				checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, committed.CommitSHA)
			}
			checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceFallback
		} else if len(inspect.NewCommitSHAs) > 0 {
			checkpoint.Lifecycle.CommitSHAs = appendUniqueStrings(checkpoint.Lifecycle.CommitSHAs, inspect.NewCommitSHAs...)
			if checkpoint.Lifecycle.Actions.Commit == lifecycle.ActionSourceNone {
				checkpoint.Lifecycle.Actions.Commit = lifecycle.ActionSourceAgent
			}
		}
	}
	checkpoint.WriteSpec.GitReconciled = true
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	// Node H product-decision hold: the spec turn surfaced a real product question
	// only the owner can answer. Ask them in the thread (product language) and HOLD
	// before the owner-approval gate — publish/grill/review don't run yet. The run
	// still completes (skipReason), so the loop reaches "completed" and a thread reply
	// reactivates it via the follow-up resume, which re-runs write-spec with the
	// answer; once no product question remains the flow proceeds to owner approval. On
	// a fresh first run or a resume that RESOLVED the question, productAsk is empty and
	// any prior hold marker is cleared so the loop advances normally.
	if productAsk != "" && r.isPlaneProject(input.Project.ID) {
		r.requestProductDecisionInThread(ctx, input, productAsk)
		r.setAwaitingProductAnswerMarker(ctx, input.Loop, true)
		r.setNodeHPhase(ctx, input.Loop.ID, "awaiting_product_answer")
		checkpoint.SkipReason = "awaiting product decision — asked the product owner in the thread"
		return checkpoint, nil
	}
	r.setAwaitingProductAnswerMarker(ctx, input.Loop, false)
	return checkpoint, nil
}

func (r *Runner) runPublishStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	// Plane provider: the tech spec lives on a Plane page (nodes G/H) and is reviewed
	// there via page comments — there is NO GitHub spec PR, and nothing is pushed. Take
	// the Plane-only publish path before the push/PR machinery below.
	if r.planeDoc != nil {
		if gw, planeProjectID, ok := r.planeDoc(input.Project.ID); ok && gw != nil {
			return r.runPlanePublishStep(ctx, input, gw, planeProjectID)
		}
	}
	if !r.allowAutoPush {
		message := fmt.Sprintf("Auto push disabled; manual publish required for planner %s", input.Loop.ID)
		checkpoint.SkipReason = message
		checkpoint.ResumePolicy = loops.ResumePolicyManualIntervention
		return checkpoint, &loopError{message: message, kind: FailureManualIntervention}
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	if checkpoint.Publish.LabelsAdded == nil {
		checkpoint.Publish.LabelsAdded = []string{}
	}
	if checkpoint.Publish.ReviewersAdded == nil {
		checkpoint.Publish.ReviewersAdded = []string{}
	}
	if !checkpoint.Publish.Pushed {
		worktreeRoot, rootErr := plannerWorktreeRoot(input.Project)
		if rootErr != nil {
			return checkpoint, rootErr
		}
		if err := r.git.Push(ctx, PushInput{RepoPath: input.Project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.Path, Branch: worktree.Branch, ProtectedBranches: []string{worktree.BaseBranch}}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.Pushed = true
		checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
		checkpoint.Lifecycle.Actions.Push = lifecycle.ActionSourceFallback
		checkpoint.Lifecycle.Pushed = true
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	if checkpoint.Publish.PullRequest == nil {
		if checkpoint.Lifecycle != nil && checkpoint.Lifecycle.PRNumber > 0 {
			adopted, err := r.validatedLifecyclePullRequest(ctx, input, *issue, *worktree, checkpoint.Lifecycle)
			if err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			if adopted != nil {
				if err := r.normalizePullRequestDisclosure(ctx, issue.Repo, adopted.Number, input.Project.RepoPath, true); err != nil {
					return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
				}
				checkpoint.Publish.PullRequest = adopted
				checkpoint.Lifecycle.PRNumber = adopted.Number
				checkpoint.Lifecycle.PRURL = adopted.URL
				checkpoint.Lifecycle.PRAdopted = true
				checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceAgent
				if err := r.persistPlannerPullRequestReference(ctx, input, *issue, *worktree, *adopted); err != nil {
					return checkpoint, wrapRetryableAfterResume(err)
				}
				if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
					return checkpoint, wrapRetryableAfterResume(err)
				}
			} else {
				checkpoint.Lifecycle.PRNumber = 0
				checkpoint.Lifecycle.PRURL = ""
				checkpoint.Lifecycle.PRAdopted = false
				checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceNone
			}
		}
	}
	if checkpoint.Publish.PullRequest == nil {
		adopted, err := r.findOpenPullRequestForBranch(ctx, issue.Repo, worktree.Branch, worktree.BaseBranch, input.Project.RepoPath)
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if adopted != nil {
			if err := r.normalizePullRequestDisclosure(ctx, issue.Repo, adopted.Number, input.Project.RepoPath, false); err != nil {
				return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
			}
			checkpoint.Publish.PullRequest = &checkpointPullRequest{Number: adopted.Number, URL: adopted.URL, Body: ""}
			checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
			checkpoint.Lifecycle.PRNumber = adopted.Number
			checkpoint.Lifecycle.PRURL = adopted.URL
			checkpoint.Lifecycle.PRAdopted = true
			checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceAgent
			if err := r.persistPlannerPullRequestReference(ctx, input, *issue, *worktree, *checkpoint.Publish.PullRequest); err != nil {
				return checkpoint, wrapRetryableAfterResume(err)
			}
			if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
				return checkpoint, wrapRetryableAfterResume(err)
			}
		}
	}
	if checkpoint.Publish.PullRequest == nil {
		body := buildPullRequestBody(*issue, *worktree, checkpoint.WriteSpec)
		pr, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{Repo: issue.Repo, HeadBranch: worktree.Branch, BaseBranch: worktree.BaseBranch, Title: "Spec: " + issue.Title, Body: body, CWD: input.Project.RepoPath})
		if err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		if pr.Number == 0 {
			return checkpoint, &loopError{message: "Planner publish requires a pull request number", kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.PullRequest = &checkpointPullRequest{Number: pr.Number, URL: pr.URL, Body: body}
		checkpoint.ensureLifecycle("planner", worktree.Branch, worktree.BaseBranch, true)
		checkpoint.Lifecycle.PRNumber = pr.Number
		checkpoint.Lifecycle.PRURL = pr.URL
		checkpoint.Lifecycle.Actions.PR = lifecycle.ActionSourceFallback
		if err := r.persistPlannerPullRequestReference(ctx, input, *issue, *worktree, checkpointPullRequest{Number: pr.Number, URL: pr.URL, Body: body}); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	pr := checkpoint.Publish.PullRequest
	if pr == nil || pr.Number == 0 {
		return checkpoint, &loopError{message: "Planner publish requires a pull request number", kind: FailureRetryableAfterResume}
	}
	// Flowchart node G: publish the tech spec to Plane as soon as the PR exists, before
	// the label/reviewer steps (so it lands even if those retry). Best-effort + idempotent.
	if err := r.publishTechSpecToPlane(ctx, input, *issue, *worktree); err != nil && r.logger != nil {
		r.logger.Warn("planner: publish tech spec to Plane failed", map[string]any{"projectId": input.Project.ID, "error": err.Error()})
	}
	if !stringInSlice(specpr.ReviewingLabel, checkpoint.Publish.LabelsAdded) {
		if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: issue.Repo, PRNumber: pr.Number, Labels: []string{specpr.ReviewingLabel}, CWD: input.Project.RepoPath}); err != nil {
			return checkpoint, &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
		}
		checkpoint.Publish.LabelsAdded = append(checkpoint.Publish.LabelsAdded, specpr.ReviewingLabel)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	pendingReviewers := make([]string, 0)
	for _, reviewer := range issue.RequestedReviewers {
		// A Plane assignee is a UUID, not a GitHub login — never request it as a
		// GitHub reviewer (it would 422). Skip UUID-shaped values.
		if looksLikeUUID(reviewer) {
			continue
		}
		if !stringInSlice(reviewer, checkpoint.Publish.ReviewersAdded) {
			pendingReviewers = append(pendingReviewers, reviewer)
		}
	}
	if len(pendingReviewers) > 0 {
		// Best-effort: requesting a reviewer can legitimately fail (e.g. a Plane
		// assignee is a UUID, not a GitHub collaborator) and must NOT wedge the spec
		// PR by retrying forever. Log and move on — the PR is opened and reviewable.
		if err := r.github.AddPullRequestReviewers(ctx, PullRequestReviewersInput{Repo: issue.Repo, PRNumber: pr.Number, Reviewers: pendingReviewers, CWD: input.Project.RepoPath}); err != nil {
			if r.logger != nil {
				r.logger.Warn("planner: request reviewers failed (continuing)", map[string]any{"repo": issue.Repo, "pr": pr.Number, "reviewers": pendingReviewers, "error": err.Error()})
			}
		}
		checkpoint.Publish.ReviewersAdded = append(checkpoint.Publish.ReviewersAdded, pendingReviewers...)
		if err := r.persistCheckpoint(ctx, input.Run.ID, stepPublish, checkpoint); err != nil {
			return checkpoint, wrapRetryableAfterResume(err)
		}
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// runGrillStep is flowchart node H's first mandatory gate (Plane-only): a fresh
// adversarial agent interrogates the tech spec on its Plane page, reads the real
// source behind any claim, and REVISES the page to converge it — anything it genuinely
// can't decide is left as an open question for a human. No-op for GitHub/forgejo
// (their spec is the PR, reviewed there) and idempotent on resume.
func (r *Runner) runGrillStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if r.planeDoc == nil {
		return checkpoint, nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return checkpoint, nil
	}
	if checkpoint.Publish != nil && checkpoint.Publish.Grilled {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, planedoc.WorkItemIDFromURL(issue.URL), planedoc.TechSpecLinkTitle)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("grill: find spec link: %v", err), kind: FailureRetryableTransient}
	}
	if !found {
		return checkpoint, nil // nothing to grill
	}
	r.setNodeHPhase(ctx, input.Loop.ID, "grilling")
	specPath := firstNonEmpty(worktree.SpecPath, issue.SpecPath)
	prompt := buildGrillPrompt(*issue, specPath)
	result, err := r.runPlannerAgent(ctx, input, worktree.Path, prompt, "grill")
	if err != nil {
		return checkpoint, err
	}
	if !strings.EqualFold(result.Status, "completed") {
		if result.Interrupted {
			return checkpoint, plannerInterruptError() // 强操控: answer the human from the main session
		}
		checkpoint.ResumePolicy = "retry_from_timeout_context"
		return checkpoint, &loopError{message: "grill agent did not complete", kind: FailureRetryableAfterResume}
	}
	// The grill revised the spec FILE in the worktree (sandbox-safe); re-publish it to
	// the Plane page so the page reflects the converged spec.
	if revised, rErr := readPlannerSpecFile(worktree.Path, specPath); rErr == nil && strings.TrimSpace(revised) != "" {
		if err := gateway.UpdatePageContent(ctx, planeProjectID, planedoc.PageIDFromURL(specURL), revised); err != nil && r.logger != nil {
			r.logger.Warn("grill: re-publish revised spec to Plane failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
	}
	// Record the grill transcript on the spec page (signed) so the challenge/fix trail
	// sits next to the spec for the human reviewer.
	if summary := cleanAgentSummary(result.Summary); summary != "" {
		body := planedoc.SignComment("<p>🔬 <b>GRILL 拷问结论</b></p><p>"+htmlEscape(truncateRunes(summary, 1500))+"</p>", "grill", r.agentModel)
		if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, body); err != nil && r.logger != nil {
			r.logger.Warn("grill: post transcript failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
		// node H condition #2: surface the grill transcript in the Feishu thread too.
		r.postNodeHThreadNote(ctx, input, "grillTranscript", "🔬 GRILL 拷问结论:\n"+truncateRunes(summary, 1200), false)
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	checkpoint.Publish.Grilled = true
	r.setNodeHPhase(ctx, input.Loop.ID, "reviewing")
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// runReviewStep is node H's second mandatory gate (Plane-only): an INDEPENDENT reviewer
// agent (a different pass from the grill) re-reviews the converged spec and posts a
// verdict, then opens the human-approve gate — marks the card 需要人类审核 spec and lets
// the reconcile poll the page for a human's approve. Idempotent; no-op for non-Plane.
func (r *Runner) runReviewStep(ctx context.Context, input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" {
		return checkpoint, nil
	}
	if r.planeDoc == nil {
		return checkpoint, nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return checkpoint, nil
	}
	if checkpoint.Publish != nil && checkpoint.Publish.Reviewed {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, planedoc.WorkItemIDFromURL(issue.URL), planedoc.TechSpecLinkTitle)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("review: find spec link: %v", err), kind: FailureRetryableTransient}
	}
	if !found {
		return checkpoint, nil
	}
	r.setNodeHPhase(ctx, input.Loop.ID, "reviewing")
	prompt := buildReviewPrompt(*issue, firstNonEmpty(worktree.SpecPath, issue.SpecPath))
	result, err := r.runPlannerAgent(ctx, input, worktree.Path, prompt, "review")
	if err != nil {
		return checkpoint, err
	}
	if !strings.EqualFold(result.Status, "completed") {
		if result.Interrupted {
			return checkpoint, plannerInterruptError() // 强操控: answer the human from the main session
		}
		checkpoint.ResumePolicy = "retry_from_timeout_context"
		return checkpoint, &loopError{message: "review agent did not complete", kind: FailureRetryableAfterResume}
	}
	if summary := cleanAgentSummary(result.Summary); summary != "" {
		body := planedoc.SignComment("<p>👀 <b>REVIEW 复核结论</b></p><p>"+htmlEscape(truncateRunes(summary, 1500))+"</p><p>方案已过 grill + review。请人过目,无异议在本页评论 <b>approve / 同意 / 👍</b> 即进入实现。</p>", "reviewer", r.agentModel)
		if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, body); err != nil && r.logger != nil {
			r.logger.Warn("review: post verdict failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	checkpoint.Publish.Reviewed = true
	// node H converged → open the human-approve gate and @-mention the owner in the
	// thread ONCE: review is done, please approve. This review-end ping IS the
	// notification — no timer / periodic nudge.
	r.setNodeHPhase(ctx, input.Loop.ID, "awaiting_human_review")
	// Retire the planner trigger so the planner won't re-discover this item while it
	// awaits approval — the label lifecycle is plan → (approval) → worker-ready.
	if workItemID := planedoc.WorkItemIDFromURL(issue.URL); workItemID != "" {
		if err := gateway.RemoveWorkItemLabel(ctx, planeProjectID, workItemID, discoveryLabel); err != nil && r.logger != nil {
			r.logger.Warn("review: retire looper:plan failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
	}
	r.postNodeHThreadNote(ctx, input, "approvalRequest", "🙋 方案已过 GRILL 拷问 + 独立 REVIEW,请你审批 —— 无异议在方案页评论 approve / 同意 / 👍 即进入实现:"+specURL, true)
	if r.repos != nil && r.repos.Loops != nil {
		if loop, gErr := r.repos.Loops.GetByID(ctx, input.Loop.ID); gErr == nil && loop != nil {
			if metadataJSON, mErr := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"awaitingSpecApproval": true}); mErr == nil {
				_, _ = r.updateLoop(ctx, *loop, func(u *storage.LoopRecord) { u.MetadataJSON = stringPtr(metadataJSON) })
			}
		}
	}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// runPlannerAgent runs one bounded agent pass in the worktree and returns its result —
// the shared execution path for the grill + review node H gates.
func (r *Runner) runPlannerAgent(ctx context.Context, input stepInput, worktreePath, prompt, phase string) (AgentResult, error) {
	if r.agentExecutor == nil {
		return AgentResult{}, fmt.Errorf("planner agent executor not configured")
	}
	executionID := eventlog.NewEventID("agent")
	metadata := map[string]any{"loopType": "planner", "phase": phase}
	execution, err := r.agentExecutor.Start(ctx, AgentRunInput{ExecutionID: executionID, ProjectID: input.Project.ID, LoopID: input.Loop.ID, RunID: input.Run.ID, Prompt: prompt, WorkingDirectory: worktreePath, Timeout: r.agentTimeout, HeartbeatTimeout: r.agentIdleTimeout, Metadata: metadata, IdempotencyKey: fmt.Sprintf("planner:%s:%s", phase, input.Loop.ID)})
	if err != nil {
		return AgentResult{}, err
	}
	return execution.Wait(ctx)
}

// buildGrillPrompt is the fresh adversarial reviewer's brief (node H GRILL). It revises
// the spec FILE in the worktree — never Plane or anything outside the project (the codex
// sandbox blocks external writes); looper re-publishes the revised file to Plane after.
func buildGrillPrompt(issue checkpointIssue, specFilePath string) string {
	return strings.Join([]string{
		fmt.Sprintf("You are a FRESH, adversarial technical-spec reviewer for %s#%d — you did NOT write this spec.", issue.Repo, issue.IssueNumber),
		fmt.Sprintf("The tech spec is the file `%s` in this worktree. Read it.", specFilePath),
		"Adversarially interrogate it against the REAL codebase in this worktree: open the actual source at any file:line it cites; if a claim can't be verified from real code, flag it '⚠️ 未经源码验证'. Never pretend to have read code you didn't.",
		"Hunt for: missing/weak acceptance criteria, unhandled edge cases, security/data/migration/concurrency risk, hand-wavy steps, scope creep, and anything a worker couldn't implement unambiguously.",
		fmt.Sprintf("REVISE the spec file `%s` IN PLACE to resolve what you can (keep the structure; tighten, don't bloat). Edit ONLY that file — do NOT write anything outside this worktree, do NOT run `plane`/`gh`, do NOT push or open a pull request. looper publishes the file to Plane for you.", specFilePath),
		"For anything you genuinely cannot decide (a real product ambiguity), do NOT guess — leave it as a clearly-labelled open question in the spec.",
		"Finish with a concise grill transcript as your final summary: what you challenged, what you fixed in the file, and what remains open.",
	}, "\n")
}

// buildReviewPrompt is the independent reviewer's brief (node H REVIEW) — a different
// pass from the grill, reading the converged spec file and issuing a verdict (no writes).
func buildReviewPrompt(issue checkpointIssue, specFilePath string) string {
	return strings.Join([]string{
		fmt.Sprintf("You are an INDEPENDENT spec reviewer for %s#%d, reviewing a tech spec that already passed an adversarial grill.", issue.Repo, issue.IssueNumber),
		fmt.Sprintf("Read the spec file `%s` in this worktree.", specFilePath),
		"Verify against the real codebase in this worktree (open cited source; flag unverified claims). Judge whether a worker could implement it unambiguously and safely.",
		"This is a READ-ONLY verdict pass: do NOT edit any file, do NOT run `plane`/`gh`, do NOT push or open a pull request. If you find a blocking gap, state it precisely; otherwise confirm it is implementation-ready.",
		"Finish with a concise verdict as your final summary: READY or the specific blockers.",
	}, "\n")
}

// htmlEscape escapes text for safe embedding inside a Plane comment's HTML body.
func htmlEscape(s string) string { return htmlpkg.EscapeString(s) }

// codexLogNoise matches codex runtime log lines (timestamp + level) that sometimes
// leak into an agent's summary.
var codexLogNoise = regexp.MustCompile(`(?i)\d{4}-\d{2}-\d{2}T[\d:.]+Z?\s+(?:ERROR|INFO|WARN|DEBUG|TRACE)\s+[^\n]*`)

// bareNumberSummary matches a summary that is ONLY digits + separators (e.g. a stray
// token/byte count like "105,940"). That happens when an agent emits no
// __LOOPER_RESULT__ summary and the fallback grabs its last log line — a naked number,
// never a real grill/review conclusion, so we replace it with a placeholder.
var bareNumberSummary = regexp.MustCompile(`^[\d.,%\s]+$`)

// ansiEscape matches a full ANSI/CSI escape sequence (ESC + [ ... + final byte),
// e.g. "\x1b[0m" (reset), "\x1b[2K" (erase line). These leak into a summary when the
// fallback grabs a terminal-styled log line.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// csiResidueOnly matches a summary that is NOTHING but CSI residue whose ESC byte was
// already stripped upstream (so it renders as visible junk like "[0m" / "[2K[1G").
// Whole-string only, so legitimate prose containing a bracket is never touched.
var csiResidueOnly = regexp.MustCompile(`^(?:\[[0-9;?]*[a-zA-Z])+$`)

// cleanAgentSummary strips codex log noise (timestamped ERROR/INFO router lines, the
// stdin prompt) and terminal control codes from an agent summary so a grill/review
// transcript reads as prose, not machine logs. Falls back to a placeholder if
// filtering would leave nothing meaningful.
func cleanAgentSummary(s string) string {
	cleaned := ansiEscape.ReplaceAllString(s, " ")
	cleaned = codexLogNoise.ReplaceAllString(cleaned, " ")
	cleaned = strings.ReplaceAll(cleaned, "Reading additional input from stdin...", " ")
	cleaned = strings.TrimSpace(strings.Join(strings.Fields(cleaned), " "))
	if cleaned == "" || bareNumberSummary.MatchString(cleaned) || csiResidueOnly.MatchString(cleaned) {
		// Empty (all log noise), a bare number (a stray token/byte count), or nothing
		// but terminal control residue (e.g. "[0m") — all happen when the agent emits
		// no __LOOPER_RESULT__ summary and the fallback grabs its last log line. None is
		// a conclusion; post a placeholder, never the raw logs / a naked number / a code.
		return "(本轮 agent 未产出可展示的结论)"
	}
	return cleaned
}

// truncateRunes caps s to n runes, appending an ellipsis when it was cut.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// isPlaneProject reports whether the project delegates to Plane (its planeDoc
// resolver resolves), i.e. the tech spec lives on a Plane page rather than a spec PR.
func (r *Runner) isPlaneProject(projectID string) bool {
	if r.planeDoc == nil {
		return false
	}
	_, _, ok := r.planeDoc(projectID)
	return ok
}

// runPlanePublishStep is the Plane-provider publish path (flowchart §需求 side): the
// tech spec is written to a Plane page (node G) and reviewed there via page comments
// (node H) — there is NO GitHub spec PR and nothing is pushed. It publishes the
// agent's spec, verifies it landed (no PR fallback exists on Plane), and completes;
// opening the implementation PR is the worker's job after the spec is approved.
func (r *Runner) runPlanePublishStep(ctx context.Context, input stepInput, gateway *planedoc.Gateway, planeProjectID string) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	worktree, err := requireWorktree(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return checkpoint, &loopError{message: "Plane publish: cannot resolve work item id from " + issue.URL, kind: FailureManualIntervention}
	}
	// node G: write the tech spec to a Plane page + link it (idempotent).
	if err := r.publishTechSpecToPlane(ctx, input, *issue, *worktree); err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("publish tech spec to Plane: %v", err), kind: FailureRetryableTransient}
	}
	// Verify it actually landed: the agent must have written a spec file. Without the
	// tech-spec link there is nothing to review and — unlike the GitHub path — no PR
	// to fall back to, so hold for a human rather than silently completing empty.
	specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.TechSpecLinkTitle)
	if err != nil {
		return checkpoint, &loopError{message: fmt.Sprintf("verify tech spec link: %v", err), kind: FailureRetryableTransient}
	}
	if !found {
		return checkpoint, &loopError{message: "planner produced no tech spec to publish to Plane (agent wrote no spec file)", kind: FailureManualIntervention}
	}
	// Retire any PR an agent opened on this branch despite instructions — on a Plane
	// project the spec is the page, not a PR, so a planner-branch PR is a stray.
	if r.github != nil {
		if stray, sErr := r.findOpenPullRequestForBranch(ctx, issue.Repo, worktree.Branch, worktree.BaseBranch, input.Project.RepoPath); sErr == nil && stray != nil && stray.Number > 0 {
			if err := r.github.ClosePullRequest(ctx, ClosePullRequestInput{Repo: issue.Repo, PRNumber: stray.Number, CWD: input.Project.RepoPath}); err != nil {
				if r.logger != nil {
					r.logger.Warn("plane publish: close stray agent PR failed (continuing)", map[string]any{"repo": issue.Repo, "pr": stray.Number, "error": err.Error()})
				}
			} else if r.logger != nil {
				r.logger.Info("plane publish: closed stray agent-opened PR", map[string]any{"repo": issue.Repo, "pr": stray.Number})
			}
		}
	}
	// Leave a neutral [looper] status note on the spec page — the tech-spec draft has
	// landed and is entering review (node H). The GRILL + human-approve (via a Feishu
	// card, not a page reply) is a later stage; this note is only an audit breadcrumb,
	// not an approve invitation. Idempotent + best-effort — a failed note must not wedge
	// the planner (the tech-spec link is the real discovery signal).
	noteHTML := planedoc.SignComment("<p>技术方案初稿已写到本页,进入评审(node H)。人看过没问题在本页评论 approve / 同意 / 👍 即进入实现。</p>", "planner", strings.TrimSpace(r.agentModel))
	if _, err := gateway.PostSpecReviewComment(ctx, planeProjectID, specURL, noteHTML); err != nil && r.logger != nil {
		r.logger.Warn("planner: post spec status note failed (continuing)", map[string]any{"projectId": input.Project.ID, "page": specURL, "error": err.Error()})
	}
	if checkpoint.Publish == nil {
		checkpoint.Publish = &checkpointPublishState{}
	}
	checkpoint.Publish.PlaneSpecReview = true
	// node H condition #2: surface the spec DRAFT in the Feishu thread + @product owner
	// (FYI) — so a human sees it as review begins, before grill/review run.
	r.postNodeHThreadNote(ctx, input, "specDraftFYI", "📋 技术方案初稿已出炉:"+specURL+"\n即将进入 fresh agent 拷问(GRILL)+ 独立复核(REVIEW),收敛后请你在方案页 approve。", false)
	// node H begins here: the tech spec is on Plane. The grill step runs next; only
	// after grill + review converge does the loop open the human-approve gate. Mark the
	// card so it reads 🔬 方案拷问中 instead of resting on 编写技术方案中.
	r.setNodeHPhase(ctx, input.Loop.ID, "grilling")
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// postNodeHThreadNote posts a node H touchpoint into the loop's Feishu thread,
// @-mentioning the project's product owner so a human sees the spec draft / grill
// transcript in the thread (goal condition #2). Best-effort — a missing transport or
// owner just skips the ping.
func (r *Runner) postNodeHThreadNote(ctx context.Context, input stepInput, dedupKey, text string, mention bool) {
	if r.postThreadNote == nil {
		return
	}
	// Post each node-H thread note at most ONCE per loop. A follow-up resume re-enters
	// the pipeline (线程永远可追问); without this the draft-FYI / grill / approval notes
	// would re-post and spam the thread on every re-run. dedupKey "" opts out.
	if r.nodeHNotePosted(ctx, input.Loop.ID, dedupKey) {
		return
	}
	// Only @-mention on the ACTIONABLE post (please approve); the draft FYI and grill
	// transcript are informational and shouldn't ping a human every time.
	var mentions []string
	if mention && r.projectRoleConfig != nil {
		if openID := strings.TrimSpace(config.ProjectProductOwner(*r.projectRoleConfig, input.Project.ID).FeishuOpenID); openID != "" {
			mentions = []string{openID}
		}
	}
	if err := r.postThreadNote(ctx, input.Loop.ID, text, mentions); err != nil {
		if r.logger != nil {
			r.logger.Warn("planner: post node H thread note failed (continuing)", map[string]any{"loopId": input.Loop.ID, "error": err.Error()})
		}
		return
	}
	r.markNodeHNotePosted(ctx, input.Loop.ID, dedupKey)
}

// nodeHNotesPostedSet reads the set of node-H note keys already posted for a loop from
// its metadata (`nodeHNotesPosted` string list).
func nodeHNotesPostedSet(metadataJSON *string) map[string]bool {
	out := map[string]bool{}
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return out
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return out
	}
	raw, ok := meta["nodeHNotesPosted"].([]any)
	if !ok {
		return out
	}
	for _, v := range raw {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out[s] = true
		}
	}
	return out
}

// nodeHNotePosted reports whether the node-H note keyed by dedupKey was already posted
// for this loop. dedupKey "" is never deduped; a read error reports "not posted" so the
// note still goes out rather than being silently dropped.
func (r *Runner) nodeHNotePosted(ctx context.Context, loopID, dedupKey string) bool {
	if strings.TrimSpace(dedupKey) == "" || r.repos == nil || r.repos.Loops == nil {
		return false
	}
	loop, err := r.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return false
	}
	return nodeHNotesPostedSet(loop.MetadataJSON)[dedupKey]
}

// markNodeHNotePosted records that the node-H note keyed by dedupKey has been posted for
// this loop, re-reading + merging so it doesn't clobber markers set by earlier steps.
func (r *Runner) markNodeHNotePosted(ctx context.Context, loopID, dedupKey string) {
	if strings.TrimSpace(dedupKey) == "" || r.repos == nil || r.repos.Loops == nil {
		return
	}
	loop, err := r.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}
	posted := nodeHNotesPostedSet(loop.MetadataJSON)
	posted[dedupKey] = true
	keys := make([]string, 0, len(posted))
	for k := range posted {
		keys = append(keys, k)
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"nodeHNotesPosted": keys})
	if err != nil {
		return
	}
	if _, err := r.updateLoop(ctx, *loop, func(u *storage.LoopRecord) { u.MetadataJSON = stringPtr(metadataJSON) }); err != nil && r.logger != nil {
		r.logger.Warn("planner: mark node H note posted failed", map[string]any{"loopId": loopID, "key": dedupKey, "error": err.Error()})
	}
}

// setNodeHPhase records the planner's node H spec-pipeline phase in loop metadata
// (authoring / grilling / reviewing / awaiting_human_review) so the anchor card names
// the exact sub-phase. Best-effort + guarded — re-reads the current loop so it merges
// with markers set by earlier steps rather than clobbering them.
func (r *Runner) setNodeHPhase(ctx context.Context, loopID, phase string) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	loop, err := r.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}
	metadataJSON, err := mergeLoopMetadataJSON(loop.MetadataJSON, map[string]any{"nodeHPhase": phase})
	if err != nil {
		return
	}
	if _, err := r.updateLoop(ctx, *loop, func(u *storage.LoopRecord) { u.MetadataJSON = stringPtr(metadataJSON) }); err != nil && r.logger != nil {
		r.logger.Warn("planner: set node H phase failed", map[string]any{"loopId": loopID, "phase": phase, "error": err.Error()})
	}
}

// publishTechSpecToPlane writes the agent's tech spec to a Plane page and links it
// to the work item (node G). No-op for github/forgejo projects, when the work item
// can't be resolved, or when a tech-spec page is already linked (idempotent).
func (r *Runner) publishTechSpecToPlane(ctx context.Context, input stepInput, issue checkpointIssue, worktree checkpointWorktree) error {
	if r.planeDoc == nil {
		return nil
	}
	gateway, planeProjectID, ok := r.planeDoc(input.Project.ID)
	if !ok || gateway == nil {
		return nil
	}
	workItemID := planedoc.WorkItemIDFromURL(issue.URL)
	if workItemID == "" {
		return nil
	}
	if _, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.TechSpecLinkTitle); err != nil {
		return err
	} else if found {
		return nil // already published
	}
	specPath := firstNonEmpty(worktree.SpecPath, issue.SpecPath)
	content, err := readPlannerSpecFile(worktree.Path, specPath)
	if err != nil || strings.TrimSpace(content) == "" {
		return err // nothing to publish (agent wrote no spec file) — leave it to the GitHub PR
	}
	_, err = gateway.WriteTechSpec(ctx, planeProjectID, workItemID, "Tech Spec: "+issue.Title, content)
	return err
}

// readPlannerSpecFile reads the spec markdown the agent wrote in the worktree.
func readPlannerSpecFile(worktreePath, specPath string) (string, error) {
	if strings.TrimSpace(specPath) == "" {
		return "", nil
	}
	resolved := specPath
	if !filepath.IsAbs(specPath) {
		resolved = filepath.Join(worktreePath, specPath)
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(content), nil
}

func plannerWorktreeRoot(project storage.ProjectRecord) (string, error) {
	projectMetadata := parseJSONObject(project.MetadataJSON)
	worktreeRoot := stringFromAnyDefault(projectMetadata["worktreeRoot"])
	if worktreeRoot != "" {
		return worktreeRoot, nil
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

func (r *Runner) findOpenPullRequestForBranch(ctx context.Context, repo, branch, baseBranch, cwd string) (*PullRequestSummary, error) {
	if r.github == nil || strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	pullRequests, err := r.github.ListOpenPullRequests(ctx, ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: plannerPRDedupeLookupLimit})
	if err != nil {
		return nil, err
	}
	for _, pr := range pullRequests {
		state := strings.TrimSpace(pr.State)
		if state != "" && !strings.EqualFold(state, "open") {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(pr.HeadRefName), strings.TrimSpace(branch)) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(pr.BaseRefName), strings.TrimSpace(baseBranch)) {
			continue
		}
		if pr.Number <= 0 {
			continue
		}
		candidate := pr
		return &candidate, nil
	}
	return nil, nil
}

func (r *Runner) validatedLifecyclePullRequest(ctx context.Context, input stepInput, issue checkpointIssue, worktree checkpointWorktree, state *lifecycle.State) (*checkpointPullRequest, error) {
	if state == nil || state.PRNumber <= 0 {
		return nil, nil
	}
	detail, err := r.github.ViewPullRequest(ctx, ViewPullRequestInput{Repo: issue.Repo, PRNumber: state.PRNumber, CWD: input.Project.RepoPath})
	if err != nil {
		return nil, nil
	}
	if detail.State != "" && !strings.EqualFold(strings.TrimSpace(detail.State), "open") {
		return nil, nil
	}
	if !strings.EqualFold(strings.TrimSpace(detail.HeadRefName), strings.TrimSpace(worktree.Branch)) || !strings.EqualFold(strings.TrimSpace(detail.BaseRefName), strings.TrimSpace(worktree.BaseBranch)) {
		return nil, nil
	}
	prNumber := detail.Number
	if prNumber == 0 {
		prNumber = state.PRNumber
	}
	return &checkpointPullRequest{Number: prNumber, URL: firstNonEmpty(detail.URL, state.PRURL), Body: ""}, nil
}

func (r *Runner) normalizePullRequestDisclosure(ctx context.Context, repo string, prNumber int64, cwd string, force bool) error {
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
	stamper := disclosure.Stamper{Config: r.disclosure, Agent: r.agentRuntime, Model: r.agentModel}
	body := stamper.Markdown(detail.Body, "planner", disclosure.ChannelPullRequest)
	if body == detail.Body {
		return nil
	}
	return r.github.UpdatePullRequestBody(ctx, UpdatePullRequestBodyInput{Repo: repo, PRNumber: prNumber, Body: body, CWD: cwd})
}

func (r *Runner) persistPlannerPullRequestReference(ctx context.Context, input stepInput, issue checkpointIssue, worktree checkpointWorktree, pr checkpointPullRequest) error {
	if pr.Number == 0 {
		return nil
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		updated.Repo = stringPtr(issue.Repo)
		updated.PRNumber = &pr.Number
	}); err != nil {
		return err
	}
	metadataJSON, err := mergeLoopMetadataJSON(input.Loop.MetadataJSON, map[string]any{"issueNumber": issue.IssueNumber, "issueUrl": issue.URL, "issueTitle": issue.Title, "specPath": issue.SpecPath, "branch": worktree.Branch, "prUrl": pr.URL, "prNumber": pr.Number, "requestedReviewers": issue.RequestedReviewers})
	if err != nil {
		return err
	}
	_, err = r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) { updated.MetadataJSON = stringPtr(metadataJSON) })
	return err
}

func (r *Runner) runNotifyStep(input stepInput) (plannerCheckpoint, error) {
	checkpoint := input.Checkpoint
	if checkpoint.SkipReason != "" || checkpoint.Notify != nil {
		return checkpoint, nil
	}
	issue, err := requireIssue(checkpoint)
	if err != nil {
		return checkpoint, err
	}
	message := fmt.Sprintf("Planner completed for %s#%d", issue.Repo, issue.IssueNumber)
	if checkpoint.Publish != nil && checkpoint.Publish.PullRequest != nil && checkpoint.Publish.PullRequest.URL != "" {
		message = "Spec PR ready for review: " + checkpoint.Publish.PullRequest.URL
	}
	checkpoint.Notify = &checkpointNotify{SentAt: r.nowISO(), Message: message}
	checkpoint.ResumePolicy = "advance_from_checkpoint"
	return checkpoint, nil
}

// latestNativeSessionID returns the loop's most recent captured agent session id, so
// a follow-up write-spec turn can native-resume the SAME session and keep the full
// spec-authoring conversation. Empty when none is recorded (e.g. a vendor that did
// not emit a session id) — the follow-up gate then refuses to reactivate.
func (r *Runner) latestNativeSessionID(ctx context.Context, loopID string) string {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return ""
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || execution == nil || execution.NativeSessionID == nil {
		return ""
	}
	return strings.TrimSpace(*execution.NativeSessionID)
}

// clearHumanInbox drops ONLY the messages that were actually fed to this write-spec
// turn (drained), so they are not re-injected on a later run. Messages that arrived
// WHILE the agent was mid-run (never read by it) are preserved for a follow-up turn
// rather than silently eaten. No-op when nothing was drained or the inbox is empty.
func (r *Runner) clearHumanInbox(ctx context.Context, loop *storage.LoopRecord, drained []loops.HumanMessage) {
	if r.repos == nil || r.repos.Loops == nil || len(drained) == 0 {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	if len(loops.ReadHumanInbox(fresh.MetadataJSON)) == 0 {
		return
	}
	meta, werr := loops.RemoveHumanMessages(fresh.MetadataJSON, drained)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = &meta
	}
}

// shouldFollowupResume reports whether a finished planner loop that just received a
// new human thread message can be safely reactivated for a single incremental
// follow-up turn (线程永远可追问). When true, createRunContext resumes at stepWriteSpec
// with the spec-authoring + node-H gate flags rewound so the write-spec step
// native-resumes the SAME planner session, drains the queued message, and revises
// the spec incrementally — then re-publishes/re-grills/re-reviews the updated spec
// (all idempotent). It NEVER re-discovers or re-plans from scratch.
//
// Strict guard so the "re-plan the whole thing" path can never be taken:
//   - the prior run must have completed successfully ("success"); and
//   - the loop must carry at least one pending human message; and
//   - the checkpoint must carry the issue + a worktree + a completed write-spec; and
//   - a native agent session id must be captured. Without a session, native resume
//     is impossible and write-spec would author a fresh spec — so we refuse and let
//     the caller fall back to the honest closed-task ack instead.
//
// Only ever reachable via the follow-up reactivation path (enqueueHumanMessageToLoop,
// which only reactivates planner loops that have a captured session); a completed
// loop is otherwise never re-dispatched.
func (r *Runner) shouldFollowupResume(ctx context.Context, loop storage.LoopRecord, latestRun *storage.RunRecord, checkpoint plannerCheckpoint) bool {
	// "success" = a finished loop the human is following up on; "interrupted" = the
	// human's mid-run @bot killed the active agent (强操控) — either way, answer them from
	// the main write-spec session. A genuinely "failed" run still retries normally.
	if latestRun == nil || (latestRun.Status != "success" && latestRun.Status != "interrupted") {
		return false
	}
	if len(loops.ReadHumanInbox(loop.MetadataJSON)) == 0 {
		return false
	}
	if checkpoint.Issue == nil || checkpoint.Worktree == nil {
		return false
	}
	if checkpoint.WriteSpec == nil || !strings.EqualFold(checkpoint.WriteSpec.Status, "completed") {
		return false
	}
	return strings.TrimSpace(r.latestNativeSessionID(ctx, loop.ID)) != ""
}

// rewindPlannerCheckpointForFollowup rewinds a completed planner checkpoint so a
// follow-up turn re-enters write-spec (re-runs the agent, native-resuming the SAME
// session with the human message so the MAIN looper — not the fresh grill critic —
// answers). It does NOT clear Grilled/Reviewed: a follow-up is "respond then hand the
// wheel back to the human" (强操控), so it must NOT auto-re-run the minutes-long
// grill/review passes or re-post their thread notes on every question. The revised
// spec re-settles at the human approval gate; the human drives any re-validation.
// Preserves everything needed to resume: issue, worktree, lock, lifecycle, publish ref.
func rewindPlannerCheckpointForFollowup(checkpoint plannerCheckpoint) plannerCheckpoint {
	checkpoint.WriteSpec = nil
	checkpoint.SkipReason = ""
	return checkpoint
}

func (r *Runner) createRunContext(ctx context.Context, loop storage.LoopRecord) (resumedRunContext, error) {
	latestRun, err := r.repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return resumedRunContext{}, err
	}
	check := parseCheckpoint(nil)
	lastCompleted := PlannerStep("")
	if latestRun != nil {
		check = parseCheckpoint(latestRun.CheckpointJSON)
		lastCompleted = asPlannerStep(derefString(latestRun.LastCompletedStep))
	}
	// FOLLOW-UP RESUME (线程永远可追问): a planner loop got a new human message — resume
	// the SAME session at write-spec so the MAIN looper answers (never the fresh grill
	// critic), for ONE incremental turn (see shouldFollowupResume). This takes PRECEDENCE
	// over shouldResume: when a run was interrupted BY the human's message (强操控 mid-run
	// @bot kills the active agent → interrupted), we must answer them from the main
	// session, not silently resume the interrupted grill/review step.
	followupResume := r.shouldFollowupResume(ctx, loop, latestRun, check)
	shouldResume := !followupResume && latestRun != nil && (latestRun.Status == "failed" || latestRun.Status == "interrupted") && !loops.IsManualHoldResumePolicy(check.ResumePolicy) && lastCompleted != ""
	startStep := stepDiscoverIssues
	switch {
	case followupResume:
		startStep = stepWriteSpec
	case shouldResume:
		if next := nextPlannerStep(lastCompleted); next != "" {
			startStep = next
		}
	}
	resumed := followupResume || (shouldResume && startStep != stepDiscoverIssues)
	initialCheckpoint := plannerCheckpoint{ResumePolicy: "replay_step"}
	if followupResume {
		initialCheckpoint = rewindPlannerCheckpointForFollowup(check)
		initialCheckpoint.ResumePolicy = "advance_from_checkpoint"
	} else if resumed {
		initialCheckpoint = check
		initialCheckpoint.ResumePolicy = "advance_from_checkpoint"
	}
	nowISO := r.nowISO()
	run := storage.RunRecord{ID: eventlog.NewEventID("run"), LoopID: loop.ID, Status: "running", CurrentStep: stringPtr(string(startStep)), StartedAt: nowISO, LastHeartbeatAt: stringPtr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}
	if followupResume {
		if prev := previousPlannerStep(startStep); prev != "" {
			run.LastCompletedStep = stringPtr(string(prev))
		}
	} else if resumed && lastCompleted != "" {
		run.LastCompletedStep = stringPtr(string(lastCompleted))
	}
	encoded := mustMarshalJSON(initialCheckpoint)
	run.CheckpointJSON = &encoded
	if err := r.repos.Runs.Upsert(ctx, run); err != nil {
		return resumedRunContext{}, err
	}
	return resumedRunContext{Run: run, StartStep: startStep, Checkpoint: initialCheckpoint, Resumed: resumed}, nil
}

func (r *Runner) persistStepStarted(ctx context.Context, run storage.RunRecord, step PlannerStep, checkpoint plannerCheckpoint) (storage.RunRecord, error) {
	updated := run
	nowISO := r.nowISO()
	updated.CurrentStep = stringPtr(string(step))
	encoded := mustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := r.repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func (r *Runner) persistStepCompleted(ctx context.Context, run storage.RunRecord, step PlannerStep, checkpoint plannerCheckpoint) (storage.RunRecord, error) {
	updated := run
	nowISO := r.nowISO()
	if next := nextPlannerStep(step); next != "" {
		updated.CurrentStep = stringPtr(string(next))
	} else {
		updated.CurrentStep = nil
	}
	updated.LastCompletedStep = stringPtr(string(step))
	encoded := mustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.LastHeartbeatAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := r.repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func (r *Runner) completeRun(ctx context.Context, run storage.RunRecord, status, summary, errorMessage string, checkpoint plannerCheckpoint) (storage.RunRecord, error) {
	updated := run
	endedAt := r.nowISO()
	updated.Status = status
	if summary != "" {
		updated.Summary = stringPtr(summary)
	}
	if errorMessage != "" {
		updated.ErrorMessage = stringPtr(errorMessage)
	}
	encoded := mustMarshalJSON(checkpoint)
	updated.CheckpointJSON = &encoded
	updated.EndedAt = &endedAt
	updated.LastHeartbeatAt = &endedAt
	updated.UpdatedAt = endedAt
	if err := r.repos.Runs.Upsert(ctx, updated); err != nil {
		return storage.RunRecord{}, err
	}
	return updated, nil
}

func (r *Runner) persistCheckpoint(ctx context.Context, runID string, step PlannerStep, checkpoint plannerCheckpoint) error {
	run, err := r.repos.Runs.GetByID(ctx, runID)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("run not found: %s", runID)
	}
	_, err = r.persistStepStarted(ctx, *run, step, checkpoint)
	return err
}

func (r *Runner) getLatestCheckpoint(ctx context.Context, run storage.RunRecord, fallback plannerCheckpoint) plannerCheckpoint {
	persisted, err := r.repos.Runs.GetByID(ctx, run.ID)
	if err != nil || persisted == nil {
		return fallback
	}
	return parseCheckpoint(persisted.CheckpointJSON)
}

type eventInput struct {
	eventType  string
	projectID  string
	loopID     string
	runID      string
	entityType string
	entityID   string
	payload    any
}

func (r *Runner) appendEvent(ctx context.Context, input eventInput) {
	if r.repos == nil || r.repos.Events == nil {
		return
	}
	_ = eventlog.Append(ctx, r.repos, eventlog.AppendInput{EventType: input.eventType, ProjectID: optionalString(input.projectID), LoopID: optionalString(input.loopID), RunID: optionalString(input.runID), EntityType: optionalString(input.entityType), EntityID: optionalString(input.entityID), ActorType: optionalString("system"), ActorID: optionalString("planner-loop"), ActorDisplayName: optionalString("planner-loop"), Payload: input.payload, CreatedAt: r.now()})
}

type loopUpsertResult struct {
	record  storage.LoopRecord
	created bool
}

func (r *Runner) ensureLoopForIssue(ctx context.Context, project storage.ProjectRecord, repo string, issue IssueSummary, currentFingerprint string) (loopUpsertResult, error) {
	nowISO := r.nowISO()
	targetID := buildIssueTargetID(repo, issue.Number)
	existingLoops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	for _, existing := range existingLoops {
		if existing.Type == "planner" && existing.ProjectID == project.ID && existing.TargetType == "issue" && derefString(existing.TargetID) == targetID {
			pausedOrCompleted := existing.Status == "paused" || existing.Status == "completed" || existing.Status == "awaiting_human"
			updated := existing
			updated.Repo = stringPtr(repo)
			suppressFailedRevival := loops.ShouldSuppressFailedRediscovery(existing.Status, loops.LastFailedDiscoveryFingerprint(existing.MetadataJSON), currentFingerprint)
			if !pausedOrCompleted && !suppressFailedRevival && updated.Status != "running" {
				updated.Status = "queued"
				updated.NextRunAt = &nowISO
			}
			metadataJSON, err := mergeLoopMetadataJSON(existing.MetadataJSON, map[string]any{"issueTitle": issue.Title, "issueURL": issue.URL, "issueNumber": issue.Number, "specPath": buildSpecPath(r.now(), issue.Number, issue.Title)})
			if err == nil {
				updated.MetadataJSON = stringPtr(metadataJSON)
			}
			updated.UpdatedAt = nowISO
			if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
				return loopUpsertResult{}, err
			}
			return loopUpsertResult{record: updated, created: false}, nil
		}
	}
	seq, err := r.repos.Loops.AllocateSeq(ctx)
	if err != nil {
		return loopUpsertResult{}, err
	}
	meta := mustMarshalJSON(map[string]any{"issueTitle": issue.Title, "issueURL": issue.URL, "issueNumber": issue.Number, "specPath": buildSpecPath(r.now(), issue.Number, issue.Title)})
	loop := storage.LoopRecord{ID: eventlog.NewEventID("loop"), Seq: seq, ProjectID: project.ID, Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "queued", MetadataJSON: &meta, NextRunAt: &nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := r.repos.Loops.Upsert(ctx, loop); err != nil {
		return loopUpsertResult{}, err
	}
	r.appendEvent(ctx, eventInput{eventType: "loop.created", projectID: project.ID, loopID: loop.ID, entityType: "loop", entityID: loop.ID, payload: map[string]any{"type": "planner", "repo": repo, "issueNumber": issue.Number}})
	return loopUpsertResult{record: loop, created: true}, nil
}

type enqueueInput struct {
	ProjectID   string
	LoopID      string
	Repo        string
	IssueNumber int64
	Payload     map[string]any
}

func (r *Runner) enqueue(ctx context.Context, input enqueueInput) (storage.QueueItemRecord, error) {
	dedupeKey := buildPlannerDedupeKey(input.ProjectID, input.LoopID, input.Repo, input.IssueNumber)
	existing, err := r.repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		return storage.QueueItemRecord{}, err
	}
	if existing != nil {
		return *existing, nil
	}
	nowISO := r.nowISO()
	targetID := buildIssueTargetID(input.Repo, input.IssueNumber)
	lockKey := buildIssueLockKey(input.Repo, input.IssueNumber)
	projectID := input.ProjectID
	loopID := input.LoopID
	payload := mustMarshalJSON(input.Payload)
	queueItem := storage.QueueItemRecord{ID: eventlog.NewEventID("queue"), ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: targetID, Repo: &input.Repo, DedupeKey: dedupeKey, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: r.retryMaxAttempts, LockKey: &lockKey, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}
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

func (r *Runner) failQueueItem(ctx context.Context, queueItem storage.QueueItemRecord, kind QueueFailureKind, message string) (*storage.QueueItemRecord, error) {
	nextAttempts := queueItem.Attempts + 1
	nowISO := r.nowISO()
	if !shouldRetryQueueFailure(kind, nextAttempts, queueItem.MaxAttempts) {
		if err := r.repos.Queue.Fail(ctx, storage.QueueFailInput{ID: queueItem.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
			return nil, err
		}
		return r.repos.Queue.GetByID(ctx, queueItem.ID)
	}
	retryAt := eventlog.FormatJavaScriptISOString(r.now().Add(backoffDelay(r.retryBaseDelay, cappedRetryDelayAttempt(nextAttempts, queueItem.MaxAttempts))))
	if err := r.repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: queueItem.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: optionalString(message), ErrorKind: string(kind), UpdatedAt: nowISO}); err != nil {
		return nil, err
	}
	updated, err := r.repos.Queue.GetByID(ctx, queueItem.ID)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (r *Runner) updateLoop(ctx context.Context, loop storage.LoopRecord, mutate func(*storage.LoopRecord)) (storage.LoopRecord, error) {
	current, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if current == nil {
		return storage.LoopRecord{}, fmt.Errorf("loop not found: %s", loop.ID)
	}
	if current.Status == "terminated" {
		return *current, nil
	}
	updated := *current
	mutate(&updated)
	updated.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
		return storage.LoopRecord{}, err
	}
	return updated, nil
}

func (r *Runner) classifyFailure(err error) *loopError {
	return r.classifyFailureWithBoundary(err, failureclass.BoundaryUnknown)
}

func (r *Runner) classifyFailureWithBoundary(err error, boundary failureclass.Boundary) *loopError {
	var typed *loopError
	if errors.As(err, &typed) {
		return typed
	}
	var transient transientFailure
	if errors.As(err, &transient) && transient.Temporary() {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	if githubinfra.IsTransientError(err) {
		return &loopError{message: err.Error(), kind: FailureRetryableTransient}
	}
	return &loopError{message: err.Error(), kind: plannerFailureKind(failureclass.Classify(err, failureclass.Context{Runner: failureclass.RunnerPlanner, Boundary: boundary}))}
}

func plannerFailureBoundaryForStep(step PlannerStep) failureclass.Boundary {
	switch step {
	case stepDiscoverIssues, stepPublish, stepNotify:
		return failureclass.BoundaryGitHubAPI
	case stepPrepareWorktree:
		return failureclass.BoundaryGitRemote
	case stepWriteSpec, stepGrill, stepReview:
		return failureclass.BoundaryModelProvider
	default:
		return failureclass.BoundaryUnknown
	}
}

func plannerFailureKind(kind failureclass.Kind) QueueFailureKind {
	switch kind {
	case failureclass.RetryableTransient:
		return FailureRetryableTransient
	case failureclass.RetryableAfterResume:
		return FailureRetryableAfterResume
	case failureclass.RecoverableInfra:
		return FailureRecoverableInfra
	case failureclass.ManualIntervention:
		return FailureManualIntervention
	default:
		return FailureNonRetryable
	}
}

func (r *Runner) nowISO() string { return eventlog.FormatJavaScriptISOString(r.now()) }

func stepsFrom(start PlannerStep) []PlannerStep {
	startIndex := 0
	for i, step := range plannerStepSequence {
		if step == start {
			startIndex = i
			break
		}
	}
	return plannerStepSequence[startIndex:]
}

func nextPlannerStep(step PlannerStep) PlannerStep {
	for i, candidate := range plannerStepSequence {
		if candidate == step && i+1 < len(plannerStepSequence) {
			return plannerStepSequence[i+1]
		}
	}
	return ""
}

func previousPlannerStep(step PlannerStep) PlannerStep {
	for i, candidate := range plannerStepSequence {
		if candidate == step && i > 0 {
			return plannerStepSequence[i-1]
		}
	}
	return ""
}

func asPlannerStep(value string) PlannerStep {
	for _, candidate := range plannerStepSequence {
		if string(candidate) == value {
			return candidate
		}
	}
	return ""
}

func parseCheckpoint(value *string) plannerCheckpoint {
	if value == nil || *value == "" {
		return plannerCheckpoint{}
	}
	var checkpoint plannerCheckpoint
	if err := json.Unmarshal([]byte(*value), &checkpoint); err != nil {
		return plannerCheckpoint{}
	}
	return checkpoint
}

func parseJSONObject(value *string) map[string]any {
	if value == nil || *value == "" {
		return map[string]any{}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(*value), &parsed); err != nil {
		return map[string]any{}
	}
	return parsed
}

func mergeLoopMetadataJSON(current *string, updates map[string]any) (string, error) {
	parsed := parseJSONObject(current)
	for key, value := range updates {
		parsed[key] = value
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func requireIssue(checkpoint plannerCheckpoint) (*checkpointIssue, error) {
	if checkpoint.Issue == nil {
		return nil, &loopError{message: "Missing issue checkpoint for planner step", kind: FailureRetryableTransient}
	}
	return checkpoint.Issue, nil
}

func requireWorktree(checkpoint plannerCheckpoint) (*checkpointWorktree, error) {
	if checkpoint.Worktree == nil {
		return nil, &loopError{message: "Missing worktree checkpoint for planner step", kind: FailureRetryableTransient}
	}
	return checkpoint.Worktree, nil
}

func (c *plannerCheckpoint) ensureLifecycle(runner, branch, baseBranch string, expectPR bool) {
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

func buildPlannerPrompt(project storage.ProjectRecord, instructionConfig config.Config, issue *checkpointIssue, worktree *checkpointWorktree, allowAutoPush bool, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) (string, config.CustomInstructionBlock) {
	providerLabel := providerIssueSystemLabel(providerKindForProject(instructionConfig, project.ID))
	parts := []string{
		fmt.Sprintf("Write a planning spec for %s issue %s#%d.", providerLabel, issue.Repo, issue.IssueNumber),
		"Repository: " + issue.Repo,
		"Base branch: " + worktree.BaseBranch,
		"Spec path: " + issue.SpecPath,
		"Issue title: " + issue.Title,
	}
	if strings.TrimSpace(issue.Body) != "" {
		parts = append(parts, "Issue body:\n"+issue.Body)
	}
	if strings.TrimSpace(issue.URL) != "" {
		parts = append(parts, "Issue URL: "+issue.URL)
	}
	if agentsBlock := readAgentsBlock(project.RepoPath); agentsBlock != "" {
		parts = append(parts, agentsBlock)
	}
	instructionBlock := config.BuildCustomInstructionBlock(instructionConfig, project.ID, "planner")
	if instructionBlock.Text != "" {
		parts = append(parts, instructionBlock.Text)
	}
	requirements := []string{
		"Requirements:",
		"- Create or update the spec at " + issue.SpecPath,
		"- Use Markdown with clear problem, goals, approach, risks, and validation sections",
		"- Keep the implementation scope aligned to the issue",
	}
	if allowAutoPush {
		requirements = append(requirements, "- Commit the spec changes on the current branch so the PR can be opened")
	} else {
		requirements = append(requirements, "- Do not push the branch or open/update pull requests; leave repository publishing for Looper/manual follow-up")
	}
	parts = append(parts, strings.Join(requirements, "\n"))
	if allowAutoPush {
		parts = append(parts, lifecycle.PromptInstruction("planner", worktree.Branch, worktree.BaseBranch, true, true, disclosureCfg, agentRuntime, agentModel))
	} else {
		parts = append(parts, noRemoteLifecyclePromptInstruction("planner", worktree.Branch, worktree.BaseBranch, disclosureCfg, agentRuntime, agentModel))
	}
	return agent.AppendCompletionInstruction(strings.Join(parts, "\n\n")), instructionBlock
}

func providerKindForProject(cfg config.Config, projectID string) config.ProviderKind {
	for _, project := range cfg.Projects {
		if project.ID == projectID {
			return config.ResolvedProjectProviderKind(cfg, project)
		}
	}
	return config.ProviderKindGitHub
}

func providerIssueSystemLabel(kind config.ProviderKind) string {
	if kind == config.ProviderKindForgejo {
		return "Forgejo"
	}
	return "GitHub"
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
		"Include a git_pr_lifecycle object in the final " + "__LOOPER_RESULT__" + " JSON with branch, baseBranch, commitShas, pushed, prNumber, prUrl, prAdopted, and actions {commit,push,pr}; use action source \"agent\" only for local commits you completed and \"none\" for disabled remote actions.",
		fmt.Sprintf("Expected lifecycle runner=%q branch=%q baseBranch=%q expectPush=%t expectPR=%t fallbackAllowed=%t.", runner, branch, baseBranch, false, false, true),
	}, "\n")
}

func buildPullRequestBody(issue checkpointIssue, worktree checkpointWorktree, writeSpec *checkpointWriteSpec) string {
	lines := []string{"## Summary", fmt.Sprintf("- Adds the planning spec for %s#%d", issue.Repo, issue.IssueNumber), "- Spec path: " + issue.SpecPath, "- Planner branch: " + worktree.Branch}
	if issue.URL != "" {
		lines = append(lines, "- Source issue: "+issue.URL)
	}
	if writeSpec != nil && strings.TrimSpace(writeSpec.Summary) != "" {
		lines = append(lines, "", "## Agent Summary", writeSpec.Summary)
	}
	lines = append(lines, "", "Spec: "+issue.SpecPath, fmt.Sprintf("Issue: %s#%d", issue.Repo, issue.IssueNumber))
	return strings.Join(lines, "\n")
}

func buildPlannerFallbackCommitMessage(issue *checkpointIssue) string {
	title := "planner spec"
	if issue != nil && strings.TrimSpace(issue.Title) != "" {
		title = issue.Title
	}
	return "planner: " + strings.TrimSpace(title)
}

func readAgentsBlock(projectRepoPath string) string {
	content, err := os.ReadFile(filepath.Join(projectRepoPath, "AGENTS.md"))
	if err != nil {
		return ""
	}
	return "AGENTS.md:\n" + string(content)
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

func isManualPlannerQueue(payload map[string]any) bool {
	manual, ok := payload["manual"].(bool)
	return ok && manual
}

func shouldClaimIssue(issue IssueSummary, login string, policy DiscoveryPolicy) bool {
	if policy.RequireAssigneeCurrentUser && !includesLogin(issue.Assignees, login) {
		return false
	}
	return labelsMatch(issue.Labels, policy.Labels, policy.LabelMode)
}

func safeIssueQueryLabel(labels []string) string {
	for _, label := range labels {
		if strings.TrimSpace(label) != "" {
			return label
		}
	}
	return ""
}

func (r *Runner) listOpenIssuesForDiscovery(ctx context.Context, input ListOpenIssuesInput, policy DiscoveryPolicy) ([]IssueSummary, error) {
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

func labelsMatch(labels []string, required []string, mode config.LabelMode) bool {
	if len(required) == 0 {
		return true
	}
	if mode == config.LabelModeAny {
		for _, label := range required {
			if specpr.HasLabel(labels, label) {
				return true
			}
		}
		return false
	}
	for _, label := range required {
		if !specpr.HasLabel(labels, label) {
			return false
		}
	}
	return true
}

// looksLikeUUID reports whether s is shaped like a UUID (8-4-4-4-12 hex) — used to
// skip Plane assignee ids, which are UUIDs and must not be requested as GitHub
// reviewers (that would 422 and, before this, wedge the run on retry).
func looksLikeUUID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func resolveRequestedReviewers(project storage.ProjectRecord, loop storage.LoopRecord, assignees []string, currentLogin string) []string {
	requested := make([]string, 0)
	projectMetadata := parseJSONObject(project.MetadataJSON)
	loopConfig := parseJSONObject(loop.ConfigJSON)
	for _, source := range []any{loopConfig["reviewers"], projectMetadata["reviewers"]} {
		for _, value := range toStrings(source) {
			login := normalizeLogin(value)
			if login != "" && login != normalizeLogin(currentLogin) && !stringInSlice(login, requested) {
				requested = append(requested, login)
			}
		}
	}
	for _, assignee := range assignees {
		login := normalizeLogin(assignee)
		if login != "" && login != normalizeLogin(currentLogin) && !stringInSlice(login, requested) {
			requested = append(requested, login)
		}
	}
	return requested
}

func hasRequestedReviewerSources(project storage.ProjectRecord, loop storage.LoopRecord, assignees []string) bool {
	if len(assignees) > 0 {
		return true
	}
	projectMetadata := parseJSONObject(project.MetadataJSON)
	loopConfig := parseJSONObject(loop.ConfigJSON)
	return len(toStrings(loopConfig["reviewers"])) > 0 || len(toStrings(projectMetadata["reviewers"])) > 0
}

func buildIssueTargetID(repo string, issueNumber int64) string {
	return fmt.Sprintf("issue:%s:%d", repo, issueNumber)
}

// plannerQueuePayloadFPKey is the JSON key used to forward the planner
// discovery fingerprint into the queue payload so failure handlers can stamp
// it onto the loop without recomputing inputs from scratch.
const plannerQueuePayloadFPKey = "discoveryFingerprint"

// buildPlannerDiscoveryFingerprint returns a stable fingerprint over the
// inputs that planner discovery uses to decide whether a previously-failed
// loop should be revived. specPath is included so deliberate spec-path
// changes (e.g. issue title edits affecting slug, or day rollover) trigger
// rediscovery.
func buildPlannerDiscoveryFingerprint(repo string, now time.Time, issue IssueSummary) string {
	labels := loops.CanonicalSortedStrings(issue.Labels)
	assignees := loops.CanonicalSortedStrings(issue.Assignees)
	specPath := buildSpecPath(now, issue.Number, issue.Title)
	return loops.ComputeDiscoveryFingerprint(
		"planner",
		repo,
		fmt.Sprintf("%d", issue.Number),
		strings.TrimSpace(issue.Title),
		strings.TrimSpace(issue.Body),
		strings.TrimSpace(issue.URL),
		specPath,
		strings.Join(labels, ","),
		strings.Join(assignees, ","),
	)
}

// plannerQueueDiscoveryFingerprint reads the persisted fingerprint from a
// queue item's payload, returning empty when missing or invalid.
func plannerQueueDiscoveryFingerprint(payloadJSON *string) string {
	if payloadJSON == nil {
		return ""
	}
	raw := strings.TrimSpace(*payloadJSON)
	if raw == "" {
		return ""
	}
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ""
	}
	value, _ := parsed[plannerQueuePayloadFPKey].(string)
	return strings.TrimSpace(value)
}

// stampFailedDiscoveryFingerprint records the queue item's discovery
// fingerprint on the loop's metadata so the next discovery tick can suppress
// autonomous rediscovery while inputs are unchanged.
func (r *Runner) stampFailedDiscoveryFingerprint(updated *storage.LoopRecord, queueItem storage.QueueItemRecord) {
	fingerprint := plannerQueueDiscoveryFingerprint(queueItem.PayloadJSON)
	if fingerprint == "" {
		return
	}
	merged, err := loops.MergeLastFailedDiscoveryFingerprint(updated.MetadataJSON, fingerprint)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("planner fingerprint stamp failed", map[string]any{"loopId": updated.ID, "error": err.Error()})
		}
		return
	}
	updated.MetadataJSON = stringPtr(merged)
}

func buildPlannerDedupeKey(projectID, loopID, repo string, issueNumber int64) string {
	return fmt.Sprintf("planner:%s:%s:%s:%d", projectID, loopID, repo, issueNumber)
}
func buildIssueLockKey(repo string, issueNumber int64) string {
	return fmt.Sprintf("issue:%s:%d", repo, issueNumber)
}

func parseIssueNumberFromTargetID(targetID string) int64 {
	if targetID == "" {
		return 0
	}
	parts := strings.Split(targetID, ":")
	if len(parts) != 3 || parts[0] != "issue" {
		return 0
	}
	number, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0
	}
	return number
}

func buildPlannerBranch(issueNumber int64, title string) string {
	return fmt.Sprintf("looper/planner/%d-%s", issueNumber, buildPlannerSlug(title))
}

func buildSpecPath(now time.Time, issueNumber int64, title string) string {
	return fmt.Sprintf("specs/%s-%d-%s.md", now.UTC().Format("2006-01-02"), issueNumber, buildPlannerSlug(title))
}

func buildPlannerSlug(title string) string {
	normalized := strings.ToLower(title)
	replaced := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, normalized)
	parts := strings.FieldsFunc(replaced, func(r rune) bool { return r == '-' })
	if len(parts) > 4 {
		parts = parts[:4]
	}
	if len(parts) == 0 {
		return "issue"
	}
	return strings.Join(parts, "-")
}

func projectRepo(project storage.ProjectRecord) string {
	meta := parseJSONObject(project.MetadataJSON)
	return stringFromAnyDefault(meta["repo"])
}

func int64FromAny(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	}
	return 0
}

func toStrings(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if direct, ok := value.([]string); ok {
			return append([]string(nil), direct...)
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			result = append(result, text)
		}
	}
	return result
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func backoffDelay(base time.Duration, attempts int64) time.Duration {
	delay := base
	for i := int64(1); i < attempts; i++ {
		if delay >= maxRetryDelay || delay > maxRetryDelay/2 {
			return maxRetryDelay
		}
		delay *= 2
	}
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func isRetryableFailure(kind QueueFailureKind) bool {
	return kind == FailureRetryableTransient || kind == FailureRetryableAfterResume || kind == FailureRecoverableInfra || kind == FailureNonRetryable
}

func shouldRetryQueueFailure(kind QueueFailureKind, nextAttempts, maxAttempts int64) bool {
	if !isRetryableFailure(kind) {
		return false
	}
	if maxAttempts < 0 {
		return kind != FailureNonRetryable
	}
	return maxAttempts > 0 && nextAttempts < maxAttempts
}

func cappedRetryDelayAttempt(attempts, maxAttempts int64) int64 {
	if attempts <= 0 {
		return 1
	}
	if maxAttempts > 0 && attempts > maxAttempts {
		return maxAttempts
	}
	return attempts
}

func wrapRetryableAfterResume(err error) error {
	if err == nil {
		return nil
	}
	return &loopError{message: err.Error(), kind: FailureRetryableAfterResume}
}

func mustMarshalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func stringFromAnyDefault(value any) string {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return ""
	}
	return strings.TrimSpace(text)
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stringPtr(value string) *string { return &value }

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
