package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

// legacyVerdictCommentMarker identifies comments published by the withdrawn
// Gatekeeper advise lifecycle. Marker plus author remains the ownership proof;
// the marker alone is intentionally not enough because a human can quote it.
const legacyVerdictCommentMarker = "<!-- looper:gatekeeper:verdict v=1 -->"

const retiredLegacyVerdictBody = legacyVerdictCommentMarker + "\n" +
	"⚪ **Merge gate advice withdrawn.** This project now runs Gatekeeper in observe-only mode, so this verdict must not be used to decide whether to merge.\n"

const legacyVerdictRetirementEventType = "pull_request.merge_gate.legacy_verdict_retired"

type legacyVerdictRetirement struct {
	Version         int    `json:"version"`
	ProjectID       string `json:"projectId"`
	Repo            string `json:"repo"`
	PRNumber        int64  `json:"prNumber"`
	UpdatedComments int    `json:"updatedComments"`
	RetiredAt       string `json:"retiredAt"`
}

// legacyVerdictCommentGateway is deliberately optional. The rollback never
// publishes advice, but the production GitHub gateway can safely withdraw its
// own old comments. Keeping this capability separate leaves ordinary Gatekeeper
// evaluation read-only for gateways that do not support comment mutation.
type legacyVerdictCommentGateway interface {
	ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error)
	UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error
	GetCurrentUserLoginForRepo(context.Context, string, string) (string, error)
}

// retireLegacyVerdictComments withdraws one historical advise verdict. A
// successful pass records a local completion event—even when the comment is
// already absent—so subsequent ticks do not repeat the forge reads. Failures
// leave that event absent and are therefore retried by the next pass.
func (r *Runner) retireLegacyVerdictComments(ctx context.Context, report Report, cwd string) error {
	completed, err := r.legacyVerdictRetired(ctx, report.Repo, report.PRNumber)
	if err != nil || completed {
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
	updatedComments := 0
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
		updatedComments++
	}
	projectID := report.ProjectID
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", report.Repo, report.PRNumber)
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: legacyVerdictRetirementEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID,
		Payload: legacyVerdictRetirement{
			Version: 1, ProjectID: report.ProjectID, Repo: report.Repo, PRNumber: report.PRNumber,
			UpdatedComments: updatedComments, RetiredAt: r.now().UTC().Format(time.RFC3339Nano),
		}, CreatedAt: r.now(),
	}); err != nil {
		return fmt.Errorf("persist Gatekeeper verdict retirement: %w", err)
	}
	return nil
}

// reconcileLegacyVerdictComments scans the complete immutable report history,
// not just the current open-PR page. Forge retirement failures are warnings,
// not Gatekeeper evaluation failures; absence of a completion event causes the
// next tick to retry the failed retirement.
func (r *Runner) reconcileLegacyVerdictComments(ctx context.Context, projectID, repo, cwd string) error {
	reports, err := r.historicalLegacyAdviseReports(ctx, projectID, repo)
	if err != nil {
		return err
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].PRNumber < reports[j].PRNumber })
	for _, report := range reports {
		if err := r.retireLegacyVerdictComments(ctx, report, cwd); err != nil {
			if r.logWarn != nil {
				r.logWarn("gatekeeper: legacy verdict retirement deferred", map[string]any{
					"repo": report.Repo, "pr": report.PRNumber, "error": err.Error(),
				})
			}
		}
	}
	return nil
}

func (r *Runner) historicalLegacyAdviseReports(ctx context.Context, projectID, repo string) ([]Report, error) {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return nil, fmt.Errorf("gatekeeper event repository is not configured")
	}
	events, err := r.repos.Events.ListByProjectAndEntityType(ctx, projectID, "pull_request")
	if err != nil {
		return nil, fmt.Errorf("list Gatekeeper reports: %w", err)
	}
	latestByEntity := map[string]Report{}
	for _, event := range events {
		if event.EventType != GateReportEventType || event.EntityID == nil {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
			continue
		}
		if report.Version >= 2 && strings.EqualFold(strings.TrimSpace(report.Mode), "advise") && strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(repo)) {
			latestByEntity[*event.EntityID] = report
		}
	}
	reports := make([]Report, 0, len(latestByEntity))
	for _, report := range latestByEntity {
		reports = append(reports, report)
	}
	return reports, nil
}

func (r *Runner) legacyVerdictRetired(ctx context.Context, repo string, prNumber int64) (bool, error) {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return false, fmt.Errorf("gatekeeper event repository is not configured")
	}
	events, err := r.repos.Events.ListByEntity(ctx, "pull_request", fmt.Sprintf("%s#%d", repo, prNumber))
	if err != nil {
		return false, fmt.Errorf("list Gatekeeper retirement events: %w", err)
	}
	for _, event := range events {
		if event.EventType == legacyVerdictRetirementEventType {
			return true, nil
		}
	}
	return false, nil
}
