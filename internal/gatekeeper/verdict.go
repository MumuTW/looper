package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// VerdictCommentMarker identifies the one comment Gatekeeper owns on a pull
// request. Marker plus author is the comment's identity: the marker alone would
// also match a human quoting it back.
const VerdictCommentMarker = "<!-- looper:gatekeeper:verdict v=1 -->"

// retiredVerdictBody is what an owned comment says once the project no longer
// holds the authority that produced it. It is retired rather than deleted so the
// withdrawal is visible, and so a later promotion updates the same comment
// instead of creating a second one.
const retiredVerdictBody = VerdictCommentMarker + "\n" +
	"⚪ **Merge gate advice withdrawn.** This project is back at `observe`, so Looper no longer publishes a verdict here.\n"

// verdictAction is what the owned comment needs. It is decided before any forge
// read.
type verdictAction string

const (
	// verdictActionNone is the steady state: nothing needs to change, so
	// Gatekeeper performs no forge reads or writes at all.
	verdictActionNone verdictAction = "none"
	// verdictActionPublish creates or updates the owned comment.
	verdictActionPublish verdictAction = "publish"
	// verdictActionRetire withdraws it after a demotion.
	verdictActionRetire verdictAction = "retire"
)

// decideVerdictAction compares the report about to be written against the last
// one, without touching the forge.
//
// This is what keeps a quiet pull request quiet. Scanning the discussion every
// tick to discover that nothing changed costs a comment page per pull request per
// tick, and everything needed to rule that out is already in the local event log.
func decideVerdictAction(trust config.GatekeeperTrustLevel, previous *Report, next Report) verdictAction {
	if trust == config.GatekeeperTrustObserve {
		// Only a project that previously published has anything to withdraw.
		if previous != nil && previousPublished(*previous) {
			return verdictActionRetire
		}
		return verdictActionNone
	}
	if previous == nil || !previousPublished(*previous) {
		return verdictActionPublish
	}
	if BuildVerdictComment(*previous) != BuildVerdictComment(next) {
		return verdictActionPublish
	}
	return verdictActionNone
}

// previousPublished reports whether a stored report was written at a level that
// publishes. Reports predating the trust ladder carry the historical mode and
// never published.
func previousPublished(report Report) bool {
	switch config.GatekeeperTrustLevel(strings.TrimSpace(report.Mode)) {
	case config.GatekeeperTrustAdvise, config.GatekeeperTrustAuto:
		return true
	default:
		return false
	}
}

// reasonExplanations turn a reason code into something a human can act on. The
// codes are stable identifiers for machines; a verdict is read by a person
// deciding whether to merge.
var reasonExplanations = map[ReasonCode]string{
	ReasonHeadStale:                 "the head commit moved while this was being evaluated",
	ReasonPullRequestNotOpen:        "the pull request is not open",
	ReasonPullRequestDraft:          "the pull request is a draft",
	ReasonMergeConflict:             "the branch conflicts with its base",
	ReasonMergeabilityNotClean:      "GitHub does not report the branch as cleanly mergeable",
	ReasonCheckMissing:              "a required check has not reported",
	ReasonCheckPending:              "a required check is still running",
	ReasonCheckFailed:               "a required check failed",
	ReasonCheckCancelled:            "a required check was cancelled",
	ReasonReviewRequired:            "a required review is missing",
	ReasonReviewChangesRequested:    "a reviewer requested changes",
	ReasonCodexReviewMissing:        "a completed Codex review for the current head is required",
	ReasonCodexReviewInProgress:     "a Codex review for the current head is still in progress",
	ReasonCodexBlockingFindings:     "the current-head Codex review reported blocking findings",
	ReasonCodexReviewOutcomeUnknown: "the current-head Codex review has no recognized outcome",
	ReasonUnresolvedReviewThread:    "a review thread is unresolved",
	ReasonProjectPolicyDenied:       "project policy does not permit merging this target",
	ReasonHold:                      "a hold label is applied",
	ReasonProviderStateUnavailable:  "the forge did not return the state needed to judge this",
	ReasonProviderStateAmbiguous:    "the forge returned ambiguous state for this",
}

// BuildVerdictComment renders a report as the comment published at advise.
//
// It states the verdict and every blocking reason with its subject, because the
// point of advise is that a human does not have to redo the judgement to act on
// it. It deliberately does not tell anyone to merge: at this level the decision
// is still theirs.
//
// The rendering is deterministic, which is what lets an unchanged verdict be
// recognised by comparing rendered bodies instead of re-reading the forge.
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

// latestReport returns the most recent gate report stored for a pull request.
func (r *Runner) latestReport(ctx context.Context, repo string, prNumber int64) (*Report, error) {
	if r.repos == nil || r.repos.Events == nil {
		return nil, nil
	}
	events, err := r.repos.Events.ListByEntity(ctx, "pull_request", fmt.Sprintf("%s#%d", repo, prNumber))
	if err != nil {
		return nil, err
	}
	var latest *Report
	latestAt := ""
	for _, event := range events {
		if event.EventType != GateReportEventType {
			continue
		}
		if latest != nil && event.CreatedAt < latestAt {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
			// A report this build cannot decode must not read as "nothing was
			// published": that would silently strand an owned comment.
			return nil, fmt.Errorf("decode gate report for %s#%d: %w", repo, prNumber, err)
		}
		latest = &report
		latestAt = event.CreatedAt
	}
	return latest, nil
}

// applyVerdict brings the owned comment in line with the report.
func (r *Runner) applyVerdict(ctx context.Context, action verdictAction, report Report) error {
	if action == verdictActionNone {
		return nil
	}
	cwd := r.projectCWD(ctx, report.ProjectID)
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
	owned := ownedVerdictComments(comments, login)

	body := BuildVerdictComment(report)
	if action == verdictActionRetire {
		if len(owned) == 0 {
			return nil
		}
		body = retiredVerdictBody
	}

	if len(owned) == 0 {
		_, err := r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{
			Repo: report.Repo, IssueNumber: report.PRNumber, Body: body, CWD: cwd,
		})
		return err
	}

	// Two evaluators racing can each find no marker and each create one. The
	// oldest wins, because every evaluator computes the same winner from the same
	// list, so a duplicate is reconciled instead of contradicting the survivor
	// forever.
	survivor, duplicates := owned[0], owned[1:]
	if strings.TrimSpace(survivor.Body) != strings.TrimSpace(body) {
		if err := r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{
			Repo: report.Repo, CommentID: survivor.ID, Body: body, CWD: cwd,
		}); err != nil {
			return err
		}
	}
	for _, duplicate := range duplicates {
		if err := r.github.DeleteIssueComment(ctx, githubinfra.DeleteIssueCommentInput{
			Repo: report.Repo, CommentID: duplicate.ID, CWD: cwd,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ownedVerdictComments returns Looper's own verdict comments, oldest first, so
// every evaluator picks the same survivor when reconciling duplicates.
func ownedVerdictComments(comments []githubinfra.CommentInfo, login string) []githubinfra.CommentInfo {
	owned := make([]githubinfra.CommentInfo, 0, 1)
	for _, comment := range comments {
		if !strings.Contains(comment.Body, VerdictCommentMarker) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(comment.Author), strings.TrimSpace(login)) {
			continue
		}
		owned = append(owned, comment)
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].ID < owned[j].ID })
	return owned
}
