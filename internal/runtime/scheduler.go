package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/bootstrap"
	"github.com/MumuTW/looper/internal/config"
	coordinatorrole "github.com/MumuTW/looper/internal/coordinator"
	"github.com/MumuTW/looper/internal/disclosure"
	"github.com/MumuTW/looper/internal/fixer"
	"github.com/MumuTW/looper/internal/forge"
	"github.com/MumuTW/looper/internal/gatekeeper"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/notify"
	networkclient "github.com/MumuTW/looper/internal/network/client"
	"github.com/MumuTW/looper/internal/network/protocol"
	"github.com/MumuTW/looper/internal/networkpolicy"
	"github.com/MumuTW/looper/internal/planner"
	"github.com/MumuTW/looper/internal/projects"
	"github.com/MumuTW/looper/internal/reviewer"
	"github.com/MumuTW/looper/internal/reviewsubmit"
	"github.com/MumuTW/looper/internal/storage"
	"github.com/MumuTW/looper/internal/triager"
	"github.com/MumuTW/looper/internal/version"
	"github.com/MumuTW/looper/internal/webhookforward"
	"github.com/MumuTW/looper/internal/worker"
)

type plannerScheduler interface {
	DiscoverIssues(context.Context, planner.DiscoveryInput) (planner.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*planner.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*planner.ProcessResult, error)
}

type triagerScheduler interface {
	DiscoverIssues(context.Context, triager.DiscoveryInput) (triager.DiscoveryResult, error)
}

type coordinatorScheduler interface {
	DiscoverIssues(context.Context, coordinatorrole.DiscoveryInput) (coordinatorrole.DiscoveryResult, error)
}

type reviewerScheduler interface {
	DiscoverPullRequests(context.Context, reviewer.DiscoveryInput) (reviewer.DiscoveryResult, error)
	DiscoverPullRequest(context.Context, reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*reviewer.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*reviewer.ProcessResult, error)
}

type fixerScheduler interface {
	DiscoverPullRequests(context.Context, fixer.DiscoveryInput) (fixer.DiscoveryResult, error)
	DiscoverPullRequest(context.Context, fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error)
	DiscoverPullRequestsForBaseBranchUpdate(context.Context, fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error)
	ProcessNext(context.Context, string) (*fixer.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*fixer.ProcessResult, error)
}

type gatekeeperScheduler interface {
	DiscoverPullRequests(context.Context, gatekeeper.DiscoveryInput) (gatekeeper.DiscoveryResult, error)
	EvaluatePullRequest(context.Context, gatekeeper.EvaluationInput) (gatekeeper.Report, error)
}

type workerScheduler interface {
	ProcessNext(context.Context, string) (*worker.ProcessResult, error)
	ProcessClaimedQueueItem(context.Context, storage.QueueItemRecord) (*worker.ProcessResult, error)
}

type snapshotScheduler interface {
	CapturePullRequestSnapshot(context.Context, githubinfra.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error)
}

type workerIssueDiscoveryScheduler interface {
	DiscoverIssues(context.Context, worker.DiscoveryInput) (worker.DiscoveryResult, error)
}

type schedulerAsyncRunner interface {
	Go(func())
}

type defaultSchedulerTickInput struct {
	Repos             *storage.Repositories
	GitHubGateway     *githubinfra.Gateway
	GitGateway        *gitinfra.Gateway
	Logger            bootstrap.Logger
	Now               func() time.Time
	MaxConcurrentRuns int
	ClaimMu           *sync.Mutex
	ClaimBoundary     *sync.RWMutex
	RefreshForClaim   func() defaultSchedulerTickInput
	// AllowClaim, when set, is the admission projection for all work-producing
	// scheduler activity: the full default tick (discovery, HITL, claims,
	// stale-reconcile) and each durable ClaimNext*. Nil means ungated (tests).
	AllowClaim func() error
	// OperationOwner, when set, admits a Supervisor operation lease before each
	// durable ClaimNext* and holds it until durable complete/cancel/requeue
	// (ADR-0015 R6 / #579). Nil means ungated claim ownership (unit tests).
	OperationOwner           *ActiveExecutionRegistry
	ReconcileStaleRuns       func(context.Context) (StaleRunReconcileSummary, error)
	AsyncRunner              schedulerAsyncRunner
	RequestSchedulerWake     func()
	Triager                  triagerScheduler
	Planner                  plannerScheduler
	Coordinator              coordinatorScheduler
	Reviewer                 reviewerScheduler
	Fixer                    fixerScheduler
	Gatekeeper               gatekeeperScheduler
	Worker                   workerScheduler
	Snapshotter              snapshotScheduler
	Config                   *config.Config
	PlannerDiscoveryEnabled  *bool
	TriagerEnabled           func(string) bool
	CoordinatorEnabled       func(string) bool
	ReviewerDiscoveryEnabled *bool
	FixerDiscoveryEnabled    *bool
	WorkerDiscoveryEnabled   *bool
	// OnHITLAsk is the exact transport callback captured by the snapshot's
	// worker runner. Keeping it beside the answer callback makes the snapshot
	// ownership boundary explicit and testable end to end.
	OnHITLAsk worker.HITLNotifyFunc
	// OnHITLAnswerDelivered, when set, is called after a Feishu HITL answer is
	// delivered to a loop, so the transport can mark the ask card resolved.
	OnHITLAnswerDelivered func(context.Context, string, string)
	// OnDeployFinished, when set, reports a completed deploy so the person who
	// asked for the change learns it shipped.
	OnDeployFinished func(context.Context, DeployNotification)
}

type defaultSchedulerHandlers struct {
	tick                 RunSchedulerTickFunc
	claim                RunSchedulerTickFunc
	webhook              WebhookForwarder
	reviewer             reviewerScheduler
	fixer                fixerScheduler
	gatekeeper           gatekeeperScheduler
	input                func(Services) defaultSchedulerTickInput
	snapshot             func() defaultSchedulerHandlers
	notificationGateways *schedulerNotificationGatewayFactory
}

type catalogWebhookReviewer struct {
	snapshot func() reviewerScheduler
}

func (r catalogWebhookReviewer) DiscoverPullRequest(ctx context.Context, input reviewer.TargetedDiscoveryInput) (reviewer.DiscoveryResult, error) {
	if r.snapshot == nil {
		return reviewer.DiscoveryResult{}, fmt.Errorf("reviewer is not configured")
	}
	reviewerRunner := r.snapshot()
	if reviewerRunner == nil {
		return reviewer.DiscoveryResult{}, fmt.Errorf("reviewer agent is not configured")
	}
	return reviewerRunner.DiscoverPullRequest(ctx, input)
}

type catalogWebhookFixer struct {
	snapshot func() fixerScheduler
}

type catalogWebhookGatekeeper struct {
	snapshot func() gatekeeperScheduler
}

func (g catalogWebhookGatekeeper) EvaluatePullRequest(ctx context.Context, input gatekeeper.EvaluationInput) (gatekeeper.Report, error) {
	if g.snapshot == nil {
		return gatekeeper.Report{}, fmt.Errorf("gatekeeper is not configured")
	}
	runner := g.snapshot()
	if runner == nil {
		return gatekeeper.Report{}, fmt.Errorf("gatekeeper is not configured")
	}
	return runner.EvaluatePullRequest(ctx, input)
}

func (f catalogWebhookFixer) DiscoverPullRequest(ctx context.Context, input fixer.TargetedDiscoveryInput) (fixer.DiscoveryResult, error) {
	if f.snapshot == nil {
		return fixer.DiscoveryResult{}, fmt.Errorf("fixer is not configured")
	}
	fixerRunner := f.snapshot()
	if fixerRunner == nil {
		return fixer.DiscoveryResult{}, fmt.Errorf("fixer agent is not configured")
	}
	return fixerRunner.DiscoverPullRequest(ctx, input)
}

func (f catalogWebhookFixer) DiscoverPullRequestsForBaseBranchUpdate(ctx context.Context, input fixer.BaseBranchDiscoveryInput) (fixer.DiscoveryResult, error) {
	if f.snapshot == nil {
		return fixer.DiscoveryResult{}, fmt.Errorf("fixer is not configured")
	}
	fixerRunner := f.snapshot()
	if fixerRunner == nil {
		return fixer.DiscoveryResult{}, fmt.Errorf("fixer agent is not configured")
	}
	return fixerRunner.DiscoverPullRequestsForBaseBranchUpdate(ctx, input)
}

type schedulerTaskTracker struct{ wg sync.WaitGroup }

func (t *schedulerTaskTracker) Go(fn func()) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		fn()
	}()
}

func (t *schedulerTaskTracker) Wait() {
	t.wg.Wait()
}

type agentExecutionNotificationInput struct {
	ExecutionID string
	ProjectID   string
	LoopID      string
	RunID       string
	Title       string
	Subtitle    string
	Body        string
	DedupeKey   string
}

type workerRunCompletedNotificationInput struct {
	ProjectID         string
	LoopID            string
	RunID             string
	Subtitle          string
	Status            string
	Summary           string
	FailureKind       worker.QueueFailureKind
	PullRequestNumber int64
	PullRequestURL    string
}

type coordinatorNetworkGateway struct {
	statePath string
	client    *http.Client
}

func (g coordinatorNetworkGateway) Status(ctx context.Context) (protocol.NodeStatusResponse, error) {
	api, err := g.api()
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	return api.Status(ctx)
}

func (g coordinatorNetworkGateway) RevalidateLease(ctx context.Context, req protocol.CoordinatorLeaseRevalidateRequest) error {
	api, err := g.api()
	if err != nil {
		return err
	}
	return api.RevalidateLease(ctx, req)
}

func (g coordinatorNetworkGateway) api() (*networkclient.Client, error) {
	state, err := networkclient.LoadState(g.statePath)
	if err != nil {
		return nil, err
	}
	return networkclient.New(state.URL, state.NodeToken, g.client), nil
}

type plannerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

// providerTrustedEnv collects configured provider tokenEnv values from the
// daemon process environment so the review-submit proxy can inject them into
// its trusted child without exposing secrets to agent envs.
func providerTrustedEnv(cfg config.Config) map[string]string {
	env := map[string]string{}
	for _, provider := range cfg.Providers {
		if provider.TokenEnv == nil {
			continue
		}
		key := strings.TrimSpace(*provider.TokenEnv)
		if key == "" {
			continue
		}
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env[key] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// trustedReviewChildEnv captures the environment that a run-bound review-submit
// child needs. Agent.Env is already visible to that run's agent process, so
// forwarding the same captured values to the trusted child does not widen agent
// access and preserves config-only GitHub auth such as GH_TOKEN. Provider token
// values sourced from the daemon environment win on collisions and remain
// available only to the trusted child.
func trustedReviewChildEnv(cfg config.Config) map[string]string {
	env := make(map[string]string, len(cfg.Agent.Env))
	for key, value := range cfg.Agent.Env {
		env[key] = value
	}
	for key, value := range providerTrustedEnv(cfg) {
		env[key] = value
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// resolveTrustedLooperCLIPath returns the agent-facing looper CLI path, or ""
// when the configured binary cannot serve the trusted review-submit capability.
// tools.looperPath is auto-detected from PATH, so it can name a looper build
// that predates `review submit`; returning "" routes the reviewer prompt to its
// fail-closed branch instead of letting a full review run and only fail at
// publication time.
//
// Agents always receive the real configured looper path. Provider tokens for
// `looper review submit` are supplied through
// a per-run daemon-side trusted review proxy socket bound to the selected PR.
func resolveTrustedLooperCLIPath(cfg config.Config, logger bootstrap.Logger) string {
	configured := strings.TrimSpace(derefString(cfg.Tools.LooperPath))
	if configured == "" {
		return ""
	}
	capable, reason, changed := trustedReviewCapability(configured)
	if changed {
		logTrustedReviewCapabilityVerdict(logger, configured, capable, reason)
	}
	if !capable {
		return ""
	}
	return configured
}

// mintTrustedReviewProxyForPR starts a daemon-side Unix socket that invokes the
// in-process review submitter with captured credentials, bound exclusively to
// allowedPRRef (owner/repo#N), allowedCwd (daemon-selected worktree), the
// materialized run config snapshot, and the daemon-selected review-events
// policy. Agents only receive the socket path (not tokens or snapshot path).
// trustedEnv may be empty when ambient credential stores suffice. Setup fails
// closed: callers must not fall back to direct review submit. cleanup stops the
// listener and must run when the agent execution ends.
func mintTrustedReviewProxyForPR(realLooper string, trustedEnv map[string]string, allowedPRRef, allowedCwd string, configSnapshot config.Config, policy forge.TrustedReviewProxyPolicy) (sockPath string, cleanup func(), err error) {
	realLooper = strings.TrimSpace(realLooper)
	allowedPRRef = strings.TrimSpace(allowedPRRef)
	allowedCwd = strings.TrimSpace(allowedCwd)
	if realLooper == "" {
		return "", nil, fmt.Errorf("trusted looper path is required")
	}
	if allowedPRRef == "" {
		return "", nil, fmt.Errorf("daemon-selected pull request is required")
	}
	if allowedCwd == "" {
		return "", nil, fmt.Errorf("daemon-selected working directory is required")
	}
	resolvedLooper, err := exec.LookPath(realLooper)
	if err != nil {
		return "", nil, fmt.Errorf("resolve trusted looper path %q: %w", realLooper, err)
	}
	if _, err := filepath.Abs(resolvedLooper); err != nil {
		return "", nil, fmt.Errorf("make trusted looper path absolute: %w", err)
	}
	return forge.StartTrustedReviewProxy(runTrustedReviewSubmission, trustedEnv, allowedPRRef, allowedCwd, configSnapshot, policy)
}

func runTrustedReviewSubmission(ctx context.Context, input forge.TrustedReviewSubmission, stdout, stderr io.Writer) error {
	return reviewsubmit.RunTrusted(ctx, reviewsubmit.Options{
		PRRef:               input.PRRef,
		Event:               input.Event,
		CommitID:            input.CommitID,
		CleanReviewEvent:    input.CleanReviewEvent,
		BlockingReviewEvent: input.BlockingReviewEvent,
		ReviewerManual:      input.ReviewerManual,
		ReviewerRunID:       input.ReviewerRunID,
		CWD:                 input.CWD,
		Stdin:               bytes.NewReader(input.Stdin),
		Stdout:              stdout,
		Stderr:              stderr,
	}, input.Config, input.CredentialEnv)
}

func (a plannerGitHubAdapter) ListOpenIssues(ctx context.Context, input planner.ListOpenIssuesInput) ([]planner.IssueSummary, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	issues, err := a.gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label, Labels: input.Labels, Search: input.Search})
	if err != nil {
		return nil, err
	}
	result := make([]planner.IssueSummary, 0, len(issues))
	for _, issue := range issues {
		result = append(result, planner.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Author: issue.Author, Assignees: issue.Assignees, Labels: issue.Labels})
	}
	return result, nil
}

func (a plannerGitHubAdapter) ViewIssue(ctx context.Context, input planner.ViewIssueInput) (planner.IssueDetail, error) {
	if a.gateway == nil {
		return planner.IssueDetail{}, fmt.Errorf("github gateway is not configured")
	}
	issue, err := a.gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return planner.IssueDetail{}, err
	}
	return planner.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Assignees: issue.Assignees, Labels: issue.Labels}, nil
}

func (a plannerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, repo, cwd string) (string, error) {
	return roleCurrentUserLogin(ctx, a.config, a.gateway, repo, cwd)
}

func (a plannerGitHubAdapter) AddIssueAssignees(ctx context.Context, input planner.IssueAssigneesInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Assignees: input.Assignees, CWD: input.CWD})
}

func networkPolicyUsers(users []githubinfra.GitHubUser) []networkpolicy.GitHubUser {
	if users == nil {
		return nil
	}
	converted := make([]networkpolicy.GitHubUser, 0, len(users))
	for _, user := range users {
		converted = append(converted, networkpolicy.GitHubUser{Login: user.Login, ID: user.ID})
	}
	return converted
}

func (a plannerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input planner.ListOpenPullRequestsInput) ([]planner.PullRequestSummary, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]planner.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, planner.PullRequestSummary{Number: pr.Number, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName})
	}
	return result, nil
}

func (a plannerGitHubAdapter) ViewPullRequest(ctx context.Context, input planner.ViewPullRequestInput) (planner.PullRequestDetail, error) {
	if a.gateway == nil {
		return planner.PullRequestDetail{}, fmt.Errorf("github gateway is not configured")
	}
	pr, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return planner.PullRequestDetail{}, err
	}
	return planner.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, URL: pr.URL, State: pr.State, Labels: append([]string(nil), pr.Labels...), HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName}, nil
}

func (a plannerGitHubAdapter) CreatePullRequest(ctx context.Context, input planner.CreatePullRequestInput) (planner.CreatePullRequestResult, error) {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "planner", disclosure.ChannelPullRequest)
	if a.gateway == nil {
		return planner.CreatePullRequestResult{}, fmt.Errorf("github gateway is not configured")
	}
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, CWD: input.CWD})
	if err != nil {
		return planner.CreatePullRequestResult{}, err
	}
	return planner.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
}

func (a plannerGitHubAdapter) UpdatePullRequestBody(ctx context.Context, input planner.UpdatePullRequestBodyInput) error {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "planner", disclosure.ChannelPullRequest)
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdatePullRequestBody(ctx, githubinfra.UpdatePullRequestBodyInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: body, CWD: input.CWD})
}

func (a plannerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input planner.PullRequestLabelsInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a plannerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input planner.PullRequestReviewersInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestReviewers(ctx, githubinfra.PullRequestReviewersInput{Repo: input.Repo, PRNumber: input.PRNumber, Reviewers: input.Reviewers, CWD: input.CWD})
}

type plannerGitAdapter struct {
	gateway *gitinfra.Gateway
	stamper disclosure.Stamper
}

func (a plannerGitAdapter) CreateWorktree(ctx context.Context, input planner.CreateWorktreeInput) (planner.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, ProtectedBranches: input.ProtectedBranches})
	if err != nil {
		return planner.CreateWorktreeResult{}, err
	}
	return planner.CreateWorktreeResult{ID: worktree.ID, WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, BaseBranch: derefString(worktree.BaseBranch)}, nil
}

func (a plannerGitAdapter) InspectHead(ctx context.Context, input planner.InspectHeadInput) (planner.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return planner.InspectHeadResult{}, err
	}
	return planner.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a plannerGitAdapter) Commit(ctx context.Context, input planner.CommitInput) (planner.CommitResult, error) {
	message := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).CommitMessage(input.Message, "planner")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return planner.CommitResult{}, err
	}
	return planner.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a plannerGitAdapter) Push(ctx context.Context, input planner.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ProtectedBranches: input.ProtectedBranches})
}

type plannerAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type plannerAgentExecutionAdapter struct{ execution agent.Execution }

func (a plannerAgentExecutorAdapter) Start(ctx context.Context, input planner.AgentRunInput) (planner.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{
		ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID,
		Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout,
		Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey,
		UseSnapshot: input.UseSnapshot, SnapshotVendor: input.SnapshotVendor, SnapshotModel: input.SnapshotModel,
	})
	if err != nil {
		return nil, err
	}
	return plannerAgentExecutionAdapter{execution: execution}, nil
}

func (a plannerAgentExecutionAdapter) Wait(ctx context.Context) (planner.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return planner.AgentResult{}, err
	}
	return planner.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, Commits: result.Commits, Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

type reviewerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

func (a reviewerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input reviewer.ListOpenPullRequestsInput) ([]reviewer.PullRequestSummary, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Label: input.Label, Labels: input.Labels})
	if err != nil {
		return nil, err
	}
	result := make([]reviewer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, reviewer.PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: pr.Labels, HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, HasConflicts: pr.HasConflicts, Author: pr.Author, ReviewRequests: pr.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(pr.ReviewRequestUsers), Reviews: pr.Reviews})
	}
	return result, nil
}

func (a reviewerGitHubAdapter) ListReviewRequestedPullRequests(ctx context.Context, input reviewer.ListReviewRequestedPullRequestsInput) ([]reviewer.PullRequestSummary, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListReviewRequestedPullRequests(ctx, githubinfra.ListReviewRequestedPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Reviewer: input.Reviewer})
	if err != nil {
		return nil, err
	}
	result := make([]reviewer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, reviewer.PullRequestSummary{Number: pr.Number, Title: pr.Title, State: pr.State, IsDraft: pr.IsDraft, ReviewDecision: pr.ReviewDecision, Labels: pr.Labels, HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, HasConflicts: pr.HasConflicts, Author: pr.Author, ReviewRequests: pr.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(pr.ReviewRequestUsers), Reviews: pr.Reviews})
	}
	return result, nil
}

func (a reviewerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, repo, cwd string) (string, error) {
	return roleCurrentUserLogin(ctx, a.config, a.gateway, repo, cwd)
}

func (a reviewerGitHubAdapter) ViewPullRequest(ctx context.Context, input reviewer.ViewPullRequestInput) (reviewer.PullRequestDetail, error) {
	if a.gateway == nil {
		return reviewer.PullRequestDetail{}, fmt.Errorf("github gateway is not configured")
	}
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return reviewer.PullRequestDetail{}, err
	}
	detail, err := a.gateway.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return reviewer.PullRequestDetail{}, err
	}
	return reviewer.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, State: detail.State, IsDraft: detail.IsDraft, ReviewDecision: detail.ReviewDecision, Labels: detail.Labels, HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, Author: detail.Author, ReviewRequests: detail.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(detail.ReviewRequestUsers), HasConflicts: detail.HasConflicts, ChecksSummary: summarizeCheckStates(detail.Checks), Comments: detail.Comments, IssueComments: commentInfosToObjects(detail.IssueComments), Reviews: detail.Reviews}, nil
}

func (a reviewerGitHubAdapter) ViewIssue(ctx context.Context, input githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error) {
	if a.gateway == nil {
		return githubinfra.IssueDetail{}, fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.ViewIssue(ctx, input)
}

func (a reviewerGitHubAdapter) GetRepositorySettings(ctx context.Context, input githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error) {
	return a.gateway.GetRepositorySettings(ctx, input)
}

func (a reviewerGitHubAdapter) GetBranchProtection(ctx context.Context, input githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error) {
	return a.gateway.GetBranchProtection(ctx, input)
}

func (a reviewerGitHubAdapter) GetPullRequestHeadSHA(ctx context.Context, input reviewer.ViewPullRequestInput) (string, error) {
	if a.gateway == nil {
		return "", fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.GetPullRequestHeadSHA(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) CapturePullRequestSnapshot(ctx context.Context, input reviewer.CapturePullRequestSnapshotInput) (storage.PullRequestSnapshotRecord, error) {
	if a.gateway == nil {
		return storage.PullRequestSnapshotRecord{}, fmt.Errorf("github gateway is not configured")
	}
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return storage.PullRequestSnapshotRecord{}, err
	}
	return a.gateway.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: input.ProjectID, Repo: input.Repo, TransportRepo: repo, PRNumber: input.PRNumber, CWD: input.CWD, CapturedAt: input.CapturedAt})
}

func (a reviewerGitHubAdapter) FindReviewMarker(ctx context.Context, input reviewer.VerifyReviewMarkerInput) (reviewer.ReviewMarkerResult, error) {
	if a.gateway == nil {
		return reviewer.ReviewMarkerResult{}, fmt.Errorf("github gateway is not configured")
	}
	allowedReviewEvents := make([]string, 0, len(input.AllowedReviewEvents))
	for _, event := range input.AllowedReviewEvents {
		allowedReviewEvents = append(allowedReviewEvents, string(event))
	}
	marker, err := a.gateway.FindReviewMarker(ctx, githubinfra.VerifyReviewMarkerInput{Repo: input.Repo, PRNumber: input.PRNumber, Marker: input.Marker, AllowedReviewEvents: allowedReviewEvents, AuthorLogin: input.AuthorLogin, AllowCleanComment: input.AllowCleanComment, CWD: input.CWD})
	if err != nil {
		return reviewer.ReviewMarkerResult{}, err
	}
	return reviewer.ReviewMarkerResult{Found: marker.Found, Outcome: marker.Outcome, Event: reviewer.ReviewEvent(marker.Event), AuthorLogin: marker.AuthorLogin, Body: marker.Body, InlineCommentBodies: append([]string(nil), marker.InlineCommentBodies...)}, nil
}

func (a reviewerGitHubAdapter) CreateIssueComment(ctx context.Context, input reviewer.IssueCommentInput) (reviewer.IssueCommentResult, error) {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "reviewer", disclosure.ChannelIssueComment)
	if a.gateway == nil {
		return reviewer.IssueCommentResult{}, fmt.Errorf("github gateway is not configured")
	}
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return reviewer.IssueCommentResult{}, err
	}
	return reviewer.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a reviewerGitHubAdapter) ListIssueComments(ctx context.Context, input reviewer.ViewPullRequestInput) ([]reviewer.IssueComment, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	comments, err := a.gateway.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return nil, err
	}
	out := make([]reviewer.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, reviewer.IssueComment{ID: comment.ID, Body: comment.Body})
	}
	return out, nil
}

func (a reviewerGitHubAdapter) UpdateIssueComment(ctx context.Context, input reviewer.UpdateIssueCommentInput) error {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "reviewer", disclosure.ChannelIssueComment)
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) SubmitReview(ctx context.Context, input githubinfra.SubmitReviewInput) error {
	stamper := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel)
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	// Stamp with run snapshot identity before GitHub submit.
	input.Body = stamper.Markdown(input.Body, "reviewer", disclosure.ChannelReviewComment)
	if len(input.Comments) > 0 {
		comments := make([]githubinfra.ReviewComment, len(input.Comments))
		copy(comments, input.Comments)
		for i := range comments {
			comments[i].Body = stamper.ReviewComment(comments[i].Body, "reviewer")
		}
		input.Comments = comments
	}
	return a.gateway.SubmitReview(ctx, input)
}

func (a reviewerGitHubAdapter) EnableAutoMerge(ctx context.Context, input githubinfra.EnableAutoMergeInput) error {
	return a.gateway.EnableAutoMerge(ctx, input)
}

func (a reviewerGitHubAdapter) AddPullRequestReaction(ctx context.Context, input reviewer.PullRequestReactionInput) error {
	return a.gateway.AddPullRequestReaction(ctx, githubinfra.PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: input.Content, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) RemovePullRequestReaction(ctx context.Context, input reviewer.PullRequestReactionInput) error {
	return a.gateway.RemovePullRequestReaction(ctx, githubinfra.PullRequestReactionInput{Repo: input.Repo, PRNumber: input.PRNumber, Content: input.Content, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input reviewer.PullRequestLabelsInput) error {
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input reviewer.PullRequestLabelsInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) RemoveIssueLabels(ctx context.Context, input githubinfra.IssueLabelsInput) error {
	return a.gateway.RemoveIssueLabels(ctx, input)
}

func (a reviewerGitHubAdapter) ListReviewThreads(ctx context.Context, input reviewer.ListReviewThreadsInput) ([]reviewer.ReviewThread, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return nil, err
	}
	threads, err := a.gateway.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: repo, PRNumber: input.PRNumber, CWD: input.CWD, Limit: input.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]reviewer.ReviewThread, 0, len(threads))
	for _, thread := range threads {
		converted := reviewer.ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved, Path: thread.Path, Line: thread.Line, URL: thread.URL}
		for _, comment := range thread.Comments {
			converted.Comments = append(converted.Comments, reviewer.ReviewThreadComment{ID: comment.ID, Body: comment.Body, Author: comment.Author, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt, Path: comment.Path, Line: comment.Line, OriginalCommitOID: comment.OriginalCommitOID, CommitOID: comment.CommitOID, URL: comment.URL})
		}
		out = append(out, converted)
	}
	return out, nil
}

func (a reviewerGitHubAdapter) AddReviewThreadReply(ctx context.Context, input reviewer.AddReviewThreadReplyInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return err
	}
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).ReviewComment(input.Body, "reviewer")
	return a.gateway.AddReviewThreadReply(ctx, githubinfra.AddReviewThreadReplyInput{Repo: repo, ThreadID: input.ThreadID, Body: body, CWD: input.CWD})
}

func (a reviewerGitHubAdapter) ResolveReviewThread(ctx context.Context, input reviewer.ResolveReviewThreadInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return err
	}
	return a.gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: repo, ThreadID: input.ThreadID, CWD: input.CWD})
}

type reviewerAgentExecutorAdapter struct {
	executor   *agent.ConfiguredExecutor
	realLooper string
	trustedEnv map[string]string
	// config is used to gate trusted review-submit sockets on per-project
	// publish mode (summary_comment must not mint a native review socket).
	config *config.Config
	// agentVendor/agentModel are the role-resolved reviewer identity captured
	// when handlers are built. Trusted review proxy config materializes the
	// run snapshot when present, otherwise these values, so review_submit's
	// disclosure.FromConfig stamps agent=/model= from execution identity
	// rather than the daemon's global agent.vendor.
	agentVendor config.AgentVendor
	agentModel  *string
}
type reviewerAgentExecutionAdapter struct {
	execution agent.Execution
	cleanup   func()
	once      sync.Once
}

func (a *reviewerAgentExecutionAdapter) closeProxy() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if a.cleanup != nil {
			a.cleanup()
		}
	})
}

type reviewerGitAdapter struct{ gateway *gitinfra.Gateway }

func (a reviewerGitAdapter) CreateWorktree(ctx context.Context, input reviewer.CreateWorktreeInput) (reviewer.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber, ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode)})
	if err != nil {
		return reviewer.CreateWorktreeResult{}, err
	}
	return reviewer.CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, HeadSHA: derefString(worktree.HeadSHA)}, nil
}

func (a reviewerGitAdapter) PrepareWorktree(ctx context.Context, input reviewer.PrepareWorktreeInput) (reviewer.PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Ref: input.Ref, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return reviewer.PrepareWorktreeResult{}, err
	}
	return reviewer.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a reviewerGitAdapter) CleanupWorktree(ctx context.Context, input reviewer.CleanupWorktreeInput) error {
	return a.gateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches})
}

func (a reviewerGitAdapter) ScrubReservedReviewerScratch(ctx context.Context, worktreePath string) error {
	return a.gateway.ScrubReservedReviewerScratch(ctx, worktreePath)
}

// reviewerTrustedReviewEnv injects the trusted review-submit socket only for
// reviewer agent runs. Planner/worker/fixer share the executor but must not
// receive review publication capability via LOOPER_TRUSTED_REVIEW_SOCK.
func reviewerTrustedReviewEnv(sock string) map[string]string {
	sock = strings.TrimSpace(sock)
	if sock == "" {
		return nil
	}
	return map[string]string{forge.TrustedReviewSockEnv: sock}
}

// reviewerAllowsTrustedReviewProxy reports whether this reviewer agent start is
// authorized to receive a live review-submit socket. Thread-resolution
// classifiers share the reviewer adapter but must not receive publish capability.
// Every native review/publish run needs the socket so its review-submit child
// uses the immutable run config. Summary-comment mode must not mint a socket
// because Looper posts that top-level comment outside agent review submit.
func reviewerAllowsTrustedReviewProxy(cfg *config.Config, projectID string, metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	phase, _ := metadata["phase"].(string)
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "review", "publish":
		// continue
	default:
		return false
	}
	return reviewerProjectNeedsTrustedProxy(cfg, projectID)
}

// reviewerProjectNeedsTrustedProxy reports whether the selected project
// publishes native reviews.
func reviewerProjectNeedsTrustedProxy(cfg *config.Config, projectID string) bool {
	if cfg == nil {
		return false
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return false
	}
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.ID) != projectID {
			continue
		}
		return true
	}
	return false
}

// reviewerAllowedPRRef extracts the daemon-selected owner/repo#N from reviewer
// agent metadata so the trusted review proxy can be bound to that PR only.
func reviewerAllowedPRRef(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	repo, _ := metadata["repo"].(string)
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	prNumber, ok := metadataInt64(metadata["prNumber"])
	if !ok || prNumber <= 0 {
		return ""
	}
	return forge.FormatTrustedReviewPRRef(repo, prNumber)
}

// reviewerAllowedReviewPolicy extracts daemon-selected review policy, head,
// and run identity from reviewer metadata. The runner authors these fields from
// its immutable loop/run checkpoint; agent argv is never authoritative.
func reviewerAllowedReviewPolicy(metadata map[string]any) forge.TrustedReviewProxyPolicy {
	if metadata == nil {
		return forge.TrustedReviewProxyPolicy{}
	}
	clean, _ := metadata["cleanReviewEvent"].(string)
	blocking, _ := metadata["blockingReviewEvent"].(string)
	expectedCommitID, _ := metadata["expectedCommitID"].(string)
	reviewerManual, _ := metadata["reviewerManual"].(bool)
	reviewerRunID, _ := metadata["reviewerRunID"].(string)
	return forge.TrustedReviewProxyPolicy{
		Clean:            strings.TrimSpace(clean),
		Blocking:         strings.TrimSpace(blocking),
		ExpectedCommitID: strings.TrimSpace(expectedCommitID),
		ReviewerManual:   reviewerManual,
		ReviewerRunID:    strings.TrimSpace(reviewerRunID),
	}
}

func metadataInt64(value any) (int64, bool) {
	switch n := value.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case float32:
		if n != float32(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		parsed, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// reviewerTrustedReviewAgentIdentity returns vendor/model for the trusted
// review-submit config. Run snapshot fields are authority when
// UseSnapshot is set with a non-empty vendor; otherwise the role-resolved
// adapter identity is used.
func reviewerTrustedReviewAgentIdentity(input reviewer.AgentRunInput, fallbackVendor config.AgentVendor, fallbackModel *string) (config.AgentVendor, *string) {
	if input.UseSnapshot {
		if vendor := strings.TrimSpace(input.SnapshotVendor); vendor != "" {
			return config.AgentVendor(vendor), input.SnapshotModel
		}
	}
	return fallbackVendor, fallbackModel
}

// materializeTrustedReviewAgentIdentity copies cfg and overwrites
// Agent.Vendor/Model so disclosure.FromConfig in trusted review submission
// matches the reviewer execution identity.
func materializeTrustedReviewAgentIdentity(cfg config.Config, vendor config.AgentVendor, model *string) config.Config {
	if strings.TrimSpace(string(vendor)) != "" {
		v := vendor
		cfg.Agent.Vendor = &v
	} else {
		cfg.Agent.Vendor = nil
	}
	if model != nil {
		m := *model
		cfg.Agent.Model = &m
	} else {
		cfg.Agent.Model = nil
	}
	return cfg
}

func (a reviewerAgentExecutorAdapter) Start(ctx context.Context, input reviewer.AgentRunInput) (reviewer.AgentExecution, error) {
	// Mint a per-run proxy only for review/publish phases on projects that
	// publish native reviews, bound to the daemon-selected PR, worktree CWD,
	// and review-events policy. Thread-resolution classifiers and summary_comment
	// (comment-only) runs reuse this adapter but must not receive review-publish
	// capability via LOOPER_TRUSTED_REVIEW_SOCK.
	sock := ""
	proxyCleanup := func() {}
	if reviewerAllowsTrustedReviewProxy(a.config, input.ProjectID, input.Metadata) {
		allowedPR := reviewerAllowedPRRef(input.Metadata)
		allowedCwd := strings.TrimSpace(input.WorkingDirectory)
		policy := reviewerAllowedReviewPolicy(input.Metadata)
		if policy.ReviewerManual && policy.ReviewerRunID != strings.TrimSpace(input.RunID) {
			return nil, fmt.Errorf("install run-bound trusted review proxy: reviewer run id does not match agent run")
		}
		// Refuse before the agent starts rather than spend a run reviewing a PR
		// it has no way to publish to. resolveTrustedLooperCLIPath has already
		// logged which binary failed and why; this is the per-run consequence.
		//
		// The text is the prompt's wording verbatim, deliberately without the
		// "install run-bound trusted review proxy" prefix its neighbours carry.
		// Runs that never mint a proxy report the same condition from the agent
		// side instead, and an operator should not need to know which half they
		// are looking at to search for it.
		if strings.TrimSpace(a.realLooper) == "" {
			return nil, errors.New(reviewer.TrustedReviewCapabilityUnavailableMessage)
		}
		vendor, model := reviewerTrustedReviewAgentIdentity(input, a.agentVendor, a.agentModel)
		configSnapshot := materializeTrustedReviewAgentIdentity(*a.config, vendor, model)
		var err error
		sock, proxyCleanup, err = mintTrustedReviewProxyForPR(a.realLooper, a.trustedEnv, allowedPR, allowedCwd, configSnapshot, policy)
		// Past the wrapper check a failure is the daemon's own — socket, binding
		// or policy — and keeps the prefix, because it is not the condition the
		// prompt describes and must not be searched for as if it were.
		if err != nil {
			return nil, fmt.Errorf("install run-bound trusted review proxy: %w", err)
		}
	}
	execution, err := a.executor.Start(ctx, agent.RunInput{
		ExecutionID:        input.ExecutionID,
		ProjectID:          input.ProjectID,
		LoopID:             input.LoopID,
		RunID:              input.RunID,
		Prompt:             input.Prompt,
		NativeResumePrompt: input.NativeResumePrompt,
		WorkingDirectory:   input.WorkingDirectory,
		Timeout:            input.Timeout,
		HeartbeatTimeout:   input.HeartbeatTimeout,
		Metadata:           input.Metadata,
		IdempotencyKey:     input.IdempotencyKey,
		Env:                reviewerTrustedReviewEnv(sock),
		UseSnapshot:        input.UseSnapshot,
		SnapshotVendor:     input.SnapshotVendor,
		SnapshotModel:      input.SnapshotModel,
	})
	if err != nil {
		proxyCleanup()
		return nil, err
	}
	return &reviewerAgentExecutionAdapter{execution: execution, cleanup: proxyCleanup}, nil
}

func (a *reviewerAgentExecutionAdapter) Wait(ctx context.Context) (reviewer.AgentResult, error) {
	defer a.closeProxy()
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return reviewer.AgentResult{}, err
	}
	return reviewer.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

func (a *reviewerAgentExecutionAdapter) Kill(reason string) error {
	defer a.closeProxy()
	return a.execution.Kill(reason)
}

type fixerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

func (a fixerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input fixer.ListOpenPullRequestsInput) ([]fixer.PullRequestSummary, error) {
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Author: input.Author, Label: input.Label, Labels: input.Labels, BaseRefName: input.BaseRefName})
	if err != nil {
		return nil, err
	}
	result := make([]fixer.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, fixer.PullRequestSummary{Number: pr.Number, State: pr.State, IsDraft: pr.IsDraft, Labels: pr.Labels, BaseRefName: pr.BaseRefName, HeadSHA: pr.HeadSHA, Author: pr.Author, UpdatedAt: pr.UpdatedAt})
	}
	return result, nil
}

