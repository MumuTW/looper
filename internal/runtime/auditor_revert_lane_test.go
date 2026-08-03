package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/auditor"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

type fakeAuditorProposalGit struct {
	createCalls  []gitinfra.CreateWorktreeInput
	inspectInput gitinfra.InspectHeadInput
	inspect      gitinfra.InspectHeadResult
	reverts      []gitinfra.RevertCommitInput
	pushes       []gitinfra.PushInput
}

func (g *fakeAuditorProposalGit) CreateWorktree(_ context.Context, input gitinfra.CreateWorktreeInput) (storage.WorktreeRecord, error) {
	g.createCalls = append(g.createCalls, input)
	return storage.WorktreeRecord{ID: "worktree_1", ProjectID: input.ProjectID, RepoPath: input.RepoPath, WorktreePath: filepath.Join(input.WorktreeRoot, "revert"), Branch: input.Branch, BaseBranch: &input.BaseBranch}, nil
}

func (g *fakeAuditorProposalGit) InspectHead(_ context.Context, input gitinfra.InspectHeadInput) (gitinfra.InspectHeadResult, error) {
	g.inspectInput = input
	return g.inspect, nil
}

func (g *fakeAuditorProposalGit) RevertCommit(_ context.Context, input gitinfra.RevertCommitInput) (gitinfra.CommitResult, error) {
	g.reverts = append(g.reverts, input)
	return gitinfra.CommitResult{CommitSHA: "revert-commit"}, nil
}

func (g *fakeAuditorProposalGit) Push(_ context.Context, input gitinfra.PushInput) error {
	g.pushes = append(g.pushes, input)
	return nil
}

type fakeAuditorProposalGitHub struct {
	head        string
	openPRs     []githubinfra.PullRequestSummary
	createPRs   []githubinfra.CreatePullRequestInput
	labelCalls  []githubinfra.PullRequestLabelsInput
	reopenCalls []githubinfra.ReopenIssueInput
}

func (g *fakeAuditorProposalGitHub) GetBranchHeadSHA(context.Context, githubinfra.BranchHeadInput) (string, error) {
	return g.head, nil
}

func (g *fakeAuditorProposalGitHub) ListOpenPullRequests(context.Context, githubinfra.ListOpenPullRequestsInput) ([]githubinfra.PullRequestSummary, error) {
	return g.openPRs, nil
}

func (g *fakeAuditorProposalGitHub) CreatePullRequest(_ context.Context, input githubinfra.CreatePullRequestInput) (githubinfra.CreatePullRequestResult, error) {
	g.createPRs = append(g.createPRs, input)
	return githubinfra.CreatePullRequestResult{Number: 99, URL: "https://example.test/acme/looper/pull/99"}, nil
}

func (g *fakeAuditorProposalGitHub) AddPullRequestLabels(_ context.Context, input githubinfra.PullRequestLabelsInput) error {
	g.labelCalls = append(g.labelCalls, input)
	return nil
}

func (g *fakeAuditorProposalGitHub) ReopenIssue(_ context.Context, input githubinfra.ReopenIssueInput) error {
	g.reopenCalls = append(g.reopenCalls, input)
	return nil
}

