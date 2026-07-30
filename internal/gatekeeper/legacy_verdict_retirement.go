package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

// legacyVerdictCommentMarker identifies comments published by the withdrawn
// Gatekeeper advise lifecycle. Marker plus author remains the ownership proof;
// the marker alone is intentionally not enough because a human can quote it.
const legacyVerdictCommentMarker = "<!-- looper:gatekeeper:verdict v=1 -->"

const retiredLegacyVerdictBody = legacyVerdictCommentMarker + "\n" +
	"⚪ **Merge gate advice withdrawn.** This project now runs Gatekeeper in observe-only mode, so this verdict must not be used to decide whether to merge.\n"

// legacyVerdictCommentGateway is deliberately optional. The rollback never
// publishes advice, but the production GitHub gateway can safely withdraw its
// own old comments. Keeping this capability separate leaves ordinary Gatekeeper
// evaluation read-only for gateways that do not support comment mutation.
type legacyVerdictCommentGateway interface {
	ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error)
	UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error
	GetCurrentUserLoginForRepo(context.Context, string, string) (string, error)
}

// retireLegacyVerdictComments withdraws legacy advise verdicts whenever their
// v2 report remains in the event log. It intentionally rescans on later ticks:
// a transient forge failure must not let the next observe-only report hide the
// evidence needed to retry retirement.
func (r *Runner) retireLegacyVerdictComments(ctx context.Context, report Report, cwd string) error {
	legacy, err := r.hasLegacyAdviseReport(ctx, report.Repo, report.PRNumber)
	if err != nil || !legacy {
		return err
	}

	github, ok := r.github.(legacyVerdictCommentGateway)
	if !ok {
		return fmt.Errorf("gatekeeper GitHub gateway cannot retire legacy verdict comments")
	}
	login, err := github.GetCurrentUserLoginForRepo(ctx, report.Repo, cwd)
	if err != nil {
		return fmt.Errorf("get Gatekeeper comment owner: %w", err)
	}
	comments, err := github.ListIssueComments(ctx, githubinfra.ViewIssueInput{
		Repo: report.Repo, IssueNumber: report.PRNumber, CWD: cwd,
	})
	if err != nil {
		return fmt.Errorf("list Gatekeeper verdict comments: %w", err)
	}
	for _, comment := range comments {
		if !strings.Contains(comment.Body, legacyVerdictCommentMarker) || !strings.EqualFold(strings.TrimSpace(comment.Author), strings.TrimSpace(login)) {
			continue
		}
		if strings.TrimSpace(comment.Body) == strings.TrimSpace(retiredLegacyVerdictBody) {
			continue
		}
		if err := github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{
			Repo: report.Repo, CommentID: comment.ID, Body: retiredLegacyVerdictBody, CWD: cwd,
		}); err != nil {
			return fmt.Errorf("retire Gatekeeper verdict comment %d: %w", comment.ID, err)
		}
	}
	return nil
}

func (r *Runner) hasLegacyAdviseReport(ctx context.Context, repo string, prNumber int64) (bool, error) {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return false, fmt.Errorf("gatekeeper event repository is not configured")
	}
	events, err := r.repos.Events.ListByEntity(ctx, "pull_request", fmt.Sprintf("%s#%d", repo, prNumber))
	if err != nil {
		return false, fmt.Errorf("list Gatekeeper reports: %w", err)
	}
	for _, event := range events {
		if event.EventType != GateReportEventType {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
			return false, fmt.Errorf("decode Gatekeeper report: %w", err)
		}
		if report.Version >= 2 && strings.EqualFold(strings.TrimSpace(report.Mode), "advise") {
			return true, nil
		}
	}
	return false, nil
}