func (a fixerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, repo, cwd string) (string, error) {
	return roleCurrentUserLogin(ctx, a.config, a.gateway, repo, cwd)
}

func (a fixerGitHubAdapter) GetPullRequestAuthor(ctx context.Context, input fixer.ViewPullRequestInput) (string, error) {
	return a.gateway.GetPullRequestAuthor(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
}

func (a fixerGitHubAdapter) ViewPullRequest(ctx context.Context, input fixer.ViewPullRequestInput) (fixer.PullRequestDetail, error) {
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return fixer.PullRequestDetail{}, err
	}
	detail, err := a.gateway.ViewPullRequestForFixer(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return fixer.PullRequestDetail{}, err
	}
	return fixer.PullRequestDetail{Number: detail.Number, State: detail.State, IsDraft: detail.IsDraft, Labels: detail.Labels, HeadSHA: detail.HeadSHA, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, BaseSHA: detail.BaseSHA, ReviewDecision: detail.ReviewDecision, Comments: detail.Comments, IssueComments: commentInfosToObjects(detail.IssueComments), Checks: detail.Checks, HasConflicts: detail.HasConflicts, Author: detail.Author}, nil
}

func commentInfosToObjects(items []githubinfra.CommentInfo) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"id":                item.ID,
			"author":            map[string]any{"login": item.Author},
			"authorAssociation": item.AuthorAssociation,
			"body":              item.Body,
			"createdAt":         item.CreatedAt,
			"updatedAt":         item.UpdatedAt,
			"url":               item.URL,
		})
	}
	return out
}

func (a fixerGitHubAdapter) ListReviewThreads(ctx context.Context, input fixer.ListReviewThreadsInput) ([]fixer.ReviewThread, error) {
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return nil, err
	}
	threads, err := a.gateway.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: repo, PRNumber: input.PRNumber, CWD: input.CWD, Limit: input.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]fixer.ReviewThread, 0, len(threads))
	for _, thread := range threads {
		comments := make([]fixer.ReviewThreadComment, 0, len(thread.Comments))
		for _, comment := range thread.Comments {
			comments = append(comments, fixer.ReviewThreadComment{ID: comment.ID, Body: comment.Body, Author: comment.Author, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
		}
		out = append(out, fixer.ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved, Comments: comments})
	}
	return out, nil
}

func (a fixerGitHubAdapter) ViewReviewThread(ctx context.Context, input fixer.ViewReviewThreadInput) (fixer.ReviewThread, error) {
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return fixer.ReviewThread{}, err
	}
	hostname, _ := githubinfra.SplitRepoHostname(repo)
	thread, err := a.gateway.ViewReviewThread(ctx, githubinfra.ViewReviewThreadInput{ThreadID: input.ThreadID, CWD: input.CWD, Hostname: hostname})
	if err != nil {
		return fixer.ReviewThread{}, err
	}
	comments := make([]fixer.ReviewThreadComment, 0, len(thread.Comments))
	for _, comment := range thread.Comments {
		comments = append(comments, fixer.ReviewThreadComment{ID: comment.ID, Body: comment.Body, Author: comment.Author, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
	}
	return fixer.ReviewThread{ID: thread.ID, IsResolved: thread.IsResolved, Comments: comments}, nil
}

func (a fixerGitHubAdapter) ResolveReviewThread(ctx context.Context, input fixer.ResolveReviewThreadInput) error {
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return err
	}
	return a.gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: repo, ThreadID: input.ThreadID, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddReviewThreadReply(ctx context.Context, input fixer.AddReviewThreadReplyInput) error {
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return err
	}
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).ReviewComment(input.Body, "fixer")
	return a.gateway.AddReviewThreadReply(ctx, githubinfra.AddReviewThreadReplyInput{Repo: repo, ThreadID: input.ThreadID, Body: body, CWD: input.CWD})
}

// reviewThreadRepo adds the provider hostname selected by the live checkout.
func reviewThreadRepo(cfg *config.Config, repo, cwd string) (string, error) {
	if cfg == nil {
		return repo, nil
	}
	for _, project := range cfg.Projects {
		if filepath.Clean(project.RepoPath) == filepath.Clean(cwd) {
			return qualifyProjectRepo(*cfg, project, repo)
		}
	}
	return repo, nil
}

// reviewThreadRepoForProject adds the current provider hostname by durable
// project identity. The operation owns its queued slug and CWD; both may
// predate a config-managed path change.
func reviewThreadRepoForProject(cfg *config.Config, projectID, repo string) (string, error) {
	if cfg == nil {
		return repo, nil
	}
	for _, project := range cfg.Projects {
		if project.ID == strings.TrimSpace(projectID) {
			return qualifyProjectRepo(*cfg, project, repo)
		}
	}
	return "", fmt.Errorf("repository provider is not configured for project %s", projectID)
}

func qualifyProjectRepo(cfg config.Config, project config.ProjectRefConfig, repo string) (string, error) {
	_, repoSlug := githubinfra.SplitRepoHostname(repo)
	if strings.TrimSpace(repoSlug) == "" {
		return "", fmt.Errorf("repository identity is not configured for project %s", project.ID)
	}
	project.Repo = repoSlug
	identity, ok := config.ProjectRepositoryIdentity(cfg, project)
	if !ok {
		return "", fmt.Errorf("repository identity is not configured for project %s", project.ID)
	}
	baseURL, err := url.Parse(identity.BaseURL)
	if err != nil || strings.TrimSpace(baseURL.Hostname()) == "" || strings.TrimSpace(baseURL.Host) == "" {
		return "", fmt.Errorf("github provider hostname is not configured for project %s", project.ID)
	}
	if isPublicGitHubHostname(baseURL.Hostname()) {
		return identity.Repo, nil
	}
	return strings.TrimSpace(baseURL.Host) + "/" + identity.Repo, nil
}

func isPublicGitHubHostname(hostname string) bool {
	switch {
	case strings.EqualFold(hostname, "github.com"), strings.EqualFold(hostname, "www.github.com"), strings.EqualFold(hostname, "api.github.com"):
		return true
	default:
		return false
	}
}

// gatekeeperGitHubAdapter supplies the provider-qualified repository identity
// only for review-thread operations. All other gatekeeper calls retain their
// owner/slug repository contract.
type gatekeeperGitHubAdapter struct {
	*githubinfra.Gateway
	config *config.Config
}

func (a gatekeeperGitHubAdapter) ListReviewThreads(ctx context.Context, input githubinfra.ListReviewThreadsInput) ([]githubinfra.ReviewThread, error) {
	repo, err := reviewThreadRepo(a.config, input.Repo, input.CWD)
	if err != nil {
		return nil, err
	}
	input.Repo = repo
	return a.Gateway.ListReviewThreads(ctx, input)
}

func (a fixerGitHubAdapter) CompareCommits(ctx context.Context, input fixer.CompareCommitsInput) (fixer.CompareCommitsResult, error) {
	out, err := a.gateway.CompareCommits(ctx, githubinfra.CompareCommitsInput{Repo: input.Repo, Base: input.Base, Head: input.Head, CWD: input.CWD})
	if err != nil {
		return fixer.CompareCommitsResult{}, err
	}
	return fixer.CompareCommitsResult{Status: out.Status}, nil
}

func (a fixerGitHubAdapter) CreateIssueComment(ctx context.Context, input fixer.IssueCommentInput) (fixer.IssueCommentResult, error) {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "fixer", disclosure.ChannelIssueComment)
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return fixer.IssueCommentResult{}, err
	}
	return fixer.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a fixerGitHubAdapter) UpdateIssueComment(ctx context.Context, input fixer.UpdateIssueCommentInput) error {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "fixer", disclosure.ChannelIssueComment)
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input fixer.PullRequestLabelsInput) error {
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a fixerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input fixer.PullRequestLabelsInput) error {
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a fixerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input fixer.PullRequestReviewersInput) error {
	return a.gateway.AddPullRequestReviewers(ctx, githubinfra.PullRequestReviewersInput{Repo: input.Repo, PRNumber: input.PRNumber, Reviewers: input.Reviewers, CWD: input.CWD})
}

func (a fixerGitHubAdapter) ListPullRequestReviews(ctx context.Context, input fixer.ViewPullRequestInput) ([]fixer.ReviewSummary, error) {
	reviews, err := a.gateway.ListPullRequestReviews(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return nil, err
	}
	out := make([]fixer.ReviewSummary, 0, len(reviews))
	for _, rv := range reviews {
		out = append(out, fixer.ReviewSummary{ID: rv.ID, State: rv.State, Author: rv.Author, Body: rv.Body})
	}
	return out, nil
}

func (a fixerGitHubAdapter) DismissReview(ctx context.Context, input fixer.DismissReviewInput) error {
	return a.gateway.DismissReview(ctx, githubinfra.DismissReviewInput{Repo: input.Repo, PRNumber: input.PRNumber, ReviewID: input.ReviewID, Message: input.Message, CWD: input.CWD})
}

type fixerGitAdapter struct {
	gateway *gitinfra.Gateway
	stamper disclosure.Stamper
}

func (a fixerGitAdapter) CreateWorktree(ctx context.Context, input fixer.CreateWorktreeInput) (fixer.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber, ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode)})
	if err != nil {
		return fixer.CreateWorktreeResult{}, err
	}
	return fixer.CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, HeadSHA: derefString(worktree.HeadSHA)}, nil
}

func (a fixerGitAdapter) PrepareWorktree(ctx context.Context, input fixer.PrepareWorktreeInput) (fixer.PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return fixer.PrepareWorktreeResult{}, err
	}
	return fixer.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a fixerGitAdapter) InspectHead(ctx context.Context, input fixer.InspectHeadInput) (fixer.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return fixer.InspectHeadResult{}, err
	}
	return fixer.InspectHeadResult{HeadSHA: result.HeadSHA, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles}, nil
}

func (a fixerGitAdapter) Commit(ctx context.Context, input fixer.CommitInput) (fixer.CommitResult, error) {
	message := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).CommitMessage(input.Message, "fixer")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return fixer.CommitResult{}, err
	}
	return fixer.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a fixerGitAdapter) Push(ctx context.Context, input fixer.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, ExpectedRemoteHeadSHA: input.ExpectedRemoteHeadSHA, LocalHeadSHA: input.LocalHeadSHA, ProtectedBranches: input.ProtectedBranches})
}

func (a fixerGitAdapter) FetchBranch(ctx context.Context, repoPath, remote, branch string) error {
	return a.gateway.FetchBranch(ctx, repoPath, remote, branch)
}

func (a fixerGitAdapter) MergeBaseIntoWorktree(ctx context.Context, input fixer.MergeBaseInput) (fixer.MergeBaseResult, error) {
	res, err := a.gateway.MergeBaseIntoWorktree(ctx, gitinfra.MergeBaseInput{WorktreePath: input.WorktreePath, Remote: input.Remote, BaseBranch: input.BaseBranch})
	if err != nil {
		return fixer.MergeBaseResult{}, err
	}
	return fixer.MergeBaseResult{AlreadyUpToDate: res.AlreadyUpToDate, Conflicted: res.Conflicted}, nil
}

func (a fixerGitAdapter) IsAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	return a.gateway.IsAncestor(ctx, repoPath, ancestor, descendant)
}

func (a fixerGitAdapter) CleanupWorktree(ctx context.Context, input fixer.CleanupWorktreeInput) error {
	return a.gateway.CleanupWorktree(ctx, gitinfra.CleanupWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ProtectedBranches: input.ProtectedBranches})
}

type fixerAgentExecutorAdapter struct{ executor *agent.ConfiguredExecutor }
type fixerAgentExecutionAdapter struct{ execution agent.Execution }

func (a fixerAgentExecutorAdapter) Start(ctx context.Context, input fixer.AgentRunInput) (fixer.AgentExecution, error) {
	execution, err := a.executor.Start(ctx, agent.RunInput{
		ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID,
		Prompt: input.Prompt, WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout,
		Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey,
		RestrictToolNetwork: input.RestrictToolNetwork,
		UseSnapshot:         input.UseSnapshot, SnapshotVendor: input.SnapshotVendor, SnapshotModel: input.SnapshotModel,
	})
	if err != nil {
		return nil, err
	}
	return fixerAgentExecutionAdapter{execution: execution}, nil
}

func (a fixerAgentExecutionAdapter) Wait(ctx context.Context) (fixer.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return fixer.AgentResult{}, err
	}
	return fixer.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, CompletionPayload: result.CompletionPayload, Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt}, nil
}

type workerGitHubAdapter struct {
	gateway *githubinfra.Gateway
	stamper disclosure.Stamper
	config  *config.Config
}

func (a workerGitHubAdapter) ListOpenPullRequests(ctx context.Context, input worker.ListOpenPullRequestsInput) ([]worker.PullRequestSummary, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	pullRequests, err := a.gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Label: input.Label})
	if err != nil {
		return nil, err
	}
	result := make([]worker.PullRequestSummary, 0, len(pullRequests))
	for _, pr := range pullRequests {
		result = append(result, worker.PullRequestSummary{Number: pr.Number, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, BaseRefName: pr.BaseRefName})
	}
	return result, nil
}

func (a workerGitHubAdapter) ListOpenIssues(ctx context.Context, input worker.ListOpenIssuesInput) ([]worker.IssueSummary, error) {
	if a.gateway == nil {
		return nil, fmt.Errorf("github gateway is not configured")
	}
	issues, err := a.gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: input.CWD, Limit: input.Limit, Assignee: input.Assignee, Label: input.Label, Labels: input.Labels, Search: input.Search})
	if err != nil {
		return nil, err
	}
	result := make([]worker.IssueSummary, 0, len(issues))
	for _, issue := range issues {
		result = append(result, worker.IssueSummary{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, Author: issue.Author, Assignees: issue.Assignees, AssigneeUsers: networkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels})
	}
	return result, nil
}

func (a workerGitHubAdapter) GetCurrentUserLogin(ctx context.Context, repo, cwd string) (string, error) {
	return roleCurrentUserLogin(ctx, a.config, a.gateway, repo, cwd)
}

// roleCurrentUserLogin binds the lookup to the configured project identity.
// CWD selects the project snapshot; its provider base URL, not ambient gh
// state or an agent response, supplies the hostname.
func roleCurrentUserLogin(ctx context.Context, cfg *config.Config, gateway *githubinfra.Gateway, repo, cwd string) (string, error) {
	if gateway == nil {
		return "", fmt.Errorf("github gateway is not configured")
	}
	if cfg != nil {
		for _, project := range cfg.Projects {
			if filepath.Clean(project.RepoPath) != filepath.Clean(cwd) {
				continue
			}
			if strings.TrimSpace(project.Repo) == "" {
				// The catalog can briefly lag repository detection. The configured
				// provider remains host authority; the runtime-selected repo only
				// supplies the missing owner/slug.
				project.Repo = repo
			}
			identity, ok := config.ProjectRepositoryIdentity(*cfg, project)
			if !ok {
				return "", fmt.Errorf("repository identity is not configured for project %s", project.ID)
			}
			baseURL, err := url.Parse(identity.BaseURL)
			if err != nil || strings.TrimSpace(baseURL.Hostname()) == "" {
				return "", fmt.Errorf("github provider hostname is not configured for project %s", project.ID)
			}
			repo = baseURL.Hostname() + "/" + identity.Repo
			break
		}
	}
	return gateway.GetCurrentUserLoginForRepo(ctx, repo, cwd)
}

func (a workerGitHubAdapter) AddIssueAssignees(ctx context.Context, input worker.IssueAssigneesInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Assignees: input.Assignees, CWD: input.CWD})
}

func (a workerGitHubAdapter) ViewPullRequest(ctx context.Context, input worker.ViewPullRequestInput) (worker.PullRequestDetail, error) {
	if a.gateway == nil {
		return worker.PullRequestDetail{}, fmt.Errorf("github gateway is not configured")
	}
	detail, err := a.gateway.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: input.Repo, PRNumber: input.PRNumber, CWD: input.CWD})
	if err != nil {
		return worker.PullRequestDetail{}, err
	}
	return worker.PullRequestDetail{Number: detail.Number, Title: detail.Title, Body: detail.Body, URL: detail.URL, State: detail.State, HeadRefName: detail.HeadRefName, BaseRefName: detail.BaseRefName, HeadSHA: detail.HeadSHA, Labels: append([]string(nil), detail.Labels...), ReviewRequests: detail.ReviewRequests, ReviewRequestUsers: networkPolicyUsers(detail.ReviewRequestUsers)}, nil
}

func (a workerGitHubAdapter) ViewIssue(ctx context.Context, input worker.ViewIssueInput) (worker.IssueDetail, error) {
	if a.gateway == nil {
		return worker.IssueDetail{}, fmt.Errorf("github gateway is not configured")
	}
	issue, err := a.gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: input.IssueNumber, CWD: input.CWD})
	if err != nil {
		return worker.IssueDetail{}, err
	}
	return worker.IssueDetail{Number: issue.Number, Title: issue.Title, Body: issue.Body, URL: issue.URL, State: issue.State, IsPullRequest: issue.IsPullRequest, AssigneeUsers: networkPolicyUsers(issue.AssigneeUsers), Labels: issue.Labels}, nil
}

func (a workerGitHubAdapter) CreateIssueComment(ctx context.Context, input worker.IssueCommentInput) (worker.IssueCommentResult, error) {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	if a.gateway == nil {
		return worker.IssueCommentResult{}, fmt.Errorf("github gateway is not configured")
	}
	comment, err := a.gateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: input.Repo, IssueNumber: input.IssueNumber, Body: body, CWD: input.CWD})
	if err != nil {
		return worker.IssueCommentResult{}, err
	}
	return worker.IssueCommentResult{ID: comment.ID, URL: comment.URL}, nil
}

func (a workerGitHubAdapter) UpdateIssueComment(ctx context.Context, input worker.UpdateIssueCommentInput) error {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelIssueComment)
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: input.Repo, CommentID: input.CommentID, Body: body, CWD: input.CWD})
}

func (a workerGitHubAdapter) CreatePullRequest(ctx context.Context, input worker.CreatePullRequestInput) (worker.CreatePullRequestResult, error) {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	if a.gateway == nil {
		return worker.CreatePullRequestResult{}, fmt.Errorf("github gateway is not configured")
	}
	pr, err := a.gateway.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: input.Repo, HeadBranch: input.HeadBranch, BaseBranch: input.BaseBranch, Title: input.Title, Body: body, Draft: input.Draft, CWD: input.CWD})
	if err != nil {
		return worker.CreatePullRequestResult{}, err
	}
	return worker.CreatePullRequestResult{Number: pr.Number, URL: pr.URL}, nil
}