func TestProgressAuditorRevertProposalCreatesOneDraftAndReopensSourceIssue(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorRevertFixture(t)
	git := &fakeAuditorProposalGit{inspect: gitinfra.InspectHeadResult{HeadSHA: "base"}}
	github := &fakeAuditorProposalGitHub{head: head}

	if err := progressAuditorRevertProposal(ctx, repos, git, github, project, repo, "main", func() time.Time { return now }); err != nil {
		t.Fatalf("progressAuditorRevertProposal() error = %v", err)
	}
	if len(git.reverts) != 1 || git.reverts[0].CommitSHA != "abcdef1234567890" || len(git.pushes) != 1 {
		t.Fatalf("git operations = reverts:%#v pushes:%#v", git.reverts, git.pushes)
	}
	if git.inspectInput.BaseRef != "origin/main" {
		t.Fatalf("InspectHead BaseRef = %q, want fetched remote base", git.inspectInput.BaseRef)
	}
	if len(github.createPRs) != 1 || !github.createPRs[0].Draft || github.createPRs[0].BaseBranch != "main" || github.createPRs[0].HeadBranch != "looper/auditor/revert-pr-42-abcdef123456" {
		t.Fatalf("create PR = %#v, want one draft proposal branch", github.createPRs)
	}
	if len(github.labelCalls) != 1 || len(github.labelCalls[0].Labels) != 1 || github.labelCalls[0].Labels[0] != labels.HoldAuditorRevert || len(github.reopenCalls) != 1 || github.reopenCalls[0].IssueNumber != 118 || github.reopenCalls[0].Repo != repo {
		t.Fatalf("reopen calls = %#v", github.reopenCalls)
	}
	entityType, entityID := "branch_head", repo+"@"+head
	proposal := onlyAuditorRevertProposal(t, mustListAuditorEvents(t, ctx, repos, entityType, entityID))
	if proposal.PRNumber != 99 || proposal.Branch != "looper/auditor/revert-pr-42-abcdef123456" || proposal.SourceIssueNumber != 118 {
		t.Fatalf("proposal = %#v", proposal)
	}

	if err := progressAuditorRevertProposal(ctx, repos, git, github, project, repo, "main", func() time.Time { return now }); err != nil {
		t.Fatalf("replayed progressAuditorRevertProposal() error = %v", err)
	}
	if len(git.reverts) != 1 || len(github.createPRs) != 1 || len(github.reopenCalls) != 1 {
		t.Fatalf("replay duplicated proposal operations: git:%#v prs:%#v reopens:%#v", git.reverts, github.createPRs, github.reopenCalls)
	}
}

