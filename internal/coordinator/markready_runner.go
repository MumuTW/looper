package coordinator

import (
	"context"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/mergewatch"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

// markReadyOpenPullRequestLimit is deliberately larger than the default
// GitHub CLI page. `gh pr list --limit` follows its pagination until this
// bound, so a repository with more than one page of open PRs does not starve
// linked drafts at the prefix boundary.
const markReadyOpenPullRequestLimit = 1000

// markReadyCandidates lists the repository's open drafts once per tick.
//
// Without it the lane spends a Pull Request read on every reference an Issue
// has ever accumulated, on every tick, forever — a merged Pull Request from six
// months ago re-read 288 times a day to be told it is not a draft. An open
// draft is the only thing this lane can act on, so everything else is dropped
// by one listing rather than by N detail reads.
//
// A nil result means "consider nothing": the lane is disabled, or the listing
// failed and the lane has no idea what is open. Both leave every draft alone.
func (r *Runner) markReadyCandidates(ctx context.Context, repo, cwd string, roles config.RoleConfigs) map[int64]struct{} {
	cfg := roles.Coordinator.MarkReady
	if !cfg.Enabled || r.github == nil {
		return nil
	}
	// Fail closed on an unknown scope. Validation rejects any other value, so
	// reaching this branch means the config was assembled without it.
	if cfg.Scope != config.CoordinatorMarkReadyScopeLooperOnly {
		return nil
	}
	summaries, err := r.github.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: repo, CWD: cwd, Limit: markReadyOpenPullRequestLimit})
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("coordinator mark-ready skipped after forge error", map[string]any{"repo": repo, "action": "list open pull requests", "error": err.Error()})
		}
		return nil
	}
	candidates := map[int64]struct{}{}
	for _, summary := range summaries {
		if summary.Number > 0 && summary.IsDraft {
			candidates[summary.Number] = struct{}{}
		}
	}
	return candidates
}

// applyMarkReady publishes looper-authored drafts whose CI has gone green.
//
// It runs inside the merge-watch lane because that lane already holds the
// per-Issue watch lock and already knows how to read a Pull Request's required
// checks. It reads the linked Pull Requests directly rather than merge-watch's
// resolved watch target: auto-merge cannot be enabled on a draft, so a draft is
// by construction never the PR merge-watch is watching.
//
// Every failure here is deliberately non-fatal. Marking ready is a convenience
// on top of a pipeline that works without it, so a forge hiccup leaves the
// draft alone and the next tick tries again rather than aborting discovery and
// stalling triage and dispatch behind an optional lane.
func (r *Runner) applyMarkReady(ctx context.Context, repo, cwd string, issue loadedIssue, currentLogin string, candidates map[int64]struct{}, triagedLabel string) {
	if len(candidates) == 0 {
		return
	}
	// The issue list/detail read happens before the merge-watch lock and can go
	// stale while the coordinator is doing other work. Refresh the tracking
	// authority immediately before touching any linked PR.
	freshIssue, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issue.detail.Number, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issue.detail.Number, 0, "revalidate issue", err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(freshIssue.State), "open") || !issueHasCoordinatorTracking(freshIssue.Labels, triagedLabel) {
		return
	}
	issue.detail = freshIssue
	for _, prNumber := range linkedPullRequestNumbers(issue.rawTimeline) {
		if _, ok := candidates[prNumber]; !ok {
			continue
		}
		r.applyMarkReadyForPullRequest(ctx, repo, cwd, issue.detail.Number, prNumber, currentLogin, triagedLabel)
	}
}

