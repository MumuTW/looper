package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/auditor"
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/eventlog"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

type auditorRevertGit interface {
	CreateWorktree(context.Context, gitinfra.CreateWorktreeInput) (storage.WorktreeRecord, error)
	InspectHead(context.Context, gitinfra.InspectHeadInput) (gitinfra.InspectHeadResult, error)
	RevertCommit(context.Context, gitinfra.RevertCommitInput) (gitinfra.CommitResult, error)
	Push(context.Context, gitinfra.PushInput) error
}

type auditorRevertGitHub interface {
	GetBranchHeadSHA(context.Context, githubinfra.BranchHeadInput) (string, error)
	ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error)
	CreatePullRequest(context.Context, githubinfra.CreatePullRequestInput) (githubinfra.CreatePullRequestResult, error)
	AddPullRequestLabels(context.Context, githubinfra.PullRequestLabelsInput) error
	ReopenIssue(context.Context, githubinfra.ReopenIssueInput) error
}

// progressAuditorRevertProposal creates at most one draft proposal for the
// immutable candidate in a confirmation event. It never touches the default
// branch and uses a deterministic branch so an interrupted push/create/reopen
// sequence resumes the same proposal instead of creating a second one.
func progressAuditorRevertProposal(ctx context.Context, repos *storage.Repositories, git auditorRevertGit, github auditorRevertGitHub, project storage.ProjectRecord, repo, baseBranch string, now func() time.Time) error {
	if repos == nil || repos.Events == nil || git == nil || github == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return fmt.Errorf("auditor project %s has no base branch", project.ID)
	}
	headSHA, err := github.GetBranchHeadSHA(ctx, githubinfra.BranchHeadInput{Repo: repo, Branch: baseBranch, CWD: project.RepoPath})
	if err != nil {
		return fmt.Errorf("auditor read default branch head for revert proposal: %w", err)
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return fmt.Errorf("auditor read default branch head for revert proposal: empty SHA")
	}
	entityType, entityID := "branch_head", repo+"@"+headSHA
	events, err := repos.Events.ListByEntity(ctx, entityType, entityID)
	if err != nil {
		return err
	}
	confirmationEvent, confirmation, ok, err := latestRevertConfirmation(events, headSHA)
	if err != nil || !ok {
		return err
	}
	if hasAuditorRevertProposal(events, confirmationEvent.ID) {
		return nil
	}
	branch := auditorRevertBranch(*confirmation.Candidate)
	proposal, found, err := openAuditorRevertProposal(ctx, github, repo, project.RepoPath, branch)
	if err != nil {
		return err
	}
	if !found {
		worktreeRoot, err := worktreeCleanupRoot(project)
		if err != nil {
			return fmt.Errorf("auditor resolve worktree root: %w", err)
		}
		worktree, err := git.CreateWorktree(ctx, gitinfra.CreateWorktreeInput{ProjectID: project.ID, RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, Branch: branch, BaseBranch: baseBranch, ProtectedBranches: []string{baseBranch}})
		if err != nil {
			return fmt.Errorf("auditor create proposal worktree: %w", err)
		}
		inspect, err := git.InspectHead(ctx, gitinfra.InspectHeadInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.WorktreePath, BaseRef: "origin/" + baseBranch})
		if err != nil {
			return fmt.Errorf("auditor inspect proposal branch: %w", err)
		}
		if inspect.HasUncommittedChanges {
			return fmt.Errorf("auditor proposal worktree is unexpectedly dirty")
		}
		var commitSHA string
		switch len(inspect.NewCommitSHAs) {
		case 0:
			result, err := git.RevertCommit(ctx, gitinfra.RevertCommitInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.WorktreePath, CommitSHA: confirmation.Candidate.MergeCommitSHA})
			if err != nil {
				return fmt.Errorf("auditor revert merged commit: %w", err)
			}
			commitSHA = result.CommitSHA
		case 1:
			// A commit-count signal is infrastructure evidence, not authority that
			// this branch still reverses the frozen merge. Without a tree-level
			// comparison, fail closed rather than pushing arbitrary branch state.
			return fmt.Errorf("auditor proposal branch contains an unverified existing revert commit")
		default:
			return fmt.Errorf("auditor proposal branch has %d commits beyond %s", len(inspect.NewCommitSHAs), baseBranch)
		}
		if err := git.Push(ctx, gitinfra.PushInput{RepoPath: project.RepoPath, WorktreeRoot: worktreeRoot, WorktreePath: worktree.WorktreePath, Branch: branch, LocalHeadSHA: commitSHA, ProtectedBranches: []string{baseBranch}}); err != nil {
			return fmt.Errorf("auditor push proposal branch: %w", err)
		}
		created, err := github.CreatePullRequest(ctx, githubinfra.CreatePullRequestInput{Repo: repo, HeadBranch: branch, BaseBranch: baseBranch, Title: auditorRevertTitle(*confirmation.Candidate), Body: auditorRevertBody(confirmationEvent.ID, confirmation), Draft: true, CWD: project.RepoPath})
		if err != nil {
			return fmt.Errorf("auditor create draft revert pull request: %w", err)
		}
		proposal = created
	}
	if proposal.Number <= 0 {
		return fmt.Errorf("auditor revert proposal has no pull request number for hold label")
	}
	if err := github.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: repo, PRNumber: proposal.Number, Labels: []string{labels.HoldAuditorRevert}, CWD: project.RepoPath}); err != nil {
		return fmt.Errorf("auditor hold revert proposal from auto-merge: %w", err)
	}
	if err := github.ReopenIssue(ctx, githubinfra.ReopenIssueInput{Repo: confirmation.Candidate.SourceIssueRepo, IssueNumber: confirmation.Candidate.SourceIssueNumber, CWD: project.RepoPath}); err != nil {
		return fmt.Errorf("auditor reopen source issue: %w", err)
	}
	return appendAuditorRevertProposal(ctx, repos, project.ID, entityType, entityID, confirmationEvent.ID, repo, headSHA, branch, proposal, confirmation.Candidate, now())
}

