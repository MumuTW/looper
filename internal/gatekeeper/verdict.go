package gatekeeper

import (
	"context"
	"fmt"
	"sort"
	"strings"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

// VerdictCommentMarker identifies Gatekeeper's verdict comment so it can be
// updated in place rather than reposted on every evaluation.
const VerdictCommentMarker = "<!-- looper:gatekeeper:verdict v=1 -->"

// reasonExplanations turn a reason code into something a human can act on. The
// codes are stable identifiers for machines; a verdict is read by a person
// deciding whether to click merge.
var reasonExplanations = map[ReasonCode]string{
	ReasonHeadStale:                "the head commit moved while this was being evaluated",
	ReasonPullRequestNotOpen:       "the pull request is not open",
	ReasonPullRequestDraft:         "the pull request is a draft",
	ReasonMergeConflict:            "the branch conflicts with its base",
	ReasonMergeabilityNotClean:     "GitHub does not report the branch as cleanly mergeable",
	ReasonCheckMissing:             "a required check has not reported",
	ReasonCheckPending:             "a required check is still running",
	ReasonCheckFailed:              "a required check failed",
	ReasonCheckCancelled:           "a required check was cancelled",
	ReasonReviewRequired:           "a required review is missing",
	ReasonReviewChangesRequested:   "a reviewer requested changes",
	ReasonUnresolvedReviewThread:   "a review thread is unresolved",
	ReasonProjectPolicyDenied:      "project policy does not permit merging this target",
	ReasonHold:                     "a hold label is applied",
	ReasonProviderStateUnavailable: "the forge did not return the state needed to judge this",
	ReasonProviderStateAmbiguous:   "the forge returned ambiguous state for this",
}

// BuildVerdictComment renders a report as the comment published at the advise
// trust level.
//
// It states the verdict and every blocking reason with its subject, because the
// entire point of advise is that the human does not have to redo the judgement
// to act on it. It deliberately does not tell anyone to merge: at this level the
// decision is still theirs.
func BuildVerdictComment(report Report) string {
	var b strings.Builder
	b.WriteString(VerdictCommentMarker)
	b.WriteString("\n")
	if report.Eligible {
		b.WriteString("✅ **Merge gate: eligible.** Every gate Looper checks passes at this head.\n\n")
	} else {
		b.WriteString("⛔ **Merge gate: blocked.**\n\n")
	}
	if len(report.Reasons) > 0 {
		b.WriteString("Blocked by:\n")
		for _, line := range formatReasons(report.Reasons) {
			b.WriteString("- " + line + "\n")
		}
		b.WriteString("\n")
	}
	if sha := strings.TrimSpace(report.ObservedHeadSHA); sha != "" {
		b.WriteString(fmt.Sprintf("Evaluated at `%s`.\n", shortSHA(sha)))
	}
	// The verdict is evidence, not a licence: it describes one moment, and holds,
	// reviews, threads, and policy can all change without moving the head.
	b.WriteString("\nThis verdict describes the head above. Anything that changes afterwards — a new commit, a review, a thread, a hold — invalidates it.\n")
	return b.String()
}

func formatReasons(reasons []Reason) []string {
	lines := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		explanation, ok := reasonExplanations[reason.Code]
		if !ok {
			explanation = string(reason.Code)
		}
		if subject := strings.TrimSpace(reason.Subject); subject != "" {
			lines = append(lines, fmt.Sprintf("%s — `%s`", explanation, subject))
			continue
		}
		lines = append(lines, explanation)
	}
	sort.Strings(lines)
	return lines
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// publishVerdict posts the verdict on the pull request, or updates the one
// already there. It is an upsert rather than a new comment per evaluation
// because this lane re-evaluates on every tick, and a fresh comment each time
// would bury the pull request it is meant to clarify.
func (r *Runner) publishVerdict(ctx context.Context, report Report) error {
	cwd := r.projectCWD(ctx, report.ProjectID)
	body := BuildVerdictComment(report)

	login, err := r.github.GetCurrentUserLoginForRepo(ctx, report.Repo, cwd)
	if err != nil {
		return err
	}
	comments, err := r.github.ListIssueComments(ctx, githubinfra.ViewIssueInput{
		Repo: report.Repo, IssueNumber: report.PRNumber, CWD: cwd,
	})
	if err != nil {
		return err
	}
	for _, comment := range comments {
		// Match on the marker AND the author: a human quoting the marker back must
		// not have their comment rewritten.
		if !strings.Contains(comment.Body, VerdictCommentMarker) || !strings.EqualFold(strings.TrimSpace(comment.Author), strings.TrimSpace(login)) {
			continue
		}
		if strings.TrimSpace(comment.Body) == strings.TrimSpace(body) {
			// Unchanged verdict: editing would bump the comment for no new information.
			return nil
		}
		return r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{
			Repo: report.Repo, CommentID: comment.ID, Body: body, CWD: cwd,
		})
	}
	_, err = r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{
		Repo: report.Repo, IssueNumber: report.PRNumber, Body: body, CWD: cwd,
	})
	return err
}