func (a workerGitHubAdapter) AddPullRequestLabels(ctx context.Context, input worker.PullRequestLabelsInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

// hitlGitHubSettings maps the HITL GitHub config into the worker's settings.
func hitlGitHubSettings(cfg *config.HITLGitHubConfig) worker.HITLGitHubSettings {
	if cfg == nil {
		return worker.HITLGitHubSettings{}
	}
	return worker.HITLGitHubSettings{
		AwaitingLabel: cfg.AwaitingLabel,
		MentionLogins: append([]string(nil), cfg.MentionLogins...),
	}
}

func (a workerGitHubAdapter) CompareBranches(ctx context.Context, input worker.CompareBranchesInput) (worker.CompareBranchesResult, error) {
	if a.gateway == nil {
		return worker.CompareBranchesResult{}, fmt.Errorf("github gateway is not configured")
	}
	comparison, err := a.gateway.CompareBranches(ctx, githubinfra.CompareBranchesInput{Repo: input.Repo, BaseBranch: input.BaseBranch, HeadBranch: input.HeadBranch, CWD: input.CWD})
	if err != nil {
		return worker.CompareBranchesResult{}, err
	}
	return worker.CompareBranchesResult{AheadBy: comparison.AheadBy, BehindBy: comparison.BehindBy, Status: comparison.Status, TotalCommits: comparison.TotalCommits}, nil
}

func (a workerGitHubAdapter) UpdatePullRequestBody(ctx context.Context, input worker.UpdatePullRequestBodyInput) error {
	body := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).Markdown(input.Body, "worker", disclosure.ChannelPullRequest)
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdatePullRequestBody(ctx, githubinfra.UpdatePullRequestBodyInput{Repo: input.Repo, PRNumber: input.PRNumber, Body: body, CWD: input.CWD})
}

func (a workerGitHubAdapter) UpdatePullRequestTitle(ctx context.Context, input worker.UpdatePullRequestTitleInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.UpdatePullRequestTitle(ctx, githubinfra.UpdatePullRequestTitleInput{Repo: input.Repo, PRNumber: input.PRNumber, Title: input.Title, CWD: input.CWD})
}

func (a workerGitHubAdapter) RemovePullRequestLabels(ctx context.Context, input worker.PullRequestLabelsInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: input.Repo, PRNumber: input.PRNumber, Labels: input.Labels, CWD: input.CWD})
}

func (a workerGitHubAdapter) AddPullRequestReviewers(ctx context.Context, input worker.PullRequestReviewersInput) error {
	if a.gateway == nil {
		return fmt.Errorf("github gateway is not configured")
	}
	return a.gateway.AddPullRequestReviewers(ctx, githubinfra.PullRequestReviewersInput{Repo: input.Repo, PRNumber: input.PRNumber, Reviewers: input.Reviewers, CWD: input.CWD})
}

type workerGitAdapter struct {
	gateway *gitinfra.Gateway
	stamper disclosure.Stamper
}

func (a workerGitAdapter) CreateWorktree(ctx context.Context, input worker.CreateWorktreeInput) (worker.CreateWorktreeResult, error) {
	worktree, err := a.gateway.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, Branch: input.Branch, BaseBranch: input.BaseBranch, PRNumber: input.PRNumber, ProtectedBranches: input.ProtectedBranches, CheckoutMode: gitinfra.CheckoutMode(input.CheckoutMode)})
	if err != nil {
		return worker.CreateWorktreeResult{}, err
	}
	return worker.CreateWorktreeResult{WorktreePath: worktree.WorktreePath, Branch: worktree.Branch, BaseBranch: derefString(worktree.BaseBranch), HeadSHA: derefString(worktree.HeadSHA), WorktreeID: worktree.ID}, nil
}

func (a workerGitAdapter) PrepareWorktree(ctx context.Context, input worker.PrepareWorktreeInput) (worker.PrepareWorktreeResult, error) {
	result, err := a.gateway.PrepareWorktree(ctx, gitinfra.PrepareWorktreeInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, ExpectedHeadSHA: input.ExpectedHeadSHA, Remote: input.Remote})
	if err != nil {
		return worker.PrepareWorktreeResult{}, err
	}
	return worker.PrepareWorktreeResult{HeadSHA: result.HeadSHA, Clean: result.Clean}, nil
}

func (a workerGitAdapter) InspectHead(ctx context.Context, input worker.InspectHeadInput) (worker.InspectHeadResult, error) {
	result, err := a.gateway.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, BaseRef: input.BaseRef})
	if err != nil {
		return worker.InspectHeadResult{}, err
	}
	return worker.InspectHeadResult{HeadSHA: result.HeadSHA, Branch: result.Branch, NewCommitSHAs: result.NewCommitSHAs, HasUncommittedChanges: result.HasUncommittedChanges, ChangedFiles: result.ChangedFiles, StagedFiles: result.StagedFiles, UntrackedFiles: result.UntrackedFiles, DiffFingerprint: result.DiffFingerprint}, nil
}

func (a workerGitAdapter) VerifyWorktreeIdentity(ctx context.Context, input worker.VerifyWorktreeIdentityInput) error {
	return a.gateway.VerifyWorktreeIdentity(ctx, gitinfra.VerifyWorktreeIdentityInput{
		RepoPath:        input.RepoPath,
		WorktreeRoot:    input.WorktreeRoot,
		WorktreePath:    input.WorktreePath,
		ExpectedBranch:  input.ExpectedBranch,
		ExpectedHeadSHA: input.ExpectedHeadSHA,
		CheckoutMode:    gitinfra.CheckoutMode(input.CheckoutMode),
	})
}

func (a workerGitAdapter) Commit(ctx context.Context, input worker.CommitInput) (worker.CommitResult, error) {
	message := a.stamper.WithIdentity(input.DisclosureAgent, input.DisclosureModel).CommitMessage(input.Message, "worker")
	result, err := a.gateway.Commit(ctx, gitinfra.CommitInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Message: message})
	if err != nil {
		return worker.CommitResult{}, err
	}
	return worker.CommitResult{CommitSHA: result.CommitSHA}, nil
}

func (a workerGitAdapter) Push(ctx context.Context, input worker.PushInput) error {
	return a.gateway.Push(ctx, gitinfra.PushInput{RepoPath: input.RepoPath, WorktreeRoot: input.WorktreeRoot, WorktreePath: input.WorktreePath, Branch: input.Branch, Remote: input.Remote, LocalHeadSHA: input.LocalHeadSHA, ExpectedRemoteHeadSHA: input.ExpectedRemoteHeadSHA, ProtectedBranches: input.ProtectedBranches})
}

type workerAgentExecutorAdapter struct {
	executor *agent.ConfiguredExecutor
}
type workerAgentExecutionAdapter struct {
	execution agent.Execution
}

func (a workerAgentExecutorAdapter) Start(ctx context.Context, input worker.AgentRunInput) (worker.AgentExecution, error) {
	// Ownership is acquired at the common executor boundary (AdmitSpawn +
	// BindHandle), not via post-spawn role-adapter registration (#576 / not #572).
	execution, err := a.executor.Start(ctx, agent.RunInput{
		ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID,
		Prompt: input.Prompt, NativeResumePrompt: input.NativeResumePrompt, NativeSessionID: input.NativeSessionID,
		WorkingDirectory: input.WorkingDirectory, Timeout: input.Timeout, HeartbeatTimeout: input.HeartbeatTimeout,
		Metadata: input.Metadata, IdempotencyKey: input.IdempotencyKey,
		RestrictToolNetwork: input.RestrictToolNetwork,
		OnBeforeTimeout:     input.OnBeforeTimeout,
		UseSnapshot:         input.UseSnapshot, SnapshotVendor: input.SnapshotVendor, SnapshotModel: input.SnapshotModel,
	})
	if err != nil {
		return nil, err
	}
	return workerAgentExecutionAdapter{execution: execution}, nil
}

func (a workerAgentExecutionAdapter) Wait(ctx context.Context) (worker.AgentResult, error) {
	result, err := a.execution.Wait(ctx)
	if err != nil {
		return worker.AgentResult{}, err
	}
	return worker.AgentResult{Status: result.Status, Summary: result.Summary, Stdout: result.Stdout, Stderr: result.Stderr, ParseStatus: result.ParseStatus, ChangedFiles: result.ChangedFiles, Commits: result.Commits, Lifecycle: result.Lifecycle, TimeoutType: result.TimeoutType, ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds, ConfiguredMaxRuntimeSeconds: result.ConfiguredMaxRuntimeSeconds, ElapsedRuntimeSeconds: result.ElapsedRuntimeSeconds, LastProgressAt: result.LastProgressAt, PreTimeoutError: result.PreTimeoutError}, nil
}

func (a workerAgentExecutionAdapter) Kill(reason string) error {
	return a.execution.Kill(reason)
}

type schedulerNotificationGatewayFactory struct {
	state         *notify.GatewayState
	feishuAppHTTP notify.FeishuAppHTTPFunc
}

func newSchedulerNotificationGatewayFactory() *schedulerNotificationGatewayFactory {
	return &schedulerNotificationGatewayFactory{state: notify.NewGatewayState()}
}

func (f *schedulerNotificationGatewayFactory) New(options notify.Options) *notify.Gateway {
	if options.FeishuAppHTTP == nil {
		options.FeishuAppHTTP = f.feishuAppHTTP
	}
	options.State = f.state
	return notify.NewGateway(options)
}

func buildCatalogSchedulerHandlers(source projects.ConfigSource, claimBoundary *sync.RWMutex, configPath string, logger bootstrap.Logger, coordinator *storage.SQLiteCoordinator, repos *storage.Repositories, gitGateway *gitinfra.Gateway, githubGateway *githubinfra.Gateway, activeExecutions *ActiveExecutionRegistry, asyncRunner func() schedulerAsyncRunner, requestWake func(), now func() time.Time, reconcileStaleRuns func(context.Context) (StaleRunReconcileSummary, error), allowClaim func() error, withAllowClaim func(fn func()) error) defaultSchedulerHandlers {
	if source == nil {
		fail := func(context.Context, Services) error { return fmt.Errorf("project catalog is not configured") }
		return defaultSchedulerHandlers{tick: fail, claim: fail}
	}
	claimMu := &sync.Mutex{}
	notificationGateways := newSchedulerNotificationGatewayFactory()
	coordinatorState := coordinatorrole.NewRuntimeState()
	initialConfig := source.Snapshot()
	if logger != nil {
		if len(initialConfig.Projects) == 0 && !config.HasEffectiveValidationCommands(initialConfig) {
			logger.Warn("worker/fixer validation gate disabled: defaults.validationCommands is empty; the validate step passes without running anything", nil)
		}
		for _, project := range initialConfig.Projects {
			switch {
			case config.ProjectValidationOptedOut(initialConfig, project.ID):
				logger.Warn("project explicitly opted out of worker/fixer validation", map[string]any{"projectId": project.ID})
			case config.ProjectValidationUsesLegacyDefaults(initialConfig, project.ID):
				logger.Warn("project uses deprecated defaults.validationCommands fallback; migrate to projects[].validation.commands", map[string]any{"projectId": project.ID})
			}
		}
	}
	// Trusted review proxies are minted per reviewer agent run (bound to that
	// run's PR). Catalog snapshots reuse claim serialization and notification
	// transport continuity while retaining config-specific policy.
	buildSnapshot := func() defaultSchedulerHandlers {
		cfg := source.Snapshot()
		return buildDefaultSchedulerHandlersWithOptions(cfg, configPath, logger, coordinator, repos, gitGateway, githubGateway, activeExecutions, asyncRunner, requestWake, now, reconcileStaleRuns, false, claimMu, nil, notificationGateways, coordinatorState)
	}
	handlers := defaultSchedulerHandlers{
		snapshot:             buildSnapshot,
		notificationGateways: notificationGateways,
	}
	if coordinator != nil && repos != nil {
		handlers.webhook = webhookforward.New(webhookforward.Options{
			Repos:        repos,
			Config:       source.Snapshot(),
			ConfigSource: source,
			Reviewer: catalogWebhookReviewer{snapshot: func() reviewerScheduler {
				return handlers.snapshot().reviewer
			}},
			Fixer: catalogWebhookFixer{snapshot: func() fixerScheduler {
				return handlers.snapshot().fixer
			}},
			Gatekeeper: catalogWebhookGatekeeper{snapshot: func() gatekeeperScheduler {
				return handlers.snapshot().gatekeeper
			}},
			Logger: logger,
			Now:    now,
			// Accept-time gate only: once Forward returns accepted/202 the
			// delivery is committed and workers complete discovery even if
			// admission later degrades. BeginShutdown/Stop aborts via CancelExecute.
			AllowExecute: allowClaim,
			// Hold admission across accept+enqueue so MarkDegraded cannot
			// flip closed mid-section before the accepted-delivery record.
			AllowExecuteWhile: withAllowClaim,
		})
	}
	attachClaimGate := func(input defaultSchedulerTickInput, services Services) defaultSchedulerTickInput {
		input.ClaimBoundary = claimBoundary
		input.AllowClaim = allowClaim
		// Wire Supervisor operation leases for claim ownership span (#579).
		// Prefer the live registry from Services when present (daemon path).
		if services.ActiveExecutions != nil {
			input.OperationOwner = services.ActiveExecutions
		} else if activeExecutions != nil {
			input.OperationOwner = activeExecutions
		}
		input.RefreshForClaim = func() defaultSchedulerTickInput {
			latestSnapshot := handlers.snapshot()
			if latestSnapshot.input == nil {
				stopped := input
				stopped.MaxConcurrentRuns = 0
				stopped.RefreshForClaim = nil
				stopped.AllowClaim = allowClaim
				if services.ActiveExecutions != nil {
					stopped.OperationOwner = services.ActiveExecutions
				} else if activeExecutions != nil {
					stopped.OperationOwner = activeExecutions
				}
				return stopped
			}
			latest := latestSnapshot.input(services)
			latest.ClaimBoundary = claimBoundary
			latest.AllowClaim = allowClaim
			if services.ActiveExecutions != nil {
				latest.OperationOwner = services.ActiveExecutions
			} else if activeExecutions != nil {
				latest.OperationOwner = activeExecutions
			}
			return latest
		}
		return input
	}
	handlers.tick = func(ctx context.Context, services Services) error {
		snapshot := handlers.snapshot()
		if snapshot.input == nil {
			return snapshot.tick(ctx, services)
		}
		return runDefaultSchedulerTick(ctx, attachClaimGate(snapshot.input(services), services))
	}
	handlers.claim = func(ctx context.Context, services Services) error {
		snapshot := handlers.snapshot()
		if snapshot.input == nil {
			return snapshot.claim(ctx, services)
		}
		return runIndependentClaimPass(ctx, attachClaimGate(snapshot.input(services), services))
	}
	return handlers
}