func latestRevertConfirmation(events []storage.EventLogRecord, headSHA string) (storage.EventLogRecord, auditor.ConfirmationRecord, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.EventType != auditor.ConfirmationEventType {
			continue
		}
		var confirmation auditor.ConfirmationRecord
		if err := json.Unmarshal([]byte(event.PayloadJSON), &confirmation); err != nil {
			return storage.EventLogRecord{}, auditor.ConfirmationRecord{}, false, fmt.Errorf("decode auditor confirmation event %s: %w", event.ID, err)
		}
		if confirmation.Decision != auditor.ActionProposeRevert || confirmation.Candidate == nil || !strings.EqualFold(strings.TrimSpace(confirmation.HeadSHA), strings.TrimSpace(headSHA)) {
			continue
		}
		candidate := confirmation.Candidate
		mergeStrategy := strings.ToLower(strings.TrimSpace(candidate.MergeStrategy))
		if candidate.PRNumber <= 0 || strings.TrimSpace(candidate.MergeCommitSHA) == "" || (mergeStrategy != string(config.ReviewerAutoMergeStrategySquash) && mergeStrategy != string(config.ReviewerAutoMergeStrategyMerge)) || candidate.SourceIssueNumber <= 0 || strings.TrimSpace(candidate.SourceIssueRepo) == "" {
			return storage.EventLogRecord{}, auditor.ConfirmationRecord{}, false, fmt.Errorf("auditor confirmation event %s lacks proposal provenance", event.ID)
		}
		return event, confirmation, true, nil
	}
	return storage.EventLogRecord{}, auditor.ConfirmationRecord{}, false, nil
}

func hasAuditorRevertProposal(events []storage.EventLogRecord, confirmationEventID string) bool {
	for _, event := range events {
		if event.EventType == auditor.RevertProposalEventType && event.CausationID != nil && *event.CausationID == confirmationEventID {
			return true
		}
	}
	return false
}

func openAuditorRevertProposal(ctx context.Context, github auditorRevertGitHub, repo, cwd, branch string) (githubinfra.CreatePullRequestResult, bool, error) {
	pullRequests, err := github.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: 1000})
	if err != nil {
		return githubinfra.CreatePullRequestResult{}, false, fmt.Errorf("auditor list open revert pull requests: %w", err)
	}
	for _, pullRequest := range pullRequests {
		if strings.EqualFold(strings.TrimSpace(pullRequest.HeadRefName), strings.TrimSpace(branch)) {
			return githubinfra.CreatePullRequestResult{Number: pullRequest.Number, URL: pullRequest.URL}, true, nil
		}
	}
	return githubinfra.CreatePullRequestResult{}, false, nil
}

func auditorRevertBranch(candidate auditor.ConfirmedCandidate) string {
	mergeCommit := strings.ToLower(strings.TrimSpace(candidate.MergeCommitSHA))
	if len(mergeCommit) > 12 {
		mergeCommit = mergeCommit[:12]
	}
	return fmt.Sprintf("looper/auditor/revert-pr-%d-%s", candidate.PRNumber, mergeCommit)
}

func auditorRevertTitle(candidate auditor.ConfirmedCandidate) string {
	return fmt.Sprintf("revert: post-merge regression from #%d", candidate.PRNumber)
}

func auditorRevertBody(confirmationEventID string, confirmation auditor.ConfirmationRecord) string {
	return fmt.Sprintf("Automated post-merge audit proposal; this PR is intentionally draft and must not be auto-merged.\n\n- Confirmed failure: %s\n- Reverts merge commit: `%s`\n- Source issue reopened: #%d\n- Audit confirmation event: `%s`\n- Attribution: %s\n", strings.Join(confirmation.ConfirmedChecks, ", "), confirmation.Candidate.MergeCommitSHA, confirmation.Candidate.SourceIssueNumber, confirmationEventID, confirmation.Reason)
}

func appendAuditorRevertProposal(ctx context.Context, repos *storage.Repositories, projectID, entityType, entityID, confirmationEventID, repo, headSHA, branch string, proposal githubinfra.CreatePullRequestResult, candidate *auditor.ConfirmedCandidate, proposedAt time.Time) error {
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType: auditor.RevertProposalEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, CausationID: &confirmationEventID,
		Payload: auditor.RevertProposal{Version: 1, ConfirmationEventID: confirmationEventID, Repo: repo, HeadSHA: headSHA, MergeCommitSHA: candidate.MergeCommitSHA, MergeStrategy: candidate.MergeStrategy, SourceIssueNumber: candidate.SourceIssueNumber, Branch: branch, PRNumber: proposal.Number, PRURL: proposal.URL, ProposedAt: eventlog.FormatJavaScriptISOString(proposedAt)}, CreatedAt: proposedAt,
	})
}
