package coordinator

import (
	"context"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/coordinator/mergewatch"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

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
func (r *Runner) applyMarkReady(ctx context.Context, repo, cwd string, issue loadedIssue, roles config.RoleConfigs, currentLogin string) {
	cfg := roles.Coordinator.MarkReady
	if !cfg.Enabled || r.github == nil {
		return
	}
	// Fail closed on an unknown scope. Validation rejects any other value, so
	// reaching this branch means the config was assembled without it.
	if cfg.Scope != config.CoordinatorMarkReadyScopeLooperOnly {
		return
	}
	for _, prNumber := range linkedPullRequestNumbers(issue.rawTimeline) {
		r.applyMarkReadyForPullRequest(ctx, repo, cwd, issue.detail.Number, prNumber, currentLogin)
	}
}

func (r *Runner) applyMarkReadyForPullRequest(ctx context.Context, repo, cwd string, issueNumber, prNumber int64, currentLogin string) {
	detail, err := r.github.ViewPullRequestMergeWatch(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "read pull request", err)
		return
	}
	snapshot := markReadySnapshot(repo, issueNumber, prNumber, detail)
	// First pass over the guards that need nothing but the Pull Request
	// itself. A draft that already fails one of those is left alone without
	// spending the check-run, branch-protection, and commit reads below.
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
	snapshot.RequiredChecks = summarizeRequiredChecks(markReadyRequiredCheckSet(protection, checkRuns), checkRuns, true)
	commits, err := r.github.ListPullRequestCommits(ctx, githubinfra.ListPullRequestCommitsInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
	if err != nil {
		r.logMarkReadySkip(repo, issueNumber, prNumber, "read branch commits", err)
		return
	}
	snapshot.ForeignCommitAuthors, snapshot.UnattributedCommits = foreignCommitAuthors(commits, currentLogin)
	decision := mergewatch.DecideMarkReady(snapshot)
	if !decision.MarkReady {
		r.logMarkReadyBlocked(repo, issueNumber, prNumber, decision.Blocker)
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

func markReadySnapshot(repo string, issueNumber, prNumber int64, detail githubinfra.PullRequestDetail) mergewatch.MarkReadySnapshot {
	return mergewatch.MarkReadySnapshot{
		Repo:           repo,
		PRNumber:       prNumber,
		IssueNumber:    issueNumber,
		HeadSHA:        detail.HeadSHA,
		Draft:          detail.IsDraft,
		Open:           strings.EqualFold(strings.TrimSpace(detail.State), "open") && detail.MergedAt == "",
		InScope:        labels.AnyLooperOwned(detail.Labels) && prLinksIssue(repo, issueNumber, detail.Body),
		Held:           labels.Has(detail.Labels, labels.HoldGlobal) || labels.Has(detail.Labels, labels.DoNotMerge),
		Mergeable:      detail.Mergeable,
		MergeableState: detail.MergeableState.Raw(),
	}
}

// markReadyRequiredCheckSet names the checks that must be green before a draft
// is published. Branch protection is the authority whenever it names any
// check. When it names none there is no authority to defer to, so every check
// observed on the head counts instead: the alternative — treating "no required
// checks" as "nothing to wait for" — would publish drafts while CI is still
// running, which is exactly what this lane promises not to do.
func markReadyRequiredCheckSet(protection githubinfra.BranchProtection, checkRuns githubinfra.PullRequestCheckRuns) map[string]struct{} {
	if required := requiredCheckSet(protection.RequiredChecks); len(required) > 0 {
		return required
	}
	observed := map[string]struct{}{}
	for _, checkRun := range checkRuns.CheckRuns {
		if key := strings.ToLower(strings.TrimSpace(checkRun.Name)); key != "" {
			observed[key] = struct{}{}
		}
	}
	for _, status := range checkRuns.Statuses {
		if key := strings.ToLower(strings.TrimSpace(status.Context)); key != "" {
			observed[key] = struct{}{}
		}
	}
	return observed
}

// foreignCommitAuthors splits a branch's commits into the logins that are not
// the daemon's and the count of commits GitHub could attribute to nobody.
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
		if len(commit.Authors) == 0 {
			unattributed++
			continue
		}
		for _, author := range commit.Authors {
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