func buildDefaultSchedulerHandlersWithOptions(cfg config.Config, configPath string, logger bootstrap.Logger, coordinator *storage.SQLiteCoordinator, repos *storage.Repositories, gitGateway *gitinfra.Gateway, githubGateway *githubinfra.Gateway, activeExecutions *ActiveExecutionRegistry, asyncRunner func() schedulerAsyncRunner, requestWake func(), now func() time.Time, reconcileStaleRuns func(context.Context) (StaleRunReconcileSummary, error), includeWebhook bool, claimMu *sync.Mutex, configSource projects.ConfigSource, notificationGateways *schedulerNotificationGatewayFactory, coordinatorState *coordinatorrole.RuntimeState) defaultSchedulerHandlers {
	if now == nil {
		now = time.Now
	}
	if repos == nil || coordinator == nil {
		fail := func(context.Context, Services) error {
			return fmt.Errorf("default scheduler dependencies are not configured")
		}
		return defaultSchedulerHandlers{tick: fail, claim: fail}
	}
	view := projects.OperationViewFromConfig(cfg)
	// Always build coding-role runners, even when live ResolveAgent fails.
	// Sticky retries of failed/interrupted runs copy runs.agent_snapshot_json and
	// execute via UseSnapshot; omitting runners strands those queue items after
	// vendor removal. Fresh work for unconfigured roles is still not claimed —
	// claimTypeSetsFromInput puts those types in stickySnapshotOnly so only
	// loops with a valid predecessor snapshot are eligible. New discovery stays
	// gated on CodingRoleAgentConfigured via *DiscoveryEnabled flags and
	// webhook nil-runner checks.
	notificationGateway := notificationGateways.New(notify.Options{
		Config:        cfg.Notifications,
		OsascriptPath: derefString(cfg.Tools.OsascriptPath),
		LogFilePath:   filepath.Join(cfg.Daemon.LogDir, "looperd.log"),
		Repositories:  repos,
		Now:           now,
	})
	var gatekeeperRunner gatekeeperScheduler
	if githubGateway != nil {
		gatekeeperRunner = gatekeeper.New(gatekeeper.Options{
			Repos:  repos,
			GitHub: gatekeeperGitHubAdapter{Gateway: githubGateway, config: &cfg},
			Now:    now,
			TrustForProject: func(projectID string) config.GatekeeperTrustLevel {
				return gatekeeperTrustForProject(cfg, projectID)
			},
			MergeStrategyForProject: func(projectID string) config.ReviewerAutoMergeStrategy {
				return config.ProjectRoleConfigs(cfg, projectID).Reviewer.AutoMerge.Strategy
			},
			DiffBudgetForProject: func(projectID string) config.GatekeeperDiffBudget {
				return gatekeeperDiffBudgetForProject(cfg, projectID)
			},
			LogWarn: func(msg string, fields map[string]any) {
				if logger != nil {
					logger.Warn(msg, fields)
				}
			},
			PolicyPermitsTarget: func(projectID, repo, baseRefName string) bool {
				project, ok := runtimeProjectBinding(cfg, projectID)
				if !ok {
					return false
				}
				if !strings.EqualFold(strings.TrimSpace(project.Repo), strings.TrimSpace(repo)) {
					return false
				}
				configuredBase := cfg.Defaults.BaseBranch
				if project.BaseBranch != nil && strings.TrimSpace(*project.BaseBranch) != "" {
					configuredBase = strings.TrimSpace(*project.BaseBranch)
				}
				return strings.EqualFold(strings.TrimSpace(configuredBase), strings.TrimSpace(baseRefName))
			},
		})
	}
	// refreshFeishuAnchor re-renders a loop's thread-anchor card to reflect its
	// CURRENT status (colour + label), without disturbing the retained live tail.
	// The anchor is otherwise only patched opportunistically by the progress ticker
	// (OnProgress) while an agent runs — so a loop that finishes quickly, or leaves
	// awaiting_human on resume, would leave a stale "🔧 处理中 / ⏸ 等你定夺" header
	// forever. Calling this on every run start/finish makes the header converge to
	// the real status. App-mode only; a no-op otherwise.
	refreshFeishuAnchor := func(ctx context.Context, loopID string) {
		if strings.TrimSpace(loopID) == "" || !strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
			return
		}
		notificationGateway.RefreshThreadHeader(ctx, loopID, nil, 0)
	}
	notifyAgentExecutionStarted := func(ctx context.Context, input agentExecutionNotificationInput) error {
		notificationGateway.Notify(ctx, notify.SystemNotificationPayload{
			ID:         input.ExecutionID,
			ProjectID:  input.ProjectID,
			LoopID:     input.LoopID,
			RunID:      input.RunID,
			Level:      "info",
			Title:      input.Title,
			Subtitle:   input.Subtitle,
			Body:       input.Body,
			EntityType: "agent_execution",
			EntityID:   input.ExecutionID,
			DedupeKey:  input.DedupeKey,
		})
		refreshFeishuAnchor(ctx, input.LoopID)
		return nil
	}
	notifyWorkerRunCompleted := func(ctx context.Context, input workerRunCompletedNotificationInput) error {
		workerNotificationKeyID := runtimeFirstNonEmpty(input.RunID, input.LoopID)
		payload := notify.SystemNotificationPayload{
			ProjectID:  input.ProjectID,
			LoopID:     input.LoopID,
			RunID:      input.RunID,
			Subtitle:   input.Subtitle,
			EntityType: "run",
			EntityID:   input.RunID,
		}
		switch {
		case input.Status == "failed" && input.FailureKind == worker.FailureManualIntervention:
			payload.Level = "action_required"
			payload.Title = "Looper Worker Needs Attention"
			payload.Body = input.Summary
			payload.DedupeKey = fmt.Sprintf("runtime.worker.action_required:%s", workerNotificationKeyID)
		case input.Status == "failed":
			payload.Level = "failure"
			payload.Title = "Looper Worker Failed"
			payload.Body = input.Summary
			payload.DedupeKey = fmt.Sprintf("runtime.worker.failed:%s", workerNotificationKeyID)
		case input.Status == "skipped":
			payload.Level = "action_required"
			payload.Title = "Looper Worker Needs Attention"
			payload.Body = input.Summary
			payload.DedupeKey = fmt.Sprintf("runtime.worker.action_required:%s", workerNotificationKeyID)
		case input.PullRequestNumber > 0:
			payload.Level = "action_required"
			payload.Title = "Looper Worker Opened a PR"
			payload.Body = fmt.Sprintf("PR #%d is ready: %s", input.PullRequestNumber, runtimeFirstNonEmpty(input.PullRequestURL, input.Summary))
			payload.DedupeKey = fmt.Sprintf("runtime.worker.pr_ready:%s", input.RunID)
		default:
			payload.Level = "success"
			payload.Title = "Looper Worker Completed"
			payload.Body = runtimeFirstNonEmpty(input.Summary, "Worker completed successfully")
			payload.DedupeKey = fmt.Sprintf("runtime.worker.completed:%s", input.RunID)
		}
		notificationGateway.Notify(ctx, payload)
		// Log the outcome to the loop's story so the anchor reads as a narrative.
		if strings.TrimSpace(input.LoopID) != "" && strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
			switch {
			case input.PullRequestNumber > 0:
				link := runtimeFirstNonEmpty(input.PullRequestURL, "")
				if link != "" {
					notificationGateway.RecordMilestone(ctx, input.LoopID, fmt.Sprintf("🔀 已开 [PR #%d](%s)", input.PullRequestNumber, link))
				} else {
					notificationGateway.RecordMilestone(ctx, input.LoopID, fmt.Sprintf("🔀 已开 PR #%d", input.PullRequestNumber))
				}
			case input.Status == "failed" && input.FailureKind == worker.FailureManualIntervention:
				notificationGateway.RecordMilestone(ctx, input.LoopID, "⏸ 需要人处理")
			case input.Status == "failed":
				notificationGateway.RecordMilestone(ctx, input.LoopID, "⚠️ 本轮失败,重试中")
			case input.Status != "skipped":
				notificationGateway.RecordMilestone(ctx, input.LoopID, "✅ 完成")
			}
		} else {
			refreshFeishuAnchor(ctx, input.LoopID)
		}
		return nil
	}

	var triagerRunner triagerScheduler
	var plannerRoleRunner *planner.Runner
	var plannerRunner plannerScheduler
	var coordinatorRunner coordinatorScheduler
	var reviewerRunner reviewerScheduler
	var fixerRunner fixerScheduler
	var workerRunner workerScheduler

	looperCLIPath := resolveTrustedLooperCLIPath(cfg, logger)
	// Keep LOOPER_TRUSTED_REVIEW_SOCK out of shared agent executor env so
	// planner/worker/fixer cannot publish reviews. Inject only via the
	// reviewer adapter, which mints a per-run proxy bound to the selected PR.
	// Env-gated (not a config field yet) so it stays zero-risk to the schema
	// / parity fixtures until the codex --json path is proven end-to-end.
	liveToolEvents := strings.EqualFold(strings.TrimSpace(os.Getenv("LOOPER_CODEX_JSON_EVENTS")), "1")
	// Live progress → Feishu anchor card. Vendor-agnostic (works off the agent
	// subprocess's stdout tail). Only wired when the Feishu app-bot transport is
	// configured; a no-op otherwise.
	onAgentProgress := func(ctx context.Context, p agent.ProgressUpdate) {
		if p.LoopID == "" || !strings.EqualFold(strings.TrimSpace(cfg.Notifications.Webhook.Mode), "app") {
			return
		}
		// In-memory only — never writes the loop record, so it can't race the
		// scheduler's loop/run writes.
		notificationGateway.RefreshThreadHeader(ctx, p.LoopID, p.TailLines, p.ElapsedSeconds)
	}
	// Hard observation write failures degrade sticky admission (#578).
	onHardPersistFailure := func(err error) {
		if activeExecutions != nil {
			activeExecutions.ReportHardPersistFailure(err)
		}
	}
	newRoleAgentExecutor := func(resolved config.ResolvedAgent) *agent.ConfiguredExecutor {
		// agent.params (especially command/args) belong to the global agent
		// vendor. Keep the unstripped map and the owner vendor on the executor
		// so effectiveConfig can filter against the effective identity: live
		// role claims strip cross-vendor wrappers, while sticky snapshot
		// retries that restore the owner vendor keep command/args (resolveCommand
		// prefers params.command over vendor defaults).
		return agent.New(agent.ExecutorOptions{
			Config: agent.ExecutorConfig{
				Vendor:              resolved.Vendor,
				Model:               resolved.Model,
				Params:              cfg.Agent.Params,
				Env:                 cfg.Agent.Env,
				NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
				LiveToolEvents:      liveToolEvents,
			},
			ParamsOwnerVendor: cfg.Agent.Vendor,
			Repos:             repos,
			LogDir:            cfg.Daemon.LogDir,
			Now:               now,
			// Common executor boundary ownership for every in-scope agent role
			// (planner/reviewer/fixer/worker/coordinator) — not post-spawn adapters (#576).
			Owner:                activeExecutions,
			OnHardPersistFailure: onHardPersistFailure,
			OnProgress:           onAgentProgress,
		})
	}
	retryBaseDelay := time.Duration(cfg.Scheduler.RetryBaseDelayMS) * time.Millisecond
	codingRoles := config.EffectiveCodingRoles(cfg.Roles)
	plannerRole := codingRoles[config.CodingRolePlanner]
	reviewerRole := codingRoles[config.CodingRoleReviewer]
	fixerRole := codingRoles[config.CodingRoleFixer]
	workerRole := codingRoles[config.CodingRoleWorker]
	roleStamper := func(resolved config.ResolvedAgent) disclosure.Stamper {
		model := ""
		if resolved.Model != nil {
			model = *resolved.Model
		}
		return disclosure.Stamper{
			Config:  cfg.Disclosure,
			Version: version.Current().Version,
			Agent:   string(resolved.Vendor),
			Model:   model,
		}
	}

	// Construct even when live config no longer resolves so sticky snapshot retries remain claimable.
	resolvedPlanner, plannerConfigured := config.ResolveAgent(cfg, "", config.CodingRolePlanner)
	var plannerExecutor *agent.ConfiguredExecutor
	{
		resolved := resolvedPlanner
		plannerExecutor = newRoleAgentExecutor(resolved)
		var agentModel *string
		if resolved.Model != nil {
			model := *resolved.Model
			agentModel = &model
		}
		plannerStamper := roleStamper(resolved)
		plannerAutoDiscovery := plannerRole.Discovery.Enabled
		if !plannerConfigured {
			plannerAutoDiscovery = false
		}
		plannerRoleRunner = planner.New(planner.Options{
			DB:                 coordinator.DB(),
			Repos:              repos,
			GitHub:             plannerGitHubAdapter{gateway: githubGateway, stamper: plannerStamper, config: &cfg},
			Git:                plannerGitAdapter{gateway: gitGateway, stamper: plannerStamper},
			AgentExecutor:      plannerAgentExecutorAdapter{executor: plannerExecutor},
			Logger:             logger,
			Now:                now,
			AllowAutoPush:      boolPtr(cfg.Defaults.AllowAutoPush),
			Disclosure:         &cfg.Disclosure,
			AgentRuntime:       string(resolved.Vendor),
			AgentProfileID:     resolved.ProfileID,
			CustomInstructions: &cfg,
			AgentModel:         agentModel,
			AgentTimeout:       time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds) * time.Second,
			AgentIdleTimeout:   time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds) * time.Second,
			DiscoveryPolicy: planner.DiscoveryPolicy{
				AutoDiscovery:              plannerAutoDiscovery,
				Labels:                     append([]string(nil), plannerRole.Discovery.Labels...),
				LabelMode:                  plannerRole.Discovery.LabelMode,
				RequireAssigneeCurrentUser: plannerRole.Discovery.RequireAssigneeCurrentUser,
			},
			RetryBaseDelay:      retryBaseDelay,
			RetryMaxAttempts:    int64(cfg.Scheduler.RetryMaxAttempts),
			OnQueueItemEnqueued: requestWake,
			OnAgentExecutionStarted: func(ctx context.Context, input planner.AgentExecutionStartedInput) error {
				return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Planner", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
			},
		})
		plannerRunner = plannerRoleRunner
	}
	if plannerConfigured && githubGateway != nil {
		triagerRunner = triager.New(triager.Options{
			Repos:   repos,
			GitHub:  githubGateway,
			Planner: plannerRoleRunner,
			LLM: triager.NewAgentLLM(
				plannerExecutor,
				time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds)*time.Second,
				time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds)*time.Second,
			),
			Now:            now,
			SourceLookback: time.Duration(maxInt(300, 2*cfg.Scheduler.PollIntervalSeconds)) * time.Second,
		})
	}

	// Coordinator triage LLM uses the global agent only; missing global vendor
	// must not disable coding roles that resolve via role/profile bindings.
	coordinatorOpts := coordinatorrole.Options{
		Repos:   repos,
		GitHub:  githubGateway,
		Config:  &cfg,
		Logger:  logger,
		Now:     now,
		State:   coordinatorState,
		Network: coordinatorrole.NewLoopernetGateway(networkclient.DefaultStatePath(runtimeHomeDirOrEmpty())),
	}
	if cfg.Agent.Vendor != nil {
		// Same ParamsOwnerVendor as coding-role executors: agent.params.command/args
		// are owned by global agent.vendor. Without this, effectiveConfig's
		// ParamsForRoleVendor nil-owner path strips wrappers and coordinator
		// triage launches bare vendor defaults while role executors keep them.
		globalExecutor := agent.New(agent.ExecutorOptions{
			Config: agent.ExecutorConfig{
				Vendor:              *cfg.Agent.Vendor,
				Model:               cfg.Agent.Model,
				Params:              cfg.Agent.Params,
				Env:                 cfg.Agent.Env,
				NativeResumeEnabled: cfg.Agent.NativeResume.Enabled,
				LiveToolEvents:      liveToolEvents,
			},
			ParamsOwnerVendor:    cfg.Agent.Vendor,
			Repos:                repos,
			LogDir:               cfg.Daemon.LogDir,
			Now:                  now,
			Owner:                activeExecutions,
			OnHardPersistFailure: onHardPersistFailure,
			OnProgress:           onAgentProgress,
		})
		coordinatorOpts.TriageLLM = coordinatorrole.NewAgentLLM(globalExecutor, now,
			time.Duration(cfg.Agent.Timeouts.PlannerMaxRuntimeSeconds)*time.Second,
			time.Duration(cfg.Agent.Timeouts.PlannerIdleTimeoutSeconds)*time.Second,
		)
	} else if logger != nil {
		logger.Warn("coordinator triage LLM skipped: global agent.vendor is not configured", nil)
	}
	coordinatorRunner = coordinatorrole.New(coordinatorOpts)

	resolvedReviewer, reviewerConfigured := config.ResolveAgent(cfg, "", config.CodingRoleReviewer)
	{
		resolved := resolvedReviewer
		reviewerExecutor := newRoleAgentExecutor(resolved)
		var agentModel *string
		if resolved.Model != nil {
			model := *resolved.Model
			agentModel = &model
		}
		reviewerStamper := roleStamper(resolved)
		reviewerAutoDiscovery := reviewerRole.Discovery.Enabled
		if !reviewerConfigured {
			reviewerAutoDiscovery = false
		}
		reviewerRunner = reviewer.New(reviewer.Options{
			DB:     coordinator.DB(),
			Repos:  repos,
			GitHub: reviewerGitHubAdapter{gateway: githubGateway, stamper: reviewerStamper, config: &cfg},
			Git:    reviewerGitAdapter{gateway: gitGateway},
			AgentExecutor: reviewerAgentExecutorAdapter{
				executor:    reviewerExecutor,
				realLooper:  looperCLIPath,
				trustedEnv:  trustedReviewChildEnv(cfg),
				config:      &cfg,
				agentVendor: resolved.Vendor,
				agentModel:  agentModel,
			},
			Logger:           logger,
			Now:              now,
			AllowAutoApprove: cfg.Defaults.AllowAutoApprove,
			ReviewEvents:     cfg.Roles.Reviewer.Behavior.ReviewEvents,
			LoopConfig:       cfg.Roles.Reviewer.Behavior.Loop,
			DiscoveryPolicy: reviewer.DiscoveryPolicy{
				AutoDiscovery:             reviewerAutoDiscovery,
				IncludeDrafts:             reviewerRole.Discovery.IncludeDrafts,
				RequireReviewRequest:      reviewerRole.Discovery.RequireReviewRequest,
				EnableSelfReview:          reviewerRole.Discovery.EnableSelfReview,
				Labels:                    append([]string(nil), reviewerRole.Discovery.Labels...),
				LabelMode:                 reviewerRole.Discovery.LabelMode,
				IncludeSpecReviewingLabel: cfg.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel,
				SpecReviewingLabel:        cfg.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel,
			},
			Scope:                   cfg.Roles.Reviewer.Behavior.Scope,
			DetectDuplicateFindings: cfg.Roles.Reviewer.Behavior.DetectDuplicateFindings,
			NativeResume:            cfg.Roles.Reviewer.Behavior.NativeResume,
			ThreadResolution:        cfg.Roles.Reviewer.Behavior.ThreadResolution,
			Disclosure:              &cfg.Disclosure,
			AgentRuntime:            string(resolved.Vendor),
			AgentProfileID:          resolved.ProfileID,
			CustomInstructions:      &cfg,
			LooperCLIPath:           looperCLIPath,
			AgentModel:              agentModel,
			AgentTimeout:            time.Duration(cfg.Agent.Timeouts.ReviewerMaxRuntimeSeconds) * time.Second,
			AgentIdleTimeout:        time.Duration(cfg.Agent.Timeouts.ReviewerIdleTimeoutSeconds) * time.Second,
			RetryBaseDelay:          retryBaseDelay,
			RetryMaxAttempts:        int64(cfg.Scheduler.RetryMaxAttempts),
			RetryPolicy:             cfg.Roles.Reviewer.Behavior.Retry,
			OnQueueItemEnqueued:     requestWake,
			OnAgentExecutionStarted: func(ctx context.Context, input reviewer.AgentExecutionStartedInput) error {
				return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Reviewer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
			},
		})
	}
	validationCommands := config.ResolveValidationCommands(cfg)
	validationCommandsByProject := config.ResolveProjectValidationCommandsByID(cfg)
	resolvedFixer, fixerConfigured := config.ResolveAgent(cfg, "", config.CodingRoleFixer)
	{
		resolved := resolvedFixer
		fixerExecutor := newRoleAgentExecutor(resolved)
		var agentModel *string
		if resolved.Model != nil {
			model := *resolved.Model
			agentModel = &model
		}
		fixerStamper := roleStamper(resolved)
		fixerAutoDiscovery := fixerRole.Discovery.Enabled
		if !fixerConfigured {
			fixerAutoDiscovery = false
		}
		fixerRunner = fixer.New(fixer.Options{
			DB:                          coordinator.DB(),
			Repos:                       repos,
			GitHub:                      fixerGitHubAdapter{gateway: githubGateway, stamper: fixerStamper, config: &cfg},
			Git:                         fixerGitAdapter{gateway: gitGateway, stamper: fixerStamper},
			AgentExecutor:               fixerAgentExecutorAdapter{executor: fixerExecutor},
			Logger:                      logger,
			Now:                         now,
			AllowAutoCommit:             cfg.Defaults.AllowAutoCommit,
			AllowAutoPush:               cfg.Defaults.AllowAutoPush,
			AllowRiskyFixes:             cfg.Defaults.AllowRiskyFixes,
			FixAllPullRequests:          cfg.Defaults.FixAllPullRequests,
			ValidationCommands:          validationCommands,
			ValidationCommandsByProject: validationCommandsByProject,
			// Validation shell is Supervisor-owned (#577): track handles for retain-storage.
			ContainmentTracker: activeExecutions,
			DiscoveryPolicy: fixer.DiscoveryPolicy{
				AutoDiscovery: fixerAutoDiscovery,
				IncludeDrafts: fixerRole.Discovery.IncludeDrafts,
				AuthorFilter:  config.FixerAuthorFilter(fixerRole.Discovery.AuthorFilter),
				Labels:        append([]string(nil), fixerRole.Discovery.Labels...),
				LabelMode:     fixerRole.Discovery.LabelMode,
			},
			Disclosure:                  &cfg.Disclosure,
			AgentRuntime:                string(resolved.Vendor),
			AgentProfileID:              resolved.ProfileID,
			CustomInstructions:          &cfg,
			AgentModel:                  agentModel,
			AgentTimeout:                time.Duration(cfg.Agent.Timeouts.FixerMaxRuntimeSeconds) * time.Second,
			AgentIdleTimeout:            time.Duration(cfg.Agent.Timeouts.FixerIdleTimeoutSeconds) * time.Second,
			RetryBaseDelay:              retryBaseDelay,
			RetryMaxAttempts:            int64(cfg.Scheduler.RetryMaxAttempts),
			MaxConsecutiveFixerFailures: cfg.Scheduler.ConsecutiveFailureThreshold,
			OnQueueItemEnqueued:         requestWake,
			OnAgentExecutionStarted: func(ctx context.Context, input fixer.AgentExecutionStartedInput) error {
				return notifyAgentExecutionStarted(ctx, agentExecutionNotificationInput{ExecutionID: input.ExecutionID, ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Title: "Looper Fixer", Subtitle: input.Subtitle, Body: input.Body, DedupeKey: input.DedupeKey})
			},
		})
	}
	notifyHITLAsk := func(ctx context.Context, ask worker.HITLAskNotification) error {
		return notificationGateway.SendHITLAsk(ctx, notify.HITLAskCard{
			ProjectID: ask.ProjectID, LoopID: ask.LoopID, LoopSeq: ask.LoopSeq,
			Repo: ask.Repo, Title: ask.Title, Question: ask.Question, Options: ask.Options,
			SourceType: ask.SourceType, SourceRef: ask.SourceRef, SourceURL: ask.SourceURL,
			TriggerLogin:      ask.TriggerLogin,
			Recommendation:    ask.Recommendation,
			RecommendedOption: ask.RecommendedOption,
			Consequences:      ask.Consequences,
			Confidence:        ask.Confidence,
		})
	}
	resolvedWorker, workerConfigured := config.ResolveAgent(cfg, "", config.CodingRoleWorker)
	{
		resolved := resolvedWorker
		workerExecutor := newRoleAgentExecutor(resolved)
		var agentModel *string
		if resolved.Model != nil {
			model := *resolved.Model
			agentModel = &model
		}
		workerStamper := roleStamper(resolved)
		workerAutoDiscovery := workerRole.Discovery.Enabled
		if !workerConfigured {
			workerAutoDiscovery = false
		}
		workerRunner = worker.New(worker.Options{
			DB:     coordinator.DB(),
			Repos:  repos,
			GitHub: workerGitHubAdapter{gateway: githubGateway, stamper: workerStamper, config: &cfg},
			GitHubCLIAutoPROpeningAvailable: func(ctx context.Context, repo, cwd string) bool {
				return githubCLIAutoPROpeningAvailable(ctx, cfg, githubGateway, logger, repo, cwd)
			},
			Git:                         workerGitAdapter{gateway: gitGateway, stamper: workerStamper},
			AgentExecutor:               workerAgentExecutorAdapter{executor: workerExecutor},
			Logger:                      logger,
			Now:                         now,
			AllowAutoCommit:             cfg.Defaults.AllowAutoCommit,
			AllowAutoPush:               cfg.Defaults.AllowAutoPush,
			OpenPRStrategy:              cfg.Defaults.OpenPRStrategy,
			ValidationCommands:          validationCommands,
			ValidationCommandsByProject: validationCommandsByProject,
			// Validation shell is Supervisor-owned (#577): track handles for retain-storage.
			ContainmentTracker: activeExecutions,
			DiscoveryPolicy: worker.DiscoveryPolicy{
				AutoDiscovery:              workerAutoDiscovery,
				Labels:                     append([]string(nil), workerRole.Discovery.Labels...),
				LabelMode:                  workerRole.Discovery.LabelMode,
				RequireAssigneeCurrentUser: workerRole.Discovery.RequireAssigneeCurrentUser,
			},
			Disclosure:          &cfg.Disclosure,
			AgentRuntime:        string(resolved.Vendor),
			AgentProfileID:      resolved.ProfileID,
			CustomInstructions:  &cfg,
			Network:             coordinatorNetworkGateway{statePath: networkclient.DefaultStatePath(runtimeHomeDirOrEmpty()), client: &http.Client{Timeout: 10 * time.Second}},
			AgentModel:          agentModel,
			AgentTimeout:        time.Duration(cfg.Agent.Timeouts.WorkerMaxRuntimeSeconds) * time.Second,
			AgentIdleTimeout:    time.Duration(cfg.Agent.Timeouts.WorkerIdleTimeoutSeconds) * time.Second,
			RetryBaseDelay:      retryBaseDelay,
			RetryMaxAttempts:    int64(cfg.Scheduler.RetryMaxAttempts),
			OnQueueItemEnqueued: requestWake,
			OnRunCompleted: func(ctx context.Context, input worker.RunCompletedInput) error {
				return notifyWorkerRunCompleted(ctx, workerRunCompletedNotificationInput{ProjectID: input.ProjectID, LoopID: input.LoopID, RunID: input.RunID, Subtitle: input.Subtitle, Status: input.Status, Summary: input.Summary, FailureKind: input.FailureKind, PullRequestNumber: input.PullRequestNumber, PullRequestURL: input.PullRequestURL})
			},
			HITLEnabled:         cfg.HITL.Enabled,
			HITLAnswerTransport: cfg.HITL.AnswerTransport,
			HITLGitHub:          hitlGitHubSettings(cfg.HITL.GitHub),
			HITLNotify:          notifyHITLAsk,
		})
	}
	if claimMu == nil {
		claimMu = &sync.Mutex{}
	}

	inputForServices := func(services Services) defaultSchedulerTickInput {
		var runner schedulerAsyncRunner
		if asyncRunner != nil {
			runner = asyncRunner()
		}
		// Avoid typed-nil interface: (*githubinfra.Gateway)(nil) assigned to
		// snapshotScheduler is a non-nil interface value and would enable snapshot claims.
		var snapshotter snapshotScheduler
		if githubGateway != nil {
			snapshotter = githubGateway
		}
		return defaultSchedulerTickInput{
			Repos:                services.Repositories,
			GitHubGateway:        githubGateway,
			GitGateway:           gitGateway,
			Logger:               logger,
			Now:                  now,
			MaxConcurrentRuns:    cfg.Scheduler.MaxConcurrentRuns,
			ClaimMu:              claimMu,
			ReconcileStaleRuns:   reconcileStaleRuns,
			AsyncRunner:          runner,
			RequestSchedulerWake: requestWake,
			Triager:              triagerRunner,
			Planner:              plannerRunner,
			Coordinator:          coordinatorRunner,
			Reviewer:             reviewerRunner,
			Fixer:                fixerRunner,
			Gatekeeper:           gatekeeperRunner,
			Worker:               workerRunner,
			Snapshotter:          snapshotter,
			Config:               &cfg,
			// Live discovery requires a currently resolvable agent; sticky retries
			// still claim via always-present runners when vendor was removed
			// (claimTypeSetsFromInput restricts unconfigured roles to snapshot items).
			PlannerDiscoveryEnabled: boolPtr(plannerConfigured && view.AnyProjectRoleAutoDiscovery("planner")),
			TriagerEnabled: func(projectID string) bool {
				policy := view.RolePolicy(projectID)
				return plannerConfigured && policy.RoleAutoDiscovery(config.CodingRolePlanner) && !policy.Roles.Coordinator.Enabled
			},
			CoordinatorEnabled:       func(projectID string) bool { return view.RolePolicy(projectID).Roles.Coordinator.Enabled },
			ReviewerDiscoveryEnabled: boolPtr(reviewerConfigured && view.AnyProjectRoleAutoDiscovery("reviewer")),
			FixerDiscoveryEnabled:    boolPtr(fixerConfigured && view.AnyProjectRoleAutoDiscovery("fixer")),
			WorkerDiscoveryEnabled:   boolPtr(workerConfigured && view.AnyProjectRoleAutoDiscovery("worker")),
			OnHITLAsk:                notifyHITLAsk,
			OnHITLAnswerDelivered:    notificationGateway.MarkAskAnswered,
			OnDeployFinished: func(ctx context.Context, notification DeployNotification) {
				level := "info"
				if !notification.Outcome.Succeeded {
					level = "action_required"
				}
				notificationGateway.Notify(ctx, notify.SystemNotificationPayload{
					ID:         fmt.Sprintf("deploy-%d", notification.Outcome.DeploymentID),
					ProjectID:  notification.ProjectID,
					Level:      level,
					Title:      notification.Title(),
					Subtitle:   notification.Subtitle(),
					Body:       notification.Body(),
					EntityType: "deployment",
					EntityID:   notification.Outcome.SHA,
					// One notification per deployment: a retried status write must not
					// produce a second ping for the same deploy.
					DedupeKey: fmt.Sprintf("deploy:%s:%s", notification.Repo, notification.Outcome.SHA),
				})
			},
		}
	}

	// Webhook-driven discovery must not enroll new work without a live agent.
	// Claim/tick still use the always-present runners for sticky snapshot retries.
	var webhookReviewer reviewerScheduler
	var webhookFixer fixerScheduler
	if reviewerConfigured {
		webhookReviewer = reviewerRunner
	}
	if fixerConfigured {
		webhookFixer = fixerRunner
	}
	handlers := defaultSchedulerHandlers{
		tick: func(ctx context.Context, services Services) error {
			return runDefaultSchedulerTick(ctx, inputForServices(services))
		},
		claim: func(ctx context.Context, services Services) error {
			return runIndependentClaimPass(ctx, inputForServices(services))
		},
		reviewer:             webhookReviewer,
		fixer:                webhookFixer,
		gatekeeper:           gatekeeperRunner,
		input:                inputForServices,
		notificationGateways: notificationGateways,
	}
	if includeWebhook {
		handlers.webhook = webhookforward.New(webhookforward.Options{
			Repos:        repos,
			Config:       cfg,
			ConfigSource: configSource,
			Reviewer:     webhookReviewer,
			Fixer:        webhookFixer,
			Gatekeeper:   gatekeeperRunner,
			Logger:       logger,
			Now:          now,
		})
	}
	return handlers
}