func (r *Runner) applyMarkReadyForPullRequest(ctx context.Context, repo, cwd string, issueNumber, prNumber int64, currentLogin, triagedLabel string) {
	detail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "read pull request", err)
		return
	}
	snapshot := markReadySnapshot(repo, issueNumber, prNumber, detail, currentLogin)
	// First pass over the guards that need nothing but the Pull Request
	// itself. A draft that already fails one of those is left alone without
	// spending the timeline, check-run, branch-protection, and commit reads
	// below.
	if decision := mergewatch.DecideMarkReady(snapshot); !decision.MarkReady {
		r.logMarkReadyBlocked(repo, issueNumber, prNumber, decision.Blocker)
		return
	}
	draftEvents, err := r.github.ListPullRequestDraftEvents(ctx, githubinfra.PullRequestDraftEventsInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "read draft history", err)
		return
	}
	snapshot.HumanConvertedToDraft = mergewatch.HumanConvertedToDraft(markReadyDraftEvents(draftEvents), currentLogin)
	if decision := mergewatch.DecideMarkReady(snapshot); !decision.MarkReady {
		r.logMarkReadyBlocked(repo, issueNumber, prNumber, decision.Blocker)
		return
	}
	checkRuns, err := r.github.ListPullRequestCheckRuns(ctx, githubinfra.PullRequestCheckRunsInput{Repo: repo, Ref: detail.HeadSHA, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "read check runs", err)
		return
	}
	protection, err := r.github.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: repo, Branch: detail.BaseRefName, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "read branch protection", err)
		return
	}
	required, known := markReadyRequiredChecks(protection, checkRuns)
	snapshot.ChecksUnknown = !known
	snapshot.RequiredChecks = summarizeRequiredChecks(required, checkRuns, true)
	if decision := mergewatch.DecideMarkReady(snapshot); !decision.MarkReady {
		r.logMarkReadyBlocked(repo, issueNumber, prNumber, decision.Blocker)
		return
	}
	commits, err := r.github.ListPullRequestCommits(ctx, githubinfra.ListPullRequestCommitsInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "read branch commits", err)
		return
	}
	snapshot.ForeignCommitAuthors, snapshot.UnattributedCommits = foreignCommitAuthors(commits, currentLogin)
	if decision := mergewatch.DecideMarkReady(snapshot); !decision.MarkReady {
		r.logMarkReadyBlocked(repo, issueNumber, prNumber, decision.Blocker)
		return
	}
	// Everything above is a statement about the moment it was read. Between the
	// first read and this line a push can land, a hold label can appear, or a
	// human can publish the draft themselves — none of which move any signal the
	// evidence already collected would notice. `gh pr ready` takes no head
	// argument, so it would apply that stale evidence to whatever the Pull
	// Request has become. Making the evidence true again first is the only
	// conditional mutation available.
	confirmDetail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "revalidate pull request", err)
		return
	}
	confirming := markReadySnapshot(repo, issueNumber, prNumber, confirmDetail, currentLogin)
	if !strings.EqualFold(strings.TrimSpace(detail.HeadSHA), strings.TrimSpace(confirmDetail.HeadSHA)) {
		if decision := mergewatch.ConfirmMarkReady(snapshot, confirming); !decision.MarkReady {
			r.logMarkReadyBlocked(repo, issueNumber, prNumber, decision.Blocker)
		}
		return
	}
	// A same-head revalidation still needs fresh check, protection, commit, and
	// lifecycle evidence: every one of those can change without a new commit.
	confirmDraftEvents, err := r.github.ListPullRequestDraftEvents(ctx, githubinfra.PullRequestDraftEventsInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "revalidate draft history", err)
		return
	}
	confirming.HumanConvertedToDraft = mergewatch.HumanConvertedToDraft(markReadyDraftEvents(confirmDraftEvents), currentLogin)
	confirmCheckRuns, err := r.github.ListPullRequestCheckRuns(ctx, githubinfra.PullRequestCheckRunsInput{Repo: repo, Ref: confirmDetail.HeadSHA, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "revalidate check runs", err)
		return
	}
	confirmProtection, err := r.github.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: repo, Branch: confirmDetail.BaseRefName, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "revalidate branch protection", err)
		return
	}
	confirmRequired, confirmKnown := markReadyRequiredChecks(confirmProtection, confirmCheckRuns)
	confirming.ChecksUnknown = !confirmKnown
	confirming.RequiredChecks = summarizeRequiredChecks(confirmRequired, confirmCheckRuns, true)
	confirmCommits, err := r.github.ListPullRequestCommits(ctx, githubinfra.ListPullRequestCommitsInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "revalidate branch commits", err)
		return
	}
	confirming.ForeignCommitAuthors, confirming.UnattributedCommits = foreignCommitAuthors(confirmCommits, currentLogin)
	if decision := mergewatch.ConfirmMarkReady(snapshot, confirming); !decision.MarkReady {
		r.logMarkReadyBlocked(repo, issueNumber, prNumber, decision.Blocker)
		return
	}
	// The Issue is the tracking authority as well as the PR's scope input. A
	// closure or triage-label removal during the evidence reads must win over a
	// green PR, so re-read it adjacent to the mutation too.
	finalIssue, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "revalidate issue before mutation", err)
		return
	}
	if (finalIssue.Number != 0 && finalIssue.Number != issueNumber) || !strings.EqualFold(strings.TrimSpace(finalIssue.State), "open") || !issueHasCoordinatorTracking(finalIssue.Labels, triagedLabel) {
		return
	}
	if err := r.github.MarkPullRequestReady(ctx, githubinfra.MarkPullRequestReadyInput{Repo: repo, PRNumber: prNumber, CWD: cwd}); err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "mark ready for review", err)
		return
	}
	if r.logger != nil {
		r.logger.Info("coordinator marked looper-authored draft ready for review", map[string]any{"repo": repo, "issue": issueNumber, "pr": prNumber, "headSha": detail.HeadSHA})
	}
}

