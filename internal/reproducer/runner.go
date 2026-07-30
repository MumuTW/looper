// Package reproducer turns a bug Issue into an executable failure before any
// planning happens.
//
// It runs between Triager and Planner. Ordering is the point: "cannot
// reproduce" stops the Issue before any planning cost is paid, and when a
// reproduction does exist Planner plans against a demonstrated failure rather
// than a prose description. Reproducer creates the branch Planner will adopt
// and commits the reproduction onto it, so Planner's spec is authored on top.
//
// It is bug-only. A non-bug Triage Report keeps today's path exactly: feature
// work is already served by Planner's spec and acceptance criteria, and folding
// it in here would only rename Planner.
package reproducer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/triager"
	"github.com/nexu-io/looper/internal/worktreesafety"
)

const (
	// DefaultDecisionLimit bounds reproduction agent runs per tick, matching
	// Triager: a reproduction is an agent turn plus a command execution, which
	// is far too expensive to fan out across every open bug in one pass.
	DefaultDecisionLimit = 1

	// CannotReproduceRelPath is the sentinel the agent writes when it cannot
	// make the reported bug fail. It is a separate file from the manifest so
	// "I could not reproduce" can never be mistaken for a malformed manifest.
	CannotReproduceRelPath = ".looper/cannot-reproduce.json"

	// AnswerProceed is the option a human picks to let Planner proceed without
	// a reproduction. Anything else settles the Issue.
	AnswerProceed = "plan without a reproduction"
	// AnswerReject is the option that leaves the Issue stopped.
	AnswerReject = "leave this Issue for a human"
)

// CannotReproduce is the agent-authored structured record. The daemon does not
// second-guess its content: it is the agent reporting facts about what it ran,
// and the policy decision (stop, ask a human) is the daemon's.
type CannotReproduce struct {
	Version            int      `json:"version"`
	Attempted          []string `json:"attempted"`
	ObservedInstead    string   `json:"observedInstead"`
	MissingInformation []string `json:"missingInformation,omitempty"`
	Summary            string   `json:"summary,omitempty"`
}

// GitHubGateway is the read-only Issue surface Reproducer needs.
type GitHubGateway interface {
	ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error)
}

// GitGateway and AgentExecutor are expressed over Planner's types on purpose:
// Reproducer prepares Planner's branch, so it must speak to the same adapters
// with the same safety context rather than acquiring a parallel set.
type GitGateway interface {
	CreateWorktree(context.Context, planner.CreateWorktreeInput) (planner.CreateWorktreeResult, error)
	InspectHead(context.Context, planner.InspectHeadInput) (planner.InspectHeadResult, error)
	Commit(context.Context, planner.CommitInput) (planner.CommitResult, error)
}

// AgentExecutor starts the reproduction agent session.
type AgentExecutor interface {
	Start(context.Context, planner.AgentRunInput) (planner.AgentExecution, error)
}

// PlannerHandoff is the Planner surface Reproducer uses to park an Issue.
type PlannerHandoff interface {
	ParkIssueForHuman(context.Context, planner.ParkIssueInput) (storage.LoopRecord, error)
}

// Options configures the Runner.
type Options struct {
	Repos                  *storage.Repositories
	GitHub                 GitHubGateway
	Git                    GitGateway
	AgentExecutor          AgentExecutor
	Planner                PlannerHandoff
	Now                    func() time.Time
	AgentTimeout           time.Duration
	AgentIdleTimeout       time.Duration
	CommandTimeout         time.Duration
	ValidationCodexCommand string
	ContainmentTracker     processcontainment.LiveTracker
	DecisionLimit          int

	// proveFailing is an in-package test seam. Production callers leave it nil
	// so repository-controlled commands stay inside the validation sandbox.
	proveFailing func(context.Context, reproduction.Input) (reproduction.Result, error)
}

// Runner is the Reproducer Role.
type Runner struct {
	repos                  *storage.Repositories
	github                 GitHubGateway
	git                    GitGateway
	agentExecutor          AgentExecutor
	planner                PlannerHandoff
	now                    func() time.Time
	agentTimeout           time.Duration
	agentIdleTimeout       time.Duration
	commandTimeout         time.Duration
	validationCodexCommand string
	containmentTracker     processcontainment.LiveTracker
	decisionLimit          int
	proveFailing           func(context.Context, reproduction.Input) (reproduction.Result, error)
}