func githubCLIAutoPROpeningAvailable(ctx context.Context, cfg config.Config, githubGateway *githubinfra.Gateway, logger bootstrap.Logger, repo, cwd string) bool {
	if configuredPath := strings.TrimSpace(derefString(cfg.Tools.GHPath)); configuredPath != "" {
		githubGateway = githubinfra.New(githubinfra.Options{GHPath: configuredPath, CWD: cwd, Env: config.DaemonGitHubCredentialEnv(cfg)})
	}
	if githubGateway == nil {
		return false
	}
	hostname := githubAuthHostname(repo)
	authenticated, err := githubGateway.IsAuthenticated(ctx, cwd, hostname)
	if err != nil {
		if logger != nil {
			logger.Warn("github cli auth check failed; disabling automatic PR opening", map[string]any{"error": err.Error(), "repo": repo, "hostname": hostname})
		}
		return false
	}
	return authenticated
}

func githubAuthHostname(repo string) string {
	const defaultHost = "github.com"
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return defaultHost
	}
	if parsed, err := url.Parse(repo); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	if at := strings.Index(repo, "@"); at >= 0 {
		repo = repo[at+1:]
	}
	if colon := strings.Index(repo, ":"); colon > 0 {
		return strings.TrimSpace(repo[:colon])
	}
	parts := strings.Split(repo, "/")
	if len(parts) == 3 && strings.TrimSpace(parts[0]) != "" {
		return strings.TrimSpace(parts[0])
	}
	return defaultHost
}

func runDefaultSchedulerTick(ctx context.Context, input defaultSchedulerTickInput) (retErr error) {
	if input.Repos == nil || input.Repos.Projects == nil {
		return nil
	}
	// Gate the entire work-producing tick (discovery, HITL, claims, stale
	// reconcile), not only ClaimNext*. Admission closed during starting/stopping
	// must not enqueue via CreateOrGetActiveByDedupe or requeue via reconcile.
	if err := admissionRefuseWork(input); err != nil {
		if input.Logger != nil {
			input.Logger.Debug("scheduler tick skipped: admission closed", map[string]any{"error": err.Error()})
		}
		return nil
	}

	startedAt := time.Now()
	claimStats := schedulerClaimStats{}
	if input.Logger != nil {
		input.Logger.Debug("scheduler tick start", nil)
	}
	defer func() {
		if input.Logger == nil {
			return
		}
		fields := map[string]any{"durationMs": time.Since(startedAt).Milliseconds(), "claimedCount": claimStats.claimedCount, "availableSlots": claimStats.availableSlots}
		if retErr != nil {
			fields["error"] = retErr.Error()
		}
		input.Logger.Debug("scheduler tick end", fields)
		input.Logger.Info("scheduler tick summary", fields)
	}()

	now := input.Now
	if now == nil {
		now = time.Now
	}

	errs := make([]error, 0)
	appendErr := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	discoveredRunnableIDs := make(map[string]struct{})
	trackRunnableDiscovery := func(queueItems []storage.QueueItemRecord) {
		for _, id := range runnableSchedulerQueueItemIDs(queueItems, now) {
			discoveredRunnableIDs[id] = struct{}{}
		}
	}
	recordClaim := func(claimedCount, availableSlots int, err error) {
		claimStats.record(claimedCount, availableSlots)
		appendErr(err)
	}
	// Liveness quarantine must be revisited on the scheduler's periodic full
	// tick, independently of capacity. Tying this to availableSlots == 0 strands
	// quarantined work forever on otherwise-idle daemons.
	if input.ReconcileStaleRuns != nil {
		// Recheck immediately before the mutating reconciliation so shutdown cannot
		// race past the tick-entry admission check.
		if err := admissionRefuseWork(input); err != nil {
			return nil
		}
		_, err := input.ReconcileStaleRuns(ctx)
		appendErr(err)
	}

	projectsList, catalogCurrent, err := schedulerProjectsForCapturedCatalog(ctx, input)
	if err != nil {
		appendErr(err)
		retErr = errors.Join(errs...)
		return retErr
	}
	if !catalogCurrent {
		return nil
	}

	tickDiscoveryState := githubinfra.NewDiscoveryTickState()
	projectSnapshots := map[string]*githubinfra.DiscoverySnapshot{}
	projectSnapshot := func(projectID string) *githubinfra.DiscoverySnapshot {
		if input.GitHubGateway == nil {
			return nil
		}
		if snapshot, ok := projectSnapshots[projectID]; ok {
			return snapshot
		}
		snapshot := githubinfra.NewDiscoverySnapshot(input.GitHubGateway, tickDiscoveryState, projectDiscoverySnapshotOptions(input, projectID))
		projectSnapshots[projectID] = snapshot
		return snapshot
	}
	// A queued worker can otherwise be claimed by the independent claim pump
	// before discovery observes that its issue closed. Settle against a fresh
	// issue page first; the helper holds the claim boundary through its authority
	// read and atomic retirement.
	for _, project := range projectsList {
		if project.Archived {
			continue
		}
		repo, inCatalog := schedulerProjectRepo(input, project)
		if !inCatalog || repo == "" {
			continue
		}
		if err := admissionRefuseWork(input); err != nil {
			return nil
		}
		settleErr := settleQueuedWorkerIssueTargets(ctx, input.Repos, projectSnapshot(project.ID), input.ClaimBoundary, project.ID, repo, formatJavaScriptISOString(now()), func(msg string, fields map[string]any) {
			if input.Logger != nil {
				input.Logger.Debug(msg, fields)
			}
		})
		appendErr(settleErr)
	}

	claimedCount, availableSlots, err := executeClaimPhase(ctx, "pre_discovery", input, discoveredRunnableIDs, true)
	recordClaim(claimedCount, availableSlots, err)
	lanes := discoveryLanes(input)
	for _, project := range projectsList {
		if err := ctx.Err(); err != nil {
			retErr = errors.Join(append(errs, err)...)
			return retErr
		}
		// Recheck before each project's work-producing lanes so BeginShutdown
		// during HTTP drain cannot leave a tick that passed the entry gate free
		// to enqueue discovery/HITL after admission is already stopping.
		if err := admissionRefuseWork(input); err != nil {
			if input.Logger != nil {
				input.Logger.Debug("scheduler tick stopped mid-flight: admission closed", map[string]any{"error": err.Error(), "projectId": project.ID})
			}
			break
		}
		if project.Archived {
			continue
		}
		repo, inCatalog := schedulerProjectRepo(input, project)
		if !inCatalog {
			if input.Logger != nil {
				input.Logger.Debug("scheduler skipped project missing from captured catalog", map[string]any{"projectId": project.ID})
			}
			continue
		}
		snapshot := projectSnapshot(project.ID)
		if repo == "" {
			if input.Logger != nil {
				input.Logger.Warn("scheduler skipped project without repo metadata", map[string]any{"projectId": project.ID})
			}
			continue
		}
		admissionClosed := false
		for _, lane := range lanes {
			if !lane.Present {
				continue
			}
			if !lane.Enabled(project.ID) {
				if lane.LogWhenDisabled && input.Logger != nil {
					input.Logger.Debug(lane.Name+" auto-discovery disabled", map[string]any{"projectId": project.ID, "repo": repo})
				}
				continue
			}
			if err := admissionRefuseWork(input); err != nil {
				admissionClosed = true
				break
			}
			label := lane.laneLabel()
			appendErr(runSchedulerLane(input, label, project.ID, repo, func() error {
				items, err := lane.Discover(ctx, project.ID, repo, snapshot)
				trackRunnableDiscovery(items)
				return wrapSchedulerError(label, project.ID, repo, err)
			}))
			claimedCount, availableSlots, err = executeClaimPhase(ctx, lane.claimPhaseLabel(), input, discoveredRunnableIDs, true)
			recordClaim(claimedCount, availableSlots, err)
		}
		if admissionClosed {
			break
		}

		// Auditor observes the default branch only when the operator opted in.
		// It appends evidence for a later confirmation/revert-proposal pass and
		// never creates forge work from this scheduler tick.
		if err := admissionRefuseWork(input); err != nil {
			break
		}
		appendErr(runSchedulerLane(input, "auditor", project.ID, repo, func() error {
			return runAuditorLane(ctx, input, project, repo)
		}))

		// HITL (github transport): deliver any human answers posted on this
		// project's awaiting_human PRs so those loops resume.
		if err := admissionRefuseWork(input); err != nil {
			break
		}
		runGitHubHITLPoll(ctx, input, project)

		// Deploy asynchronously. A deploy runs an operator command for up to
		// roles.deployer.timeoutSeconds — fifteen minutes by default — and the tick
		// is serial, so running it here would stall every other project's discovery
		// for the duration.
		if err := admissionRefuseWork(input); err != nil {
			break
		}
		startDeployLane(ctx, input, project, repo)
	}

	// HITL (feishu transport): poll the shared Cloudflare inbox once per tick and
	// deliver any answers for this looper's awaiting loops.
	if err := admissionRefuseWork(input); err == nil {
		runFeishuHITLPoll(ctx, input)
	}

	// Intake (telegram): drain one batch of chat messages, opening an Issue for
	// each new request.
	if err := admissionRefuseWork(input); err == nil {
		runTelegramIntakePoll(ctx, input)
	}

	claimedCount, availableSlots, err = executeClaimPhase(ctx, "post_discovery", input, discoveredRunnableIDs, true)
	recordClaim(claimedCount, availableSlots, err)

	if len(errs) == 0 {
		return nil
	}
	retErr = errors.Join(errs...)
	return retErr
}

// admissionRefuseWork rechecks the admission projection mid-tick. Nil AllowClaim
// means ungated (tests). A non-nil error means admission is closed and the
// caller must not start further discovery/HITL enqueue work.
func admissionRefuseWork(input defaultSchedulerTickInput) error {
	if input.AllowClaim == nil {
		return nil
	}
	return input.AllowClaim()
}

func discoveryEnabled(value *bool) bool {
	return value == nil || *value
}

func coordinatorEnabledForProject(input defaultSchedulerTickInput, projectID string) bool {
	if input.CoordinatorEnabled == nil {
		return false
	}
	return input.CoordinatorEnabled(projectID)
}

// schedulerProjectRepo resolves the repo a project's discovery reads, with the
// same precedence the lane walk uses: the captured catalog binding when a config
// is present, otherwise the project's stored metadata. inCatalog is false when a
// config is present but the project is absent from it.
func schedulerProjectRepo(input defaultSchedulerTickInput, project storage.ProjectRecord) (repo string, inCatalog bool) {
	repo = repoFromProjectMetadata(project.MetadataJSON)
	if input.Config != nil {
		binding, ok := runtimeProjectBinding(*input.Config, project.ID)
		if !ok {
			return "", false
		}
		repo = strings.TrimSpace(binding.Repo)
	}
	return repo, true
}

