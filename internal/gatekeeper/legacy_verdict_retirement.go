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
	Version           int    `json:"version"`
	ProjectID         string `json:"projectId"`
	Repo              string `json:"repo"`
	PRNumber          int64  `json:"prNumber"`
	ReportEventID     string `json:"reportEventId"`
	ReportEvaluatedAt string `json:"reportEvaluatedAt"`
	ReportFingerprint string `json:"reportFingerprint,omitempty"`
	UpdatedComments   int    `json:"updatedComments"`
	RetiredAt         string `json:"retiredAt"`
}

type legacyAdviseReport struct {
	Report  Report
	EventID string
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
func (r *Runner) retireLegacyVerdictComments(ctx context.Context, candidate legacyAdviseReport, cwd string) error {
	report := candidate.Report
	completed, err := r.legacyVerdictRetired(ctx, candidate)
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
			ReportEventID:     candidate.EventID,
			ReportEvaluatedAt: report.EvaluatedAt, ReportFingerprint: report.SourceFingerprint,
			UpdatedComments: updatedComments, RetiredAt: r.now().UTC().Format(time.RFC3339Nano),
		}, CreatedAt: r.now(),
	}); err != nil {
		return fmt.Errorf("persist Gatekeeper verdict retirement: %w", err)
	}
	return nil
}

// ReconcileLegacyVerdictComments scans the complete immutable report history,
// including projects archived out of the active runtime catalog. Forge
// retirement failures are warnings, not Gatekeeper evaluation failures;
// absence of a completion event causes the next tick to retry the failed
// retirement.
func (r *Runner) ReconcileLegacyVerdictComments(ctx context.Context) error {
	reports, err := r.historicalLegacyAdviseReports(ctx)
	if err != nil {
		return err
	}
	projectCWD := map[string]string{}
	if r.repos != nil && r.repos.Projects != nil {
		projects, projectErr := r.repos.Projects.List(ctx)
		if projectErr != nil {
			return fmt.Errorf("list projects for Gatekeeper verdict retirement: %w", projectErr)
		}
		for _, project := range projects {
			projectCWD[project.ID] = project.RepoPath
		}
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].Report.ProjectID != reports[j].Report.ProjectID {
			return reports[i].Report.ProjectID < reports[j].Report.ProjectID
		}
		if reports[i].Report.PRNumber != reports[j].Report.PRNumber {
			return reports[i].Report.PRNumber < reports[j].Report.PRNumber
		}
		return reports[i].EventID < reports[j].EventID
	})
	for _, report := range reports {
		if err := r.retireLegacyVerdictComments(ctx, report, projectCWD[report.Report.ProjectID]); err != nil {
			if r.logWarn != nil {
				r.logWarn("gatekeeper: legacy verdict retirement deferred", map[string]any{
					"repo": report.Report.Repo, "pr": report.Report.PRNumber, "error": err.Error(),
				})
			}
		}
	}
	return nil
}

func (r *Runner) historicalLegacyAdviseReports(ctx context.Context) ([]legacyAdviseReport, error) {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return nil, fmt.Errorf("gatekeeper event repository is not configured")
	}
	events, err := r.repos.Events.ListByEntityTypeAndEventTypes(ctx, "pull_request", []string{GateReportEventType})
	if err != nil {
		return nil, fmt.Errorf("list Gatekeeper reports: %w", err)
	}
	reports := make([]legacyAdviseReport, 0)
	for _, event := range events {
		if event.EntityID == nil || event.ProjectID == nil {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
			continue
		}
		if report.Version >= 2 && strings.EqualFold(strings.TrimSpace(report.Mode), "advise") {
			reports = append(reports, legacyAdviseReport{Report: report, EventID: event.ID})
		}
	}
	return reports, nil
}

func (r *Runner) legacyVerdictRetired(ctx context.Context, candidate legacyAdviseReport) (bool, error) {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return false, fmt.Errorf("gatekeeper event repository is not configured")
	}
	report := candidate.Report
	events, err := r.repos.Events.ListByEntity(ctx, "pull_request", fmt.Sprintf("%s#%d", report.Repo, report.PRNumber))
	if err != nil {
		return false, fmt.Errorf("list Gatekeeper retirement events: %w", err)
	}
	for _, event := range events {
		if event.EventType != legacyVerdictRetirementEventType {
			continue
		}
		var retirement legacyVerdictRetirement
		if err := json.Unmarshal([]byte(event.PayloadJSON), &retirement); err != nil {
			continue
		}
		if retirement.ReportEventID != "" && retirement.ReportEventID == candidate.EventID {
			return true, nil
		}
		if retirement.ReportEventID == "" && retirement.ReportEvaluatedAt == report.EvaluatedAt && retirement.ReportFingerprint == report.SourceFingerprint {
			return true, nil
		}
	}
	return false, nil
}