// DiscoveryInput is one lane pass over one project.
type DiscoveryInput struct {
	ProjectID      string
	Repo           string
	Snapshot       *githubinfra.DiscoverySnapshot
	DecisionBudget *int
}

// DiscoveryResult counts what the pass did. Unreproducible is reported
// separately from Skipped so an operator can tell "nothing to do" from
// "something stopped for a human".
type DiscoveryResult struct {
	Attempted      int
	Reproduced     int
	Unreproducible int
	Waived         int
	Skipped        int
	QueueItems     []storage.QueueItemRecord
}

// New builds a Reproducer.
func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	decisionLimit := options.DecisionLimit
	if decisionLimit <= 0 {
		decisionLimit = DefaultDecisionLimit
	}
	return &Runner{
		repos: options.Repos, github: options.GitHub, git: options.Git,
		agentExecutor: options.AgentExecutor, planner: options.Planner, now: now,
		agentTimeout: options.AgentTimeout, agentIdleTimeout: options.AgentIdleTimeout,
		commandTimeout: options.CommandTimeout, validationCodexCommand: options.ValidationCodexCommand,
		containmentTracker: options.ContainmentTracker, decisionLimit: decisionLimit,
		proveFailing: options.proveFailing,
	}
}

// DiscoverIssues is the lane entry point. It reads accepted Triage Reports,
// selects the bug-classified ones with no settled reproduction, and reproduces
// at most DecisionLimit of them.
func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	if r.repos == nil || r.repos.Projects == nil || r.repos.Events == nil {
		return DiscoveryResult{}, fmt.Errorf("reproducer repositories are not configured")
	}
	if r.github == nil || r.git == nil || r.agentExecutor == nil || r.planner == nil {
		return DiscoveryResult{}, fmt.Errorf("reproducer dependencies are not configured")
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

	candidates, err := r.loadCandidates(ctx, input.ProjectID, input.Repo)
	if err != nil {
		return DiscoveryResult{}, err
	}
	statuses, err := reproduction.LoadStatuses(ctx, r.repos, input.ProjectID, input.Repo)
	if err != nil {
		return DiscoveryResult{}, err
	}

	result := DiscoveryResult{}
	for _, candidate := range candidates {
		status := statuses[candidate.IssueNumber]
		if status.Unreproducible != nil && status.Waived == nil {
			waived, err := r.settleParkedIssue(ctx, *status.Unreproducible)
			if err != nil {
				return result, err
			}
			if waived {
				result.Waived++
			} else {
				result.Skipped++
			}
			continue
		}
		if status.Settled() {
			result.Skipped++
			continue
		}
		if result.Attempted >= r.decisionLimit || (input.DecisionBudget != nil && *input.DecisionBudget <= 0) {
			result.Skipped++
			continue
		}
		result.Attempted++
		if input.DecisionBudget != nil {
			*input.DecisionBudget--
		}
		if err := r.reproduce(ctx, *project, input.Repo, candidate, status, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// candidate is one bug Issue authorized by an accepted Triage Report.
type candidate struct {
	IssueNumber    int64
	TriageKey      string
	IdempotencyKey string
	CreatedAt      string
}

// loadCandidates selects the Issues Reproducer is responsible for.
//
// The selection is deliberately narrow. A Triage Report is required, so an
// Issue that reached Planner through label/assignee discovery with no report
// has no candidate here and keeps today's path unchanged. Only `bug`
// classifications qualify.
func (r *Runner) loadCandidates(ctx context.Context, projectID, repo string) ([]candidate, error) {
	reports, err := triager.LoadAcceptedReports(ctx, r.repos, projectID, repo)
	if err != nil {
		return nil, err
	}
	candidates := make([]candidate, 0, len(reports))
	for _, report := range reports {
		if report.Decision.Classification != triager.ClassificationBug {
			continue
		}
		candidates = append(candidates, candidate{
			IssueNumber:    report.IssueNumber,
			TriageKey:      report.IdempotencyKey,
			IdempotencyKey: reproduction.IdempotencyKey(projectID, repo, report.IssueNumber, report.IdempotencyKey),
			CreatedAt:      report.CreatedAt,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt != candidates[j].CreatedAt {
			return candidates[i].CreatedAt < candidates[j].CreatedAt
		}
		return candidates[i].IssueNumber < candidates[j].IssueNumber
	})
	return candidates, nil
}

// reproduce runs one reproduction attempt end to end.
func (r *Runner) reproduce(ctx context.Context, project storage.ProjectRecord, repo string, target candidate, status reproduction.Status, result *DiscoveryResult) error {
	detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: target.IssueNumber, CWD: project.RepoPath})
	if err != nil {
		return err
	}
	if !eligibleTarget(detail) || domain.IsAutoLaneHeld(domain.LoopTypePlanner, detail.Labels) {
		result.Skipped++
		return nil
	}
	branch := planner.BranchForIssue(detail.Number, detail.Title)

	if !status.Attempted {
		attempt := reproduction.Attempt{
			IdempotencyKey: target.IdempotencyKey, ProjectID: project.ID, Repo: repo,
			IssueNumber: detail.Number, Branch: branch, TriageReportID: target.TriageKey,
			AttemptedAt: r.nowISO(),
		}
		if err := reproduction.AppendAttempt(ctx, r.repos, attempt); err != nil {
			return err
		}
	}

	worktree, err := r.prepareWorktree(ctx, project, branch)
	if err != nil {
		return err
	}

	// Crash recovery: a run that committed a reproduction but died before
	// acknowledging it left the manifest on the branch. Adopt it rather than
	// authoring a second reproduction commit.
	if adopted, ok, err := r.adoptCommittedReproduction(ctx, project, repo, detail, worktree, target); err != nil {
		return err
	} else if ok {
		if err := reproduction.AppendRecord(ctx, r.repos, adopted); err != nil {
			return err
		}
		result.Reproduced++
		return nil
	}

	agentResult, err := r.runAgent(ctx, project, repo, detail, worktree)
	if err != nil {
		return err
	}
	if !strings.EqualFold(agentResult.Status, "completed") {
		// An incomplete agent turn is an ordinary transient condition, not a
		// verdict about the bug. Leave the attempt open so the next tick retries
		// without recording anything about reproducibility.
		result.Skipped++
		return nil
	}

	if declined, ok, err := readCannotReproduce(worktree.Path); err != nil {
		return err
	} else if ok {
		return r.recordUnreproducible(ctx, project, repo, detail, target, unreproducibleFrom(declined), result)
	}

	draft, ok, err := readDraft(worktree.Path)
	if err != nil {
		return err
	}
	if !ok {
		return r.recordUnreproducible(ctx, project, repo, detail, target, reproduction.Unreproducible{
			Summary:         "The reproduction agent produced neither a reproduction manifest nor a cannot-reproduce record.",
			ObservedInstead: strings.TrimSpace(agentResult.Summary),
		}, result)
	}

	hashes, err := reproduction.HashFiles(worktree.Path, draft.Files)
	if err != nil {
		return r.recordUnreproducible(ctx, project, repo, detail, target, reproduction.Unreproducible{
			Summary:         fmt.Sprintf("The reproduction manifest names files that cannot be hashed: %v", err),
			ObservedInstead: strings.TrimSpace(agentResult.Summary),
		}, result)
	}

	head, err := r.git.InspectHead(ctx, planner.InspectHeadInput{
		RepoPath: project.RepoPath, WorktreeRoot: worktree.Root, WorktreePath: worktree.Path,
	})
	if err != nil {
		return err
	}

	// The proof: the command must be observed failing on the current base. A
	// command that passes immediately is not a reproduction, no matter what the
	// agent says it wrote.
	proof, err := r.prove(ctx, reproduction.Input{
		WorktreePath: worktree.Path,
		Record:       reproduction.Record{Command: draft.Command, Files: hashes},
		Timeout:      r.commandTimeout,
		CodexCommand: r.validationCodexCommand,
		Tracker:      r.containmentTracker,
	})
	if err != nil {
		return err
	}
	if !proof.Passed {
		return r.recordUnreproducible(ctx, project, repo, detail, target, reproduction.Unreproducible{
			Reason:          proof.Reason,
			Summary:         proof.Summary,
			ObservedInstead: proof.Output,
		}, result)
	}

	manifest := reproduction.Manifest{
		Version: reproduction.ManifestVersion, Repo: repo, IssueNumber: detail.Number,
		Command: draft.Command, Files: hashes, ObservedFailure: truncate(proof.Output),
		BaseSHA: head.HeadSHA, IdempotencyKey: target.IdempotencyKey, RecordedAt: r.nowISO(),
	}
	if err := reproduction.WriteManifest(worktree.Path, manifest); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(worktree.Path, filepath.FromSlash(CannotReproduceRelPath))); err != nil && !os.IsNotExist(err) {
		return err
	}
	commit, err := r.git.Commit(ctx, planner.CommitInput{
		RepoPath: project.RepoPath, WorktreeRoot: worktree.Root, WorktreePath: worktree.Path,
		Message: fmt.Sprintf("test(reproduce): failing reproduction for %s#%d\n\n%s",
			repo, detail.Number, reproduction.CommitMarker(target.IdempotencyKey)),
	})
	if err != nil {
		return err
	}

	record := reproduction.Record{
		ProjectID: project.ID, Repo: repo, IssueNumber: detail.Number, Branch: branch,
		Command: manifest.Command, Files: hashes, CommitSHA: commit.CommitSHA, BaseSHA: head.HeadSHA,
		ObservedFailure: manifest.ObservedFailure, IdempotencyKey: target.IdempotencyKey,
		RecordedAt: r.nowISO(),
	}
	if err := reproduction.AppendRecord(ctx, r.repos, record); err != nil {
		return err
	}
	result.Reproduced++
	return nil
}

func (r *Runner) prove(ctx context.Context, input reproduction.Input) (reproduction.Result, error) {
	if r.proveFailing != nil {
		return r.proveFailing(ctx, input)
	}
	return reproduction.ProveFailing(ctx, input)
}

func (r *Runner) nowISO() string { return eventlog.FormatJavaScriptISOString(r.now()) }

func eligibleTarget(issue githubinfra.IssueDetail) bool {
	return issue.Number > 0 &&
		strings.EqualFold(strings.TrimSpace(issue.State), "open") &&
		!issue.IsPullRequest
}

func truncate(value string) string {
	value = strings.TrimSpace(value)
	const limit = 4000
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n…(truncated)"
}

// worktreeContext is the prepared branch Reproducer and Planner share.
type worktreeContext struct {
	ID     string
	Path   string
	Root   string
	Branch string
	Base   string
}

func (r *Runner) prepareWorktree(ctx context.Context, project storage.ProjectRecord, branch string) (worktreeContext, error) {
	root, err := worktreeRoot(project)
	if err != nil {
		return worktreeContext{}, err
	}
	baseBranch := "main"
	if project.BaseBranch != nil && strings.TrimSpace(*project.BaseBranch) != "" {
		baseBranch = strings.TrimSpace(*project.BaseBranch)
	}
	created, err := r.git.CreateWorktree(ctx, planner.CreateWorktreeInput{
		ProjectID: project.ID, RepoPath: project.RepoPath, WorktreeRoot: root,
		Branch: branch, BaseBranch: baseBranch, ProtectedBranches: []string{baseBranch},
	})
	if err != nil {
		return worktreeContext{}, err
	}
	if err := worktreesafety.Validate(worktreesafety.CheckInput{WorktreePath: created.WorktreePath, RepoPath: project.RepoPath, WorktreeRoot: root}); err != nil {
		return worktreeContext{}, err
	}
	base := created.BaseBranch
	if strings.TrimSpace(base) == "" {
		base = baseBranch
	}
	return worktreeContext{ID: created.ID, Path: created.WorktreePath, Root: root, Branch: created.Branch, Base: base}, nil
}

func worktreeRoot(project storage.ProjectRecord) (string, error) {
	metadata := parseJSONObject(project.MetadataJSON)
	if value, ok := metadata["worktreeRoot"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value), nil
	}
	return config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
}

// settleParkedIssue records the human's decision on a parked Issue exactly
// once. A human who resumed the parked loop has authorized planning without a
// reproduction; without this the only way forward would be disabling the Role
// for the whole project.
func (r *Runner) settleParkedIssue(ctx context.Context, record reproduction.Unreproducible) (bool, error) {
	if strings.TrimSpace(record.LoopID) == "" || r.repos.Loops == nil {
		return false, nil
	}
	loop, err := r.repos.Loops.GetByID(ctx, record.LoopID)
	if err != nil {
		return false, err
	}
	if loop == nil || loop.Status == string(domain.LoopStatusAwaitingHuman) {
		return false, nil
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || strings.TrimSpace(ask.Answer) == "" {
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(ask.Answer), AnswerProceed) {
		return false, nil
	}
	return true, reproduction.AppendWaiver(ctx, r.repos, reproduction.Waiver{
		ProjectID: record.ProjectID, Repo: record.Repo, IssueNumber: record.IssueNumber,
		Answer: ask.Answer, LoopID: record.LoopID, WaivedAt: r.nowISO(),
	})
}