func TestProgressAuditorRevertProposalAdoptsExistingDraftAfterInterruptedCreate(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorRevertFixture(t)
	branch := "looper/auditor/revert-pr-42-abcdef123456"
	git := &fakeAuditorProposalGit{inspect: gitinfra.InspectHeadResult{HeadSHA: "base"}}
	github := &fakeAuditorProposalGitHub{head: head, openPRs: []githubinfra.PullRequestSummary{{Number: 99, URL: "https://example.test/acme/looper/pull/99", HeadRefName: branch}}}

	if err := progressAuditorRevertProposal(ctx, repos, git, github, project, repo, "main", func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if len(git.createCalls) != 0 || len(git.reverts) != 0 || len(git.pushes) != 0 || len(github.createPRs) != 0 || len(github.reopenCalls) != 1 {
		t.Fatalf("existing proposal was not adopted safely: git=%#v pr=%#v reopen=%#v", git, github.createPRs, github.reopenCalls)
	}
}

func TestProgressAuditorRevertProposalUsesFrozenCandidateAfterHistoryChanges(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorRevertFixture(t)
	projectID := project.ID
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{EventType: gatekeeper.MergeOutcomeEventType, ProjectID: &projectID, Payload: gatekeeper.MergeOutcome{Version: 1, ProjectID: projectID, Repo: repo, PRNumber: 43, HeadSHA: "later-head", MergeCommitSHA: "later-commit", MergeStrategy: "squash", Merged: true, TouchedFilesAvailable: true}, CreatedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	git := &fakeAuditorProposalGit{inspect: gitinfra.InspectHeadResult{HeadSHA: "base"}}
	github := &fakeAuditorProposalGitHub{head: head}
	if err := progressAuditorRevertProposal(ctx, repos, git, github, project, repo, "main", func() time.Time { return now.Add(2 * time.Minute) }); err != nil {
		t.Fatal(err)
	}
	if len(github.createPRs) != 1 || github.createPRs[0].HeadBranch != "looper/auditor/revert-pr-42-abcdef123456" || len(git.reverts) != 1 || git.reverts[0].CommitSHA != "abcdef1234567890" {
		t.Fatalf("proposal diverged from frozen candidate: git=%#v prs=%#v", git, github.createPRs)
	}
}

func TestProgressAuditorRevertProposalRejectsUnverifiedPushedBranch(t *testing.T) {
	ctx, repos, project, repo, head, now := auditorRevertFixture(t)
	git := &fakeAuditorProposalGit{inspect: gitinfra.InspectHeadResult{HeadSHA: "existing-revert", NewCommitSHAs: []string{"existing-revert"}}}
	github := &fakeAuditorProposalGitHub{head: head}

	err := progressAuditorRevertProposal(ctx, repos, git, github, project, repo, "main", func() time.Time { return now })
	if err == nil || len(git.reverts) != 0 || len(git.pushes) != 0 || len(github.createPRs) != 0 || len(github.reopenCalls) != 0 {
		t.Fatalf("unverified branch replay = err:%v git:%#v create=%#v reopen=%#v", err, git, github.createPRs, github.reopenCalls)
	}
}

func TestProgressAuditorRevertProposalRejectsIncompleteCandidateBeforeMutating(t *testing.T) {
	candidate := auditor.ConfirmedCandidate{PRNumber: 42, MergeStrategy: "squash", SourceIssueNumber: 118, SourceIssueRepo: "acme/looper"}
	ctx, repos, project, repo, head, now := auditorRevertFixtureWithCandidate(t, candidate)
	git := &fakeAuditorProposalGit{}
	github := &fakeAuditorProposalGitHub{head: head}
	err := progressAuditorRevertProposal(ctx, repos, git, github, project, repo, "main", func() time.Time { return now })
	if err == nil || len(git.createCalls) != 0 || len(github.createPRs) != 0 {
		t.Fatalf("incomplete provenance err=%v git=%#v prs=%#v", err, git.createCalls, github.createPRs)
	}
}

func auditorRevertFixture(t *testing.T) (context.Context, *storage.Repositories, storage.ProjectRecord, string, string, time.Time) {
	return auditorRevertFixtureWithCandidate(t, auditor.ConfirmedCandidate{PRNumber: 42, MergeCommitSHA: "abcdef1234567890", MergeStrategy: "squash", SourceIssueNumber: 118, SourceIssueRepo: "acme/looper"})
}

func auditorRevertFixtureWithCandidate(t *testing.T, candidate auditor.ConfirmedCandidate) (context.Context, *storage.Repositories, storage.ProjectRecord, string, string, time.Time) {
	t.Helper()
	ctx := context.Background()
	workdir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workdir, "auditor.sqlite"), t.TempDir())
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	projectID, repo, head, base := "project_1", "acme/looper", "base-sha", "main"
	nowISO := eventlog.FormatJavaScriptISOString(now)
	worktreeRoot := filepath.Join(workdir, "worktrees")
	metadata := `{"repo":"acme/looper","worktreeRoot":"` + worktreeRoot + `"}`
	project := storage.ProjectRecord{ID: projectID, Name: "Demo", RepoPath: workdir, BaseBranch: &base, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Projects.Upsert(ctx, project); err != nil {
		t.Fatal(err)
	}
	entityType, entityID, confirmationID := "branch_head", repo+"@"+head, "confirmation_1"
	candidate.SourceIssueRepo = repo
	confirmation := auditor.ConfirmationRecord{Version: 2, ObservationEventID: "observation_1", HeadSHA: head, Outcome: auditor.ConfirmationConfirmed, ConfirmedChecks: []string{"ci"}, Decision: auditor.ActionProposeRevert, Reason: "unique_candidate_has_observed_failure_evidence", Candidate: &candidate, ConfirmedAt: nowISO}
	if err := eventlog.Append(ctx, repos, eventlog.AppendInput{ID: confirmationID, EventType: auditor.ConfirmationEventType, ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, Payload: confirmation, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return ctx, repos, project, repo, head, now
}

func onlyAuditorRevertProposal(t *testing.T, events []storage.EventLogRecord) auditor.RevertProposal {
	t.Helper()
	var found []auditor.RevertProposal
	for _, event := range events {
		if event.EventType != auditor.RevertProposalEventType {
			continue
		}
		var proposal auditor.RevertProposal
		if err := json.Unmarshal([]byte(event.PayloadJSON), &proposal); err != nil {
			t.Fatal(err)
		}
		found = append(found, proposal)
	}
	if len(found) != 1 {
		t.Fatalf("revert proposals = %#v, want one", found)
	}
	return found[0]
}