func markReadySnapshot(repo string, issueNumber, prNumber int64, detail githubinfra.PullRequestDetail, currentLogin string) mergewatch.MarkReadySnapshot {
	daemon := strings.ToLower(strings.TrimSpace(currentLogin))
	return mergewatch.MarkReadySnapshot{
		Repo:             repo,
		PRNumber:         prNumber,
		IssueNumber:      issueNumber,
		HeadSHA:          detail.HeadSHA,
		BaseRefName:      detail.BaseRefName,
		Draft:            detail.IsDraft,
		Open:             strings.EqualFold(strings.TrimSpace(detail.State), "open") && detail.MergedAt == "",
		InScope:          labels.AnyLooperOwned(detail.Labels) && prLinksIssue(repo, issueNumber, detail.Body),
		AuthoredByDaemon: daemon != "" && strings.ToLower(strings.TrimSpace(detail.Author)) == daemon,
		Held:             labels.Has(detail.Labels, labels.HoldGlobal) || labels.Has(detail.Labels, labels.DoNotMerge),
		Mergeable:        detail.Mergeable,
		MergeableState:   detail.MergeableState.Raw(),
	}
}

func markReadyDraftEvents(events []githubinfra.PullRequestDraftEvent) []mergewatch.MarkReadyDraftEvent {
	out := make([]mergewatch.MarkReadyDraftEvent, 0, len(events))
	for _, event := range events {
		out = append(out, mergewatch.MarkReadyDraftEvent{Event: event.Event, Actor: event.Actor, CreatedAt: event.CreatedAt})
	}
	return out
}

// markReadyRequiredChecks names the checks that must be green before a draft is
// published, and reports whether "green" is knowable at all.
//
// Branch protection is the authority whenever it names any check. When it names
// none there is no authority to defer to, so every check observed on the head
// counts instead: the alternative — treating "no required checks" as "nothing
// to wait for" — would publish drafts while CI is still running, which is
// exactly what this lane promises not to do.
//
// The second return value is what separates "nothing is required" from "nothing
// is known yet". An unprotected branch whose head has not registered a single
// check run produces an empty set either way, and a tick landing in the seconds
// before a workflow appears would read that emptiness as success. It is not
// evidence of anything, so it is reported as unknown and the draft waits.
func markReadyRequiredChecks(protection githubinfra.BranchProtection, checkRuns githubinfra.PullRequestCheckRuns) (requiredCheckRules, bool) {
	// A successful prefix of a truncated provider response is not evidence that
	// every check is green. Keep the draft in draft until both collections are
	// complete; this is especially important on unprotected branches where every
	// observed check becomes a requirement.
	if checkRuns.TotalCount > len(checkRuns.CheckRuns) || checkRuns.StatusesTotalCount > len(checkRuns.Statuses) {
		return requiredCheckRulesFor(protection), false
	}
	if required := requiredCheckRulesFor(protection); required.len() > 0 {
		return required, true
	}
	observed := requiredCheckRules{}
	for _, checkRun := range checkRuns.CheckRuns {
		observed.add(checkRun.Name, 0)
	}
	for _, status := range checkRuns.Statuses {
		observed.add(status.Context, 0)
	}
	return observed, observed.len() > 0
}

// foreignCommitAuthors splits a branch's commits into the forge-attributed
// committers that are not the daemon's and the count of commits GitHub could
// attribute to nobody. The author field is retained as compatibility evidence
// for older gateway implementations, but a current gateway marks the
// committer authority explicitly.
//
// Commit authorship as the forge reports it is the only authority available
// here that a pushing human cannot accidentally omit — a commit-message
// trailer is written by the agent and is therefore evidence about intent, not
// about who pushed. What it does not catch: a human whose commits are authored
// under the same account the daemon runs as is indistinguishable from the
// daemon, so a shared-account operator gets no protection from this check.
func foreignCommitAuthors(commits []githubinfra.PullRequestCommit, currentLogin string) ([]string, int) {
	daemon := strings.ToLower(strings.TrimSpace(currentLogin))
	foreign := []string(nil)
	seen := map[string]struct{}{}
	unattributed := 0
	for _, commit := range commits {
		identities := commit.Authors
		if commit.CommitterKnown {
			// The forge-attributed committer is the authority for who pushed the
			// branch. Author metadata is user-controlled and can survive a
			// cherry-pick by a different human.
			identities = commit.Committers
		}
		if len(identities) == 0 {
			unattributed++
			continue
		}
		for _, author := range identities {
			login := strings.ToLower(strings.TrimSpace(author))
			if login == "" {
				unattributed++
				continue
			}
			if daemon != "" && login == daemon {
				continue
			}
			if _, ok := seen[login]; ok {
				continue
			}
			seen[login] = struct{}{}
			foreign = append(foreign, author)
		}
	}
	return foreign, unattributed
}

func (r *Runner) logMarkReadyBlocked(repo string, issueNumber, prNumber int64, blocker mergewatch.MarkReadyBlocker) {
	if r.logger == nil || blocker == mergewatch.MarkReadyBlockerNotDraft {
		return
	}
	r.logger.Debug("coordinator left draft unpublished", map[string]any{"repo": repo, "issue": issueNumber, "pr": prNumber, "blocker": string(blocker)})
}

func (r *Runner) logMarkReadySkip(repo string, issueNumber, prNumber int64, action string, err error) {
	if r.logger == nil {
		return
	}
	r.logger.Warn("coordinator mark-ready skipped after forge error", map[string]any{"repo": repo, "issue": issueNumber, "pr": prNumber, "action": action, "error": err.Error()})
}