func projectDiscoverySnapshotOptions(input defaultSchedulerTickInput, projectID string) githubinfra.DiscoverySnapshotOptions {
	prLimit := 30
	issueLimit := 30
	if input.Coordinator != nil && coordinatorEnabledForProject(input, projectID) {
		issueLimit = maxInt(issueLimit, 100)
	}
	// Size the page to the largest limit a lane actually asks for. Gatekeeper lists
	// 100 open PRs with no filters, so against a 30-PR page
	// shouldFallbackToFilteredPullRequestQuery is unconditionally true: every
	// gatekeeper tick discarded the page it was handed and paid a second raw
	// 100-PR query. Matching the limit makes the shared page usable and removes
	// that duplicate query whether or not anything prewarms it.
	if input.Gatekeeper != nil {
		prLimit = maxInt(prLimit, gatekeeper.DefaultDiscoveryPullRequestLimit)
	}
	return githubinfra.DiscoverySnapshotOptions{PullRequestLimit: prLimit, IssueLimit: issueLimit}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func runnableSchedulerQueueItemIDs(queueItems []storage.QueueItemRecord, now func() time.Time) []string {
	if len(queueItems) == 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	ids := make([]string, 0, len(queueItems))
	for _, item := range queueItems {
		if item.Status == "queued" && item.AvailableAt <= nowISO {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func requestWakeForClaimedDiscovery(claimedItems []storage.QueueItemRecord, discoveredRunnableIDs map[string]struct{}, requestWake func()) {
	if requestWake == nil || len(claimedItems) == 0 || len(discoveredRunnableIDs) == 0 {
		return
	}
	for _, item := range claimedItems {
		if _, ok := discoveredRunnableIDs[item.ID]; ok {
			requestWake()
			return
		}
	}
}

type schedulerClaimStats struct {
	claimedCount   int
	availableSlots int
}

func (s *schedulerClaimStats) record(claimedCount, availableSlots int) {
	s.claimedCount += claimedCount
	s.availableSlots = availableSlots
}

func runIndependentClaimPass(ctx context.Context, input defaultSchedulerTickInput) error {
	if input.AllowClaim != nil {
		if err := input.AllowClaim(); err != nil {
			return nil
		}
	}
	_, catalogCurrent, err := schedulerProjectsForCapturedCatalog(ctx, input)
	if err != nil || !catalogCurrent {
		return err
	}
	_, _, err = executeClaimPhase(ctx, "claim_pump", input, nil, false)
	return err
}

func schedulerProjectsForCapturedCatalog(ctx context.Context, input defaultSchedulerTickInput) ([]storage.ProjectRecord, bool, error) {
	projectsList, err := input.Repos.Projects.List(ctx)
	if err != nil {
		return nil, false, err
	}
	if input.Config == nil {
		return projectsList, true, nil
	}
	for _, project := range projectsList {
		if project.Archived {
			continue
		}
		binding, ok := runtimeProjectBinding(*input.Config, project.ID)
		// validateCatalogValidationPolicies quarantines an unstanced legacy
		// project whenever defaults.validationCommands is empty, independently
		// of whether a coding agent is configured. The missing catalog binding
		// is therefore intentional in the agentless case too; recognizing only
		// the coding-policy-required case would report the catalog as stale and
		// block every claim pass — including durable sticky Worker/Fixer retries
		// — until the legacy record is repaired.
		if !ok && projects.IsLegacyInertProject(project) && len(config.ResolveValidationCommands(*input.Config)) == 0 {
			continue
		}
		if !ok || !catalogBindingMatchesProject(binding, project) {
			if input.Logger != nil {
				input.Logger.Debug("scheduler deferred pass until project catalog catches up", map[string]any{"projectId": project.ID})
			}
			return projectsList, false, nil
		}
	}
	return projectsList, true, nil
}

func executeClaimPhase(ctx context.Context, phase string, input defaultSchedulerTickInput, discoveredRunnableIDs map[string]struct{}, alwaysLog bool) (int, int, error) {
	if input.ClaimMu != nil {
		input.ClaimMu.Lock()
		defer input.ClaimMu.Unlock()
	}
	if input.ClaimBoundary != nil {
		input.ClaimBoundary.RLock()
		defer input.ClaimBoundary.RUnlock()
	}
	if input.RefreshForClaim != nil {
		input = input.RefreshForClaim()
	}
	// Re-check after RefreshForClaim so a mid-tick catalog snapshot cannot open
	// claims/reconcile after admission closed. Discovery/HITL recheck via
	// admissionRefuseWork around each work-producing lane as well.
	if err := admissionRefuseWork(input); err != nil {
		return 0, 0, nil
	}
	start := time.Now()
	availableSlots, err := schedulerAvailableSlots(ctx, input.Repos, input.MaxConcurrentRuns)
	if err != nil {
		logClaimPhase(input.Logger, phase, 0, 0, time.Since(start), err)
		return 0, 0, err
	}
	claimedItems := make([]storage.QueueItemRecord, 0)
	if availableSlots > 0 && input.Repos != nil && input.Repos.Queue != nil {
		claimedItems, err = claimAndRunScheduledQueueItems(ctx, availableSlots, input)
		if err == nil {
			requestWakeForClaimedDiscovery(claimedItems, discoveredRunnableIDs, input.RequestSchedulerWake)
		}
	}
	claimedCount := len(claimedItems)
	if alwaysLog || availableSlots > 0 || claimedCount > 0 || err != nil {
		logClaimPhase(input.Logger, phase, availableSlots, claimedCount, time.Since(start), err)
	}
	return claimedCount, availableSlots, err
}

func logClaimPhase(logger bootstrap.Logger, phase string, availableSlots, claimedCount int, duration time.Duration, err error) {
	if logger == nil {
		return
	}
	fields := map[string]any{"phase": phase, "availableSlots": availableSlots, "claimedCount": claimedCount, "durationMs": duration.Milliseconds()}
	if err != nil {
		fields["error"] = err.Error()
	}
	logger.Debug("scheduler claim phase", fields)
}

func runSchedulerLane(input defaultSchedulerTickInput, laneName, projectID, repo string, fn func() error) error {
	start := time.Now()
	if input.Logger != nil {
		input.Logger.Debug("scheduler lane start", map[string]any{"lane": laneName, "projectId": projectID, "repo": repo})
	}
	err := fn()
	if input.Logger != nil {
		fields := map[string]any{"lane": laneName, "projectId": projectID, "repo": repo, "durationMs": time.Since(start).Milliseconds()}
		if err != nil {
			fields["error"] = err.Error()
		}
		input.Logger.Debug("scheduler lane end", fields)
		if threshold := schedulerSlowLaneWarnThreshold(input); threshold > 0 && time.Since(start) >= threshold {
			input.Logger.Warn("scheduler lane slow", fields)
		}
	}
	return err
}

func schedulerSlowLaneWarnThreshold(input defaultSchedulerTickInput) time.Duration {
	if input.Config == nil || input.Config.Scheduler.SlowLaneWarnThresholdMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(input.Config.Scheduler.SlowLaneWarnThresholdMS) * time.Millisecond
}

func schedulerAvailableSlots(ctx context.Context, repos *storage.Repositories, maxConcurrentRuns int) (int, error) {
	if repos == nil || repos.Queue == nil {
		return 0, nil
	}
	if maxConcurrentRuns <= 0 {
		return 0, nil
	}
	runningCount, err := repos.Queue.CountByStatus(ctx, "running")
	if err != nil {
		return 0, err
	}
	available := maxConcurrentRuns - int(runningCount)
	if available < 0 {
		return 0, nil
	}
	return available, nil
}

// ownedQueueClaim is one durable running claim held under a Supervisor
// operation lease until durable finalize (#579).
type ownedQueueClaim struct {
	item   storage.QueueItemRecord
	lease  OperationLease
	permit OperationPermit
}

// testAfterClaimHook, when set by tests, rewrites ClaimNext* results so the
// ambiguous cancel path (durable claim committed, error returned) can be
// exercised without racing SQLite QueryRowContext cancellation.
var testAfterClaimHook func(item *storage.QueueItemRecord, err error) (*storage.QueueItemRecord, error)

// claimTypeSetsFromInput partitions claimable queue types:
//   - unrestricted: live agent configured (or Config unset in unit tests), claim any item
//   - stickySnapshotOnly: runner present but live ResolveAgent fails; claim only when the
//     loop's latest run is failed/interrupted with a non-empty agent_snapshot_json vendor
//
// This keeps sticky retries claimable after vendor removal without starting fresh work
// for unconfigured roles with empty agent identity.
func claimTypeSetsFromInput(input defaultSchedulerTickInput) (unrestricted, stickySnapshotOnly []string) {
	addCodingRole := func(queueType string, runnerPresent bool) {
		if !runnerPresent {
			return
		}
		// Unit tests often omit Config and inject only the runners under test;
		// treat non-nil runners as fully claimable in that case.
		if input.Config == nil || config.CodingRoleAgentConfigured(*input.Config, queueType) {
			unrestricted = append(unrestricted, queueType)
			return
		}
		stickySnapshotOnly = append(stickySnapshotOnly, queueType)
	}
	addCodingRole("planner", input.Planner != nil)
	addCodingRole("reviewer", input.Reviewer != nil)
	addCodingRole("fixer", input.Fixer != nil)
	addCodingRole("worker", input.Worker != nil)
	if input.Snapshotter != nil {
		unrestricted = append(unrestricted, "snapshot")
	}
	return unrestricted, stickySnapshotOnly
}

func claimAndRunScheduledQueueItems(ctx context.Context, availableSlots int, input defaultSchedulerTickInput) ([]storage.QueueItemRecord, error) {
	if availableSlots <= 0 || input.Repos == nil || input.Repos.Queue == nil {
		return nil, nil
	}
	unrestrictedTypes, stickySnapshotTypes := claimTypeSetsFromInput(input)
	if len(unrestrictedTypes) == 0 && len(stickySnapshotTypes) == 0 {
		return nil, nil
	}
	now := input.Now
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	owned := make([]ownedQueueClaim, 0, availableSlots)
	queueItems := make([]storage.QueueItemRecord, 0, availableSlots)
	// Recheck admission at each durable claim so BeginShutdown cannot race past
	// an earlier pump-level AllowClaim and still ClaimNext under scheduler locks.
	// If admission closes mid-batch after some ClaimNext* already persisted
	// running/claimed items, stop claiming further slots but still process the
	// items already claimed — never return them un-dispatched (stranded).
	// The same invariant applies when BeginShutdown cancels the scheduler
	// context mid-batch: stop further ClaimNext*, but still dispatch already
	// durable claims (do not return the non-empty claimed slice with ctx.Err
	// before runScheduledQueueItems).
	//
	// When OperationOwner is set (#579): AdmitOperation before each ClaimNext*,
	// BindClaim after success (explicit permit), Release immediately on miss/
	// error, and never start a processor without a valid permit.
	admitClaim := func() error {
		if input.AllowClaim == nil {
			return nil
		}
		return input.AllowClaim()
	}
	// claimOne result: empty means this lane has no more candidates (try next
	// lane); stop means halt all further ClaimNext*; err is a hard claim failure.
	const (
		claimContinue = iota
		claimEmpty
		claimStop
	)
	claimOne := func(claimFn func(context.Context, string, string) (*storage.QueueItemRecord, error)) (result int, claimErr error) {
		if err := ctx.Err(); err != nil {
			return claimStop, nil
		}
		if err := admitClaim(); err != nil {
			return claimStop, nil
		}
		var lease OperationLease
		if input.OperationOwner != nil {
			var admitErr error
			lease, admitErr = input.OperationOwner.AdmitOperation(ctx, OperationMeta{ClaimedBy: "scheduler"})
			if admitErr != nil {
				// Admission closed between AllowClaim and AdmitOperation.
				return claimStop, nil
			}
		}
		item, err := claimFn(ctx, nowISO, "scheduler")
		if testAfterClaimHook != nil {
			item, err = testAfterClaimHook(item, err)
		}
		if err != nil {
			// Context cancel mid-ClaimNext can leave UPDATE...RETURNING committed
			// without a returned item. Recover (or retain/degrade) before Release
			// so a durable running claim is never left unowned (#579 / ADR-0015 R6).
			if lease != nil && claimErrorIsAmbiguousCancel(ctx, err) {
				recovered, recErr := recoverAmbiguousCancelledClaim(ctx, input, nowISO, owned)
				if recErr != nil {
					if input.OperationOwner != nil {
						input.OperationOwner.ReportHardPersistFailure(recErr)
					}
					// Retain lease — do not Release; BeginShutdown still observes it.
					return claimStop, errors.Join(ErrOperationFinalizeFailed, recErr)
				}
				if recovered != nil {
					item = recovered
					err = nil
				} else {
					// Confirmed no durable claim under this tick — safe to drop lease.
					lease.Release()
					// BeginShutdown may cancel mid-ClaimNext after earlier slots
					// succeeded. Stop claiming; dispatch already-durable claims.
					return claimStop, nil
				}
			}
		}
		if err != nil {
			if lease != nil {
				lease.Release()
			}
			// BeginShutdown may cancel mid-ClaimNext after earlier slots succeeded.
			// Stop claiming and dispatch already-durable claims instead of stranding.
			if ctx.Err() != nil {
				return claimStop, nil
			}
			return claimStop, err
		}
		if item == nil {
			if lease != nil {
				lease.Release()
			}
			// No candidate in this lane (e.g. non-long-term exhausted); caller may
			// continue with long-term retry lane.
			return claimEmpty, nil
		}
		if lease != nil {
			permit, bindErr := lease.BindClaim(*item)
			if bindErr != nil {
				// Cancelled lease never starts the processor. Durable-requeue
				// under retained ownership; Release only after requeue commits.
				if finErr := finalizeCancelledClaim(ctx, *item, input, now); finErr != nil {
					if input.OperationOwner != nil {
						input.OperationOwner.ReportHardPersistFailure(finErr)
					}
					// Retain ownership — do not Release; do not dispatch.
					return claimContinue, errors.Join(ErrOperationFinalizeFailed, finErr)
				}
				lease.Release()
				return claimContinue, nil
			}
			if !permit.Valid() {
				// Defensive: BindClaim must return a valid permit or error.
				if finErr := finalizeCancelledClaim(ctx, *item, input, now); finErr != nil {
					if input.OperationOwner != nil {
						input.OperationOwner.ReportHardPersistFailure(finErr)
					}
					return claimContinue, errors.Join(ErrOperationFinalizeFailed, finErr)
				}
				lease.Release()
				return claimContinue, nil
			}
			owned = append(owned, ownedQueueClaim{item: *item, lease: lease, permit: permit})
		} else {
			// Tests without OperationOwner: claim without lease ownership.
			owned = append(owned, ownedQueueClaim{item: *item})
		}
		queueItems = append(queueItems, *item)
		return claimContinue, nil
	}

	projectScopedTypes, runnableProjectIDs := codingClaimProjectScope(input.Config)
	stopClaiming := false
	for i := 0; i < availableSlots; i++ {
		result, err := claimOne(func(ctx context.Context, nowISO, claimedBy string) (*storage.QueueItemRecord, error) {
			return input.Repos.Queue.ClaimNextNonLongTermRetryAmongTypeSetsForProjects(ctx, nowISO, claimedBy, unrestrictedTypes, stickySnapshotTypes, projectScopedTypes, runnableProjectIDs)
		})
		if err != nil {
			return queueItems, dispatchOwnedQueueClaims(ctx, owned, input, err)
		}
		if result == claimEmpty {
			break
		}
		if result == claimStop {
			stopClaiming = true
			break
		}
	}
	for !stopClaiming && len(queueItems) < availableSlots {
		result, err := claimOne(func(ctx context.Context, nowISO, claimedBy string) (*storage.QueueItemRecord, error) {
			return input.Repos.Queue.ClaimNextLongTermRetryAmongTypeSetsForProjects(ctx, nowISO, claimedBy, unrestrictedTypes, stickySnapshotTypes, projectScopedTypes, runnableProjectIDs)
		})
		if err != nil {
			return queueItems, dispatchOwnedQueueClaims(ctx, owned, input, err)
		}
		if result == claimEmpty || result == claimStop {
			break
		}
	}
	return queueItems, dispatchOwnedQueueClaims(ctx, owned, input, nil)
}

func codingClaimProjectScope(cfg *config.Config) ([]string, []string) {
	if cfg == nil {
		return nil, nil
	}
	projectIDs := make([]string, 0, len(cfg.Projects))
	for _, project := range cfg.Projects {
		projectIDs = append(projectIDs, project.ID)
	}
	return []string{"fixer", "worker"}, projectIDs
}

// claimErrorIsAmbiguousCancel reports whether a ClaimNext* error may have
// already committed a durable running claim that was not returned to the caller
// (context cancelled during UPDATE...RETURNING scan).
func claimErrorIsAmbiguousCancel(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return ctx != nil && ctx.Err() != nil
}

// ambiguousClaimRecoveryTimeout bounds ListRunningClaimedBy after ClaimNext
// cancel/deadline. Recovery must detach the caller deadline (an expired tick can
// still observe a committed claim) without hanging forever if SQLite is blocked
// while the operation lease remains pending.
const ambiguousClaimRecoveryTimeout = 5 * time.Second

// newAmbiguousClaimRecoveryContext returns a cancel-detached context with a
// fresh timeout. Parent values are preserved when parent is non-nil; cancel and
// deadline from the parent are not inherited.
func newAmbiguousClaimRecoveryContext(parent context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, ambiguousClaimRecoveryTimeout)
}

// recoverAmbiguousCancelledClaim looks up running claims made by the scheduler
// at nowISO that are not already owned by this tick or by any live registry
// operation lease. Zero matches means the cancel happened before any durable
// claim (safe to release). One match is the recovered item. Multiple unowned
// matches cannot be bound to a single lease.
//
// Filtering only the current batch's owned slice is insufficient: two claim
// batches can share the same formatted nowISO (back-to-back wakeups within one
// millisecond) and both use claimed_by="scheduler", so ListRunningClaimedBy
// also returns earlier still-running claims. Those must be excluded via
// OperationOwner.OwnsQueueClaim so recovery never adopts a registry-owned row
// or degrades on a multi-match that includes live ownership.
func recoverAmbiguousCancelledClaim(ctx context.Context, input defaultSchedulerTickInput, nowISO string, owned []ownedQueueClaim) (*storage.QueueItemRecord, error) {
	if input.Repos == nil || input.Repos.Queue == nil {
		return nil, errors.New("queue repository is not configured for ambiguous claim recovery")
	}
	// Bound recovery: WithoutCancel alone would wait indefinitely on blocked
	// SQLite while the lease stays pending and shutdown times out retaining storage.
	recoverCtx, cancel := newAmbiguousClaimRecoveryContext(ctx)
	defer cancel()
	items, err := input.Repos.Queue.ListRunningClaimedBy(recoverCtx, "scheduler", nowISO)
	if err != nil {
		return nil, err
	}
	ownedIDs := make(map[string]struct{}, len(owned))
	for _, o := range owned {
		if o.item.ID != "" {
			ownedIDs[o.item.ID] = struct{}{}
		}
	}
	var found *storage.QueueItemRecord
	for i := range items {
		id := items[i].ID
		if _, ok := ownedIDs[id]; ok {
			continue
		}
		// Prior-batch claims with the same claimed_at share this query result;
		// skip any still bound to a live operation lease in the registry.
		if input.OperationOwner != nil && input.OperationOwner.OwnsQueueClaim(id) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: multiple unowned running claims after cancelled ClaimNext", ErrOperationFinalizeFailed)
		}
		item := items[i]
		found = &item
	}
	return found, nil
}

// cancelledClaimFinalizeTimeout bounds MarkRetryIfRunning/GetByID after a
// stop/bind refuse. Finalize must detach the caller cancel/deadline (shutdown
// or a timed tick can still require durable requeue) without hanging forever if
// SQLite is blocked while the operation lease remains pending.
const cancelledClaimFinalizeTimeout = 5 * time.Second

// newCancelledClaimFinalizeContext returns a cancel-detached context with a
// fresh timeout. Parent values are preserved when parent is non-nil; cancel and
// deadline from the parent are not inherited.
func newCancelledClaimFinalizeContext(parent context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, cancelledClaimFinalizeTimeout)
}

// finalizeCancelledClaim durable-requeues a claim that cannot start because the
// operation lease was cancelled (stop/bind race). Keeps the claim out of the
// stranded running state without starting a processor.
//
// Uses a cancellation-detached, time-bounded context: Runtime.BeginShutdown
// cancels the scheduler context before the registry drain, so a bind refused
// after ClaimNext* must still requeue under retained ownership (same pattern as
// dispatchOwnedQueueClaims / ambiguous-claim recovery). Passing the cancelled
// scheduler ctx would make MarkRetry fail immediately, report hard persist
// failure, and strand the claim. WithoutCancel alone would drop a caller
// deadline and wait indefinitely on blocked SQLite while the lease stays pending.
//
// Pause/terminal stop may CancelByLoop the claim to cancelled after ClaimNext*
// and before BindClaim refuses. Use MarkRetryIfRunning so concurrent
// terminalization yields a zero-row no-op treated as durable success (stop must
// not resurrect cancelled work). Unrestricted MarkRetry is reserved for runner
// finalize after processing, which may requeue after mid-run pause cancel.
func finalizeCancelledClaim(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput, now func() time.Time) error {
	if input.Repos == nil || input.Repos.Queue == nil {
		return errors.New("queue repository is not configured for cancelled-claim finalize")
	}
	// Bound finalization: detach cancel/deadline then apply a fresh timeout so
	// shutdown/timed ticks still requeue without unbounded DB waits under a
	// retained operation lease.
	ctx, cancel := newCancelledClaimFinalizeContext(ctx)
	defer cancel()
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	msg := "operation lease cancelled before queue processor start"
	// Atomic status-guarded requeue: only updates while still running. Zero rows
	// (already cancelled/completed/failed) is success — do not resurrect.
	if err := input.Repos.Queue.MarkRetryIfRunning(ctx, storage.QueueMarkRetryInput{
		ID:           item.ID,
		AvailableAt:  nowISO,
		Attempts:     item.Attempts,
		ErrorMessage: &msg,
		ErrorKind:    "retryable_transient",
		UpdatedAt:    nowISO,
	}); err != nil {
		return err
	}
	// Verify durable observation: still-running after guarded requeue means
	// the write did not take effect (e.g. archived project) and ownership must
	// not pretend finalize succeeded.
	got, err := input.Repos.Queue.GetByID(ctx, item.ID)
	if err != nil {
		return err
	}
	if got == nil || got.Status == "running" {
		return fmt.Errorf("%w: claim %s still running after requeue", ErrOperationFinalizeFailed, item.ID)
	}
	return nil
}

// dispatchOwnedQueueClaims launches processors for items already durable as
// running/claimed under operation leases. Always detach the scheduler cancel:
// BeginShutdown may race after a clean ctx.Err() check here but before
// ProcessClaimedQueueItem runs its storage reads and Fail/Complete recovery
// writes. Relying on cancellation already being visible at this exact check
// strands claims as running/claimed_by=scheduler until a later recovery pass.
func dispatchOwnedQueueClaims(ctx context.Context, owned []ownedQueueClaim, input defaultSchedulerTickInput, priorErr error) error {
	if len(owned) == 0 {
		return priorErr
	}
	dispatchCtx := ctx
	if ctx != nil {
		dispatchCtx = context.WithoutCancel(ctx)
	}
	runErr := runOwnedQueueClaims(dispatchCtx, owned, input)
	return errors.Join(runErr, priorErr)
}

// schedulerLoopParked reports whether a claimed queue item's loop was parked
// (human takeover / paused) — a state the scheduler may observe AFTER the claim
// due to a race, and must then decline to run.
func schedulerLoopParked(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput) bool {
	if item.LoopID == nil || input.Repos == nil || input.Repos.Loops == nil {
		return false
	}
	loop, err := input.Repos.Loops.GetByID(ctx, *item.LoopID)
	if err != nil || loop == nil {
		return false
	}
	switch loop.Status {
	case "human_takeover", "paused":
		return true
	default:
		return false
	}
}

func runOwnedQueueClaims(ctx context.Context, owned []ownedQueueClaim, input defaultSchedulerTickInput) error {
	if len(owned) == 0 {
		return nil
	}

	now := input.Now
	if now == nil {
		now = time.Now
	}
	errList := make([]error, 0)
	// Do not abort the launch loop on ctx cancel: every item is already durable
	// as claimed/running under an operation lease. Skipping remaining launches
	// strands those claims (BeginShutdown cancels the scheduler context during drain).
	for _, claim := range owned {
		item := claim.item
		lease := claim.lease
		permit := claim.permit

		// A loop parked for human takeover (or paused) must never be run, even if a
		// queue item survived a race with the parking and got claimed. Durable-
		// complete the claim (so the slot frees) then Release the lease — only
		// an explicit handback re-arms it.
		if schedulerLoopParked(ctx, item, input) {
			if err := durableCompleteClaim(ctx, item, input, now); err != nil {
				errList = append(errList, err)
				if lease != nil {
					if input.OperationOwner != nil {
						input.OperationOwner.ReportHardPersistFailure(err)
					}
					// Retain ownership on finalize failure.
				}
			} else if lease != nil {
				lease.Release()
			}
			if input.Logger != nil {
				loopID := ""
				if item.LoopID != nil {
					loopID = *item.LoopID
				}
				input.Logger.Info("scheduler released claimed item for parked loop", map[string]any{"queueItemId": item.ID, "loopId": loopID})
			}
			continue
		}

		// Do not ClearLoopStop here. A claim can race with looper stop: pass the
		// parked check while still running, then BeginLoopStop closes admission
		// before launch. Unconditional clear would reopen the sticky gate for
		// that pre-stop claim. Intentional re-activation (API unpause/retry/
		// handback) is the authority that clears the gate.

		// Stop/bind race: never start the processor without an explicit permit
		// when OperationOwner is wired. Also refuse when the live lease was
		// cancelled after BindClaim (BeginLoopStop / BeginShutdown): permit
		// remains Valid() but the operation must not start. (Tests without
		// owner skip this gate.)
		leaseCancelled := lease != nil && lease.Context().Err() != nil
		if input.OperationOwner != nil && (!permit.Valid() || leaseCancelled) {
			if finErr := finalizeCancelledClaim(ctx, item, input, now); finErr != nil {
				errList = append(errList, finErr)
				if input.OperationOwner != nil {
					input.OperationOwner.ReportHardPersistFailure(finErr)
				}
			} else if lease != nil {
				lease.Release()
			}
			continue
		}

		process, err := schedulerQueueProcessor(item, input)
		if err != nil {
			// Processor missing: still durable-finalize when ownership is tracked
			// so the claim is not left running under a released lease.
			if lease != nil {
				if finErr := ensureClaimFinalized(ctx, item, err, input, now); finErr != nil {
					errList = append(errList, errors.Join(err, finErr))
					if input.OperationOwner != nil {
						input.OperationOwner.ReportHardPersistFailure(finErr)
					}
					// Retain ownership when finalize fails.
					continue
				}
				lease.Release()
			}
			errList = append(errList, err)
			continue
		}
		processFn := process
		run := func() {
			// Processor work is owned by the operation lease, not the detached
			// scheduler dispatch context. dispatchOwnedQueueClaims uses
			// context.WithoutCancel so finalize survives BeginShutdown, but
			// that must not keep the processor alive after the lease is
			// cancelled (post-bind stop race, including delayed AsyncRunner).
			processCtx := ctx
			if lease != nil {
				processCtx = lease.Context()
				if processCtx.Err() != nil {
					if finErr := finalizeCancelledClaim(ctx, item, input, now); finErr != nil {
						if input.OperationOwner != nil {
							input.OperationOwner.ReportHardPersistFailure(finErr)
						}
						if input.Logger != nil {
							input.Logger.Warn("scheduler queue finalize failed; retaining operation lease", map[string]any{
								"type": item.Type, "queueItemId": item.ID, "error": finErr.Error(),
							})
						}
						// Retain ownership — do not Release.
						return
					}
					lease.Release()
					return
				}
			}
			runErr := processFn(processCtx)
			if lease != nil {
				// Finalize-before-release: only drop ownership after durable
				// complete/cancel/requeue (or typed Fail recovery). Keep using
				// the detached dispatch ctx so durable writes still succeed
				// when the lease was cancelled mid-run.
				//
				// When the lease was cancelled mid-run (BeginLoopStop before
				// CancelByLoop / Terminate), do not Fail→manual_intervention.
				// That status is outside CancelByLoop's WHERE (queued|running),
				// so haltLoop would leave work on a terminated loop in a
				// non-cancelled terminal state. Status-guarded requeue preserves
				// concurrent CancelByLoop (zero-row no-op if already cancelled)
				// and keeps the row cancellable until Terminate runs.
				var finErr error
				if lease.Context().Err() != nil {
					finErr = finalizeCancelledClaim(ctx, item, input, now)
				} else {
					finErr = ensureClaimFinalized(ctx, item, runErr, input, now)
				}
				if finErr != nil {
					if input.OperationOwner != nil {
						input.OperationOwner.ReportHardPersistFailure(finErr)
					}
					if input.Logger != nil {
						input.Logger.Warn("scheduler queue finalize failed; retaining operation lease", map[string]any{
							"type": item.Type, "queueItemId": item.ID, "error": finErr.Error(),
						})
					}
					// Retain ownership — do not Release.
					return
				}
				lease.Release()
			}
			if runErr != nil && input.Logger != nil {
				input.Logger.Warn("scheduler queue item failed", map[string]any{"type": item.Type, "queueItemId": item.ID, "error": runErr.Error()})
			}
		}

		if input.AsyncRunner != nil {
			input.AsyncRunner.Go(run)
			continue
		}
		go run()
	}
	if len(errList) == 0 {
		return nil
	}
	return errors.Join(errList...)
}

// durableCompleteClaim writes queue status completed and verifies the durable
// observation left the running state.
//
// ErrQueueItemNotActive is not a hard persistence failure when the row is
// already non-running: pause/terminate CancelByLoop can concurrent-terminalize
// a claim to cancelled while the scheduler observes a parked loop and tries to
// Complete. That race must free the operation lease rather than degrade and
// retain ownership forever.
func durableCompleteClaim(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput, now func() time.Time) error {
	if input.Repos == nil || input.Repos.Queue == nil {
		return errors.New("queue repository is not configured")
	}
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	if err := input.Repos.Queue.Complete(ctx, item.ID, nowISO); err != nil {
		if !errors.Is(err, storage.ErrQueueItemNotActive) {
			return err
		}
		// Concurrent terminalization (e.g. CancelByLoop) already moved the claim
		// off running — verify and treat as durable success for lease release.
		got, getErr := input.Repos.Queue.GetByID(ctx, item.ID)
		if getErr != nil {
			return getErr
		}
		if got == nil || got.Status != "running" {
			return nil
		}
		return fmt.Errorf("%w: claim %s still running after complete conflict", ErrOperationFinalizeFailed, item.ID)
	}
	got, err := input.Repos.Queue.GetByID(ctx, item.ID)
	if err != nil {
		return err
	}
	if got == nil || got.Status == "running" {
		return fmt.Errorf("%w: claim %s still running after complete", ErrOperationFinalizeFailed, item.ID)
	}
	return nil
}

// ensureClaimFinalized guarantees typed durable finalization after a runner
// returns. Happy-path runners already Complete/Fail; this closes the gap where
// a runner error leaves status=running under a about-to-be-released lease.
// Returns nil when the claim is no longer running (already finalized).
func ensureClaimFinalized(ctx context.Context, item storage.QueueItemRecord, runErr error, input defaultSchedulerTickInput, now func() time.Time) error {
	if input.Repos == nil || input.Repos.Queue == nil {
		if runErr != nil {
			return errors.New("queue repository is not configured for finalize")
		}
		return nil
	}
	got, err := input.Repos.Queue.GetByID(ctx, item.ID)
	if err != nil {
		return err
	}
	if got == nil || got.Status != "running" {
		return nil
	}
	// Worker has durably completed every execution step but its atomic
	// run/queue/loop terminal transaction failed. Requeueing here would repeat
	// already-published work; retain the operation lease and degrade instead.
	if errors.Is(runErr, worker.ErrSuccessfulClaimFinalization) {
		return errors.Join(ErrOperationFinalizeFailed, runErr)
	}
	// Still running after processor return: typed durable finalization.
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	msg := "queue processor returned without durable finalize"
	if runErr != nil {
		msg = runErr.Error()
	}
	kind := "retryable_transient"
	if runErr == nil {
		kind = "non_retryable"
	}
	if err := input.Repos.Queue.Fail(ctx, storage.QueueFailInput{
		ID:           item.ID,
		Attempts:     got.Attempts,
		FinishedAt:   nowISO,
		ErrorMessage: &msg,
		ErrorKind:    kind,
		UpdatedAt:    nowISO,
	}); err != nil {
		return err
	}
	after, err := input.Repos.Queue.GetByID(ctx, item.ID)
	if err != nil {
		return err
	}
	if after == nil || after.Status == "running" {
		return fmt.Errorf("%w: claim %s still running after fail", ErrOperationFinalizeFailed, item.ID)
	}
	return nil
}

func schedulerQueueProcessor(item storage.QueueItemRecord, input defaultSchedulerTickInput) (func(context.Context) error, error) {
	switch item.Type {
	case "planner":
		if input.Planner == nil {
			return nil, fmt.Errorf("planner runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Planner.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "reviewer":
		if input.Reviewer == nil {
			return nil, fmt.Errorf("reviewer runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Reviewer.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "fixer":
		if input.Fixer == nil {
			return nil, fmt.Errorf("fixer runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Fixer.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "worker":
		if input.Worker == nil {
			return nil, fmt.Errorf("worker runner is not configured")
		}
		return func(ctx context.Context) error {
			_, err := input.Worker.ProcessClaimedQueueItem(ctx, item)
			return wrapSchedulerQueueError(item.Type, err)
		}, nil
	case "snapshot":
		if input.Snapshotter == nil || input.Repos == nil || input.Repos.Queue == nil || input.Repos.PullRequestSnapshots == nil {
			return nil, fmt.Errorf("snapshot runner is not configured")
		}
		return func(ctx context.Context) error {
			return processSnapshotQueueItem(ctx, item, input)
		}, nil
	default:
		return nil, fmt.Errorf("unsupported queue item type %q", item.Type)
	}
}

func processSnapshotQueueItem(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput) error {
	if item.ProjectID == nil || strings.TrimSpace(*item.ProjectID) == "" || item.Repo == nil || strings.TrimSpace(*item.Repo) == "" || item.PRNumber == nil {
		return failSnapshotQueueItem(ctx, item, input, "invalid snapshot queue item", "non_retryable")
	}
	project, err := input.Repos.Projects.GetByID(ctx, *item.ProjectID)
	if err != nil {
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	if project == nil {
		return failSnapshotQueueItem(ctx, item, input, "project not found", "non_retryable")
	}
	cwd := project.RepoPath
	if item.PayloadJSON != nil && strings.TrimSpace(*item.PayloadJSON) != "" {
		payload := map[string]any{}
		if err := json.Unmarshal([]byte(*item.PayloadJSON), &payload); err == nil {
			if payloadCWD, _ := payload["cwd"].(string); strings.TrimSpace(payloadCWD) != "" {
				cwd = strings.TrimSpace(payloadCWD)
			}
		}
	}
	now := input.Now
	if now == nil {
		now = time.Now
	}
	transportRepo, err := reviewThreadRepoForProject(input.Config, *item.ProjectID, *item.Repo)
	if err != nil {
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	snapshot, err := input.Snapshotter.CapturePullRequestSnapshot(ctx, githubinfra.CapturePullRequestSnapshotInput{ProjectID: project.ID, Repo: *item.Repo, TransportRepo: transportRepo, PRNumber: *item.PRNumber, CWD: cwd, CapturedAt: formatJavaScriptISOString(now().UTC())})
	if err != nil {
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	if err := input.Repos.PullRequestSnapshots.Upsert(ctx, snapshot); err != nil {
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	if err := input.Repos.Queue.Complete(ctx, item.ID, formatJavaScriptISOString(now().UTC())); err != nil {
		if errors.Is(err, storage.ErrQueueItemNotActive) {
			return nil
		}
		return failSnapshotQueueItem(ctx, item, input, err.Error(), "retryable_transient")
	}
	return nil
}

func failSnapshotQueueItem(ctx context.Context, item storage.QueueItemRecord, input defaultSchedulerTickInput, message, kind string) error {
	now := input.Now
	if now == nil {
		now = time.Now
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	nextAttempts := item.Attempts + 1
	if kind == "retryable_transient" && (item.MaxAttempts < 0 || (item.MaxAttempts > 0 && nextAttempts < item.MaxAttempts)) {
		retryAt := formatJavaScriptISOString(now().UTC().Add(time.Minute * time.Duration(cappedRetryDelayAttempt(nextAttempts, item.MaxAttempts))))
		return input.Repos.Queue.MarkRetry(ctx, storage.QueueMarkRetryInput{ID: item.ID, AvailableAt: retryAt, Attempts: nextAttempts, ErrorMessage: &message, ErrorKind: kind, UpdatedAt: nowISO})
	}
	return input.Repos.Queue.Fail(ctx, storage.QueueFailInput{ID: item.ID, Attempts: nextAttempts, FinishedAt: nowISO, ErrorMessage: &message, ErrorKind: kind, UpdatedAt: nowISO})
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

func repoFromProjectMetadata(metadataJSON *string) string {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return ""
	}
	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(*metadataJSON), &metadata); err != nil {
		return ""
	}
	repo, _ := metadata["repo"].(string)
	return strings.TrimSpace(repo)
}

func catalogBindingMatchesProject(binding config.ProjectRefConfig, project storage.ProjectRecord) bool {
	metadata := map[string]any{}
	if project.MetadataJSON != nil && strings.TrimSpace(*project.MetadataJSON) != "" {
		if err := json.Unmarshal([]byte(*project.MetadataJSON), &metadata); err != nil {
			return false
		}
	}
	provider, _ := metadata["provider"].(string)
	return strings.TrimSpace(binding.Provider) == strings.TrimSpace(provider) &&
		strings.TrimSpace(binding.Repo) == repoFromProjectMetadata(project.MetadataJSON)
}

func wrapSchedulerError(action, projectID, repo string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s failed for project %s (%s): %w", action, projectID, repo, err)
}

func wrapSchedulerQueueError(queueType string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s processing failed: %w", queueType, err)
}

func runtimeFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func boolPtr(value bool) *bool {
	return &value
}

func summarizeCheckStates(checks []map[string]any) string {
	if len(checks) == 0 {
		return ""
	}
	states := make([]string, 0, len(checks))
	for _, check := range checks {
		state, _ := check["conclusion"].(string)
		if strings.TrimSpace(state) == "" {
			state, _ = check["state"].(string)
		}
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		states = append(states, state)
	}
	return strings.Join(states, ", ")
}
