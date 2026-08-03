package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/storage"
)

// legacyVerdictCommentMarker identifies comments published by the withdrawn
// Gatekeeper advise lifecycle. Marker plus author remains the ownership proof;
// the marker alone is intentionally not enough because a human can quote it.
const legacyVerdictCommentMarker = "<!-- looper:gatekeeper:verdict v=1 -->"

const retiredLegacyVerdictBody = legacyVerdictCommentMarker + "\n" +
	"⚪ **Merge gate advice withdrawn.** This project now runs Gatekeeper in observe-only mode, so this verdict must not be used to decide whether to merge.\n"

const legacyVerdictRetirementEventType = "pull_request.merge_gate.legacy_verdict_retired"
const legacyVerdictRetirementScanEventType = "pull_request.merge_gate.legacy_verdict_retirement_scan_completed"

var errLegacyVerdictForeignOwner = errors.New("legacy Gatekeeper verdict belongs to another GitHub account")

type legacyVerdictRetirement struct {
	Version           int    `json:"version"`
	ProjectID         string `json:"projectId"`
	Repo              string `json:"repo"`
	PRNumber          int64  `json:"prNumber"`
	ReportEventID     string `json:"reportEventId"`
	ReportEvaluatedAt string `json:"reportEvaluatedAt"`
	ReportFingerprint string `json:"reportFingerprint,omitempty"`
	UpdatedComments   int    `json:"updatedComments"`
	OwnerLogin        string `json:"ownerLogin,omitempty"`
	// AbsenceChecks records consecutive passes that confirmed there is no
	// owned active comment. The first absence is deliberately provisional: an
	// older daemon may have persisted its report and still be about to publish
	// the comment.
	AbsenceChecks int    `json:"absenceChecks,omitempty"`
	RetiredAt     string `json:"retiredAt"`
}

type legacyAdviseReport struct {
	Report           Report
	EventID          string
	CreatedAt        string
	PersistProjectID bool
}

type legacyVerdictRetirementScan struct {
	Version           int    `json:"version"`
	ProjectID         string `json:"projectId"`
	LastReportEventID string `json:"lastReportEventId"`
	CompletedAt       string `json:"completedAt"`
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

type legacyVerdictFilteredCommentGateway interface {
	ListLegacyVerdictComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error)
}

func listLegacyVerdictComments(ctx context.Context, gateway legacyVerdictCommentGateway, input githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error) {
	if filtered, ok := gateway.(legacyVerdictFilteredCommentGateway); ok {
		return filtered.ListLegacyVerdictComments(ctx, input)
	}
	return gateway.ListIssueComments(ctx, input)
}

// retireLegacyVerdictComments withdraws one historical advise verdict. A
// successful update is final immediately. An absent owned comment requires
// two consecutive observations before completion is recorded, closing the
// report-then-comment publication race during a rolling upgrade. Failures
// leave completion absent and are therefore retried by the next pass.
func (r *Runner) retireLegacyVerdictComments(ctx context.Context, candidate legacyAdviseReport, cwd string) (bool, error) {
	report := candidate.Report
	previous, err := r.latestLegacyVerdictRetirement(ctx, candidate)
	if err != nil {
		return false, err
	}

	github, ok := r.github.(legacyVerdictCommentGateway)
	if !ok {
		return false, fmt.Errorf("gatekeeper GitHub gateway cannot retire legacy verdict comments")
	}
	login := ""
	if previous != nil && (previous.UpdatedComments > 0 || previous.AbsenceChecks >= 2) {
		login, err = github.GetCurrentUserLoginForRepo(ctx, report.Repo, cwd)
		if err != nil {
			return false, fmt.Errorf("get Gatekeeper comment owner: %w", err)
		}
		if owner := strings.TrimSpace(previous.OwnerLogin); owner != "" && strings.EqualFold(owner, strings.TrimSpace(login)) {
			return true, nil
		}
	}
	if login == "" {
		login, err = github.GetCurrentUserLoginForRepo(ctx, report.Repo, cwd)
		if err != nil {
			return false, fmt.Errorf("get Gatekeeper comment owner: %w", err)
		}
	}
	comments, err := listLegacyVerdictComments(ctx, github, githubinfra.ViewIssueInput{
		Repo: report.Repo, IssueNumber: report.PRNumber, CWD: cwd,
	})
	if err != nil {
		return false, fmt.Errorf("list Gatekeeper verdict comments: %w", err)
	}
	updatedComments := 0
	foreignOwnerMarker := false
	for _, comment := range comments {
		if !strings.Contains(comment.Body, legacyVerdictCommentMarker) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(comment.Author), strings.TrimSpace(login)) {
			foreignOwnerMarker = true
			continue
		}
		if strings.TrimSpace(comment.Body) == strings.TrimSpace(retiredLegacyVerdictBody) {
			continue
		}
		if err := github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{
			Repo: report.Repo, CommentID: comment.ID, Body: retiredLegacyVerdictBody, CWD: cwd,
		}); err != nil {
			return false, fmt.Errorf("retire Gatekeeper verdict comment %d: %w", comment.ID, err)
		}
		updatedComments++
	}
	if foreignOwnerMarker {
		return false, errLegacyVerdictForeignOwner
	}
	var projectID *string
	if candidate.PersistProjectID {
		projectIDValue := report.ProjectID
		projectID = &projectIDValue
	}
	entityType := "pull_request"
	entityID := fmt.Sprintf("%s#%d", report.Repo, report.PRNumber)
	absenceChecks := 0
	if updatedComments == 0 {
		if previous != nil {
			absenceChecks = previous.AbsenceChecks
		}
		absenceChecks++
	}
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: legacyVerdictRetirementEventType, ProjectID: projectID,
		EntityType: &entityType, EntityID: &entityID,
		Payload: legacyVerdictRetirement{
			Version: 1, ProjectID: report.ProjectID, Repo: report.Repo, PRNumber: report.PRNumber,
			ReportEventID:     candidate.EventID,
			ReportEvaluatedAt: report.EvaluatedAt, ReportFingerprint: report.SourceFingerprint,
			UpdatedComments: updatedComments, OwnerLogin: strings.TrimSpace(login), AbsenceChecks: absenceChecks,
			RetiredAt: r.now().UTC().Format(time.RFC3339Nano),
		}, CreatedAt: r.now(),
	}); err != nil {
		return false, fmt.Errorf("persist Gatekeeper verdict retirement: %w", err)
	}
	return updatedComments > 0 || absenceChecks >= 2, nil
}

// ReconcileLegacyVerdictComments scans the complete immutable report history,
// including projects archived out of the active runtime catalog. Forge
// retirement failures are warnings, not Gatekeeper evaluation failures;
// absence of a completion event causes the next tick to retry the failed
// retirement.
func (r *Runner) ReconcileLegacyVerdictComments(ctx context.Context) error {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return fmt.Errorf("gatekeeper event repository is not configured")
	}
	projectIDs, err := r.repos.Events.ListProjectIDsByEventType(ctx, GateReportEventType)
	if err != nil {
		return err
	}
	linkedProjectIDs := make(map[string]bool, len(projectIDs))
	for _, projectID := range projectIDs {
		linkedProjectIDs[projectID] = true
	}
	latestAdvise, err := r.repos.Events.LatestByLogicalProjectAndEventTypeAndPayloadMode(ctx, GateReportEventType, "advise")
	if err != nil {
		return err
	}
	for projectID := range latestAdvise {
		if !linkedProjectIDs[projectID] {
			projectIDs = append(projectIDs, projectID)
		}
	}
	seenProjectIDs := make(map[string]struct{}, len(projectIDs))
	projectCWD := map[string]string{}
	if r.repos != nil && r.repos.Projects != nil {
		projects, projectErr := r.repos.Projects.List(ctx)
		if projectErr != nil {
			return fmt.Errorf("list projects for Gatekeeper verdict retirement: %w", projectErr)
		}
		for _, project := range projects {
			projectCWD[project.ID] = usableRetirementCWD(project.RepoPath)
		}
	}
	for _, projectID := range projectIDs {
		if _, seen := seenProjectIDs[projectID]; seen {
			continue
		}
		seenProjectIDs[projectID] = struct{}{}
		latestReport, ok := latestAdvise[projectID]
		if !ok {
			continue
		}
		scanComplete, err := r.retirementScanComplete(ctx, projectID, latestReport.ID)
		if err != nil {
			return err
		}
		if scanComplete {
			continue
		}
		reports, err := r.historicalLegacyAdviseReports(ctx, projectID, linkedProjectIDs[projectID])
		if err != nil {
			return err
		}
		sort.Slice(reports, func(i, j int) bool {
			if reports[i].Report.PRNumber != reports[j].Report.PRNumber {
				return reports[i].Report.PRNumber < reports[j].Report.PRNumber
			}
			return reports[i].EventID < reports[j].EventID
		})
		retirementFailed := false
		for _, report := range reports {
			completed, err := r.retireLegacyVerdictComments(ctx, report, projectCWD[projectID])
			if err != nil || !completed {
				retirementFailed = true
				if r.logWarn != nil {
					message := "gatekeeper: legacy verdict retirement deferred"
					if err == nil {
						message = "gatekeeper: legacy verdict absence requires a second observation"
					}
					fields := map[string]any{
						"projectId": projectID, "repo": report.Report.Repo, "pr": report.Report.PRNumber,
					}
					if err != nil {
						fields["error"] = err.Error()
					}
					r.logWarn(message, fields)
				}
			}
		}
		if retirementFailed {
			continue
		}
		if err := r.persistRetirementScan(ctx, projectID, latestReport.ID, linkedProjectIDs[projectID]); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) historicalLegacyAdviseReports(ctx context.Context, projectID string, persistProjectID bool) ([]legacyAdviseReport, error) {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return nil, fmt.Errorf("gatekeeper event repository is not configured")
	}
	events, err := r.repos.Events.ListByProjectOrPayloadProjectAndEntityTypeAppendOrder(ctx, projectID, "pull_request")
	if err != nil {
		return nil, fmt.Errorf("list Gatekeeper reports: %w", err)
	}
	reports, _ := legacyAdviseReportsFromEvents(events, persistProjectID)
	return reports[projectID], nil
}

func legacyAdviseReportsFromEvents(events []storage.EventLogRecord, persistProjectID bool) (map[string][]legacyAdviseReport, map[string]storage.EventLogRecord) {
	byProject := make(map[string]map[string]legacyAdviseReport)
	latest := make(map[string]storage.EventLogRecord)
	for _, event := range events {
		if event.EntityID == nil {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
			continue
		}
		projectID := strings.TrimSpace(report.ProjectID)
		if projectID == "" || report.Version < 2 || !strings.EqualFold(strings.TrimSpace(report.Mode), "advise") {
			continue
		}
		latest[projectID] = event
		entityID := fmt.Sprintf("%s#%d", strings.ToLower(strings.TrimSpace(report.Repo)), report.PRNumber)
		if _, ok := byProject[projectID]; !ok {
			byProject[projectID] = make(map[string]legacyAdviseReport)
		}
		candidate := legacyAdviseReport{Report: report, EventID: event.ID, CreatedAt: event.CreatedAt, PersistProjectID: persistProjectID}
		// The repository query is ordered by SQLite rowid, the append-order
		// authority. Overwrite rather than comparing random event IDs when
		// timestamps share the same millisecond.
		byProject[projectID][entityID] = candidate
	}
	reports := make(map[string][]legacyAdviseReport, len(byProject))
	for projectID, candidates := range byProject {
		reports[projectID] = make([]legacyAdviseReport, 0, len(candidates))
		for _, candidate := range candidates {
			reports[projectID] = append(reports[projectID], candidate)
		}
	}
	return reports, latest
}

func (r *Runner) retirementScanComplete(ctx context.Context, projectID, latestReportEventID string) (bool, error) {
	record, err := r.repos.Events.LatestByEntityAndEventType(ctx, "project", projectID, legacyVerdictRetirementScanEventType)
	if err != nil || record == nil {
		return false, err
	}
	var scan legacyVerdictRetirementScan
	if err := json.Unmarshal([]byte(record.PayloadJSON), &scan); err != nil {
		return false, nil
	}
	return scan.LastReportEventID == latestReportEventID, nil
}

func (r *Runner) persistRetirementScan(ctx context.Context, projectID, latestReportEventID string, persistProjectID bool) error {
	entityType := "project"
	entityID := projectID
	var projectRef *string
	if persistProjectID {
		projectRef = &projectID
	}
	if err := eventlog.Append(ctx, r.repos, eventlog.AppendInput{
		EventType: legacyVerdictRetirementScanEventType, ProjectID: projectRef,
		EntityType: &entityType, EntityID: &entityID,
		Payload: legacyVerdictRetirementScan{
			Version: 1, ProjectID: projectID, LastReportEventID: latestReportEventID,
			CompletedAt: r.now().UTC().Format(time.RFC3339Nano),
		}, CreatedAt: r.now(),
	}); err != nil {
		return fmt.Errorf("persist Gatekeeper retirement scan: %w", err)
	}
	return nil
}

func (r *Runner) latestLegacyVerdictRetirement(ctx context.Context, candidate legacyAdviseReport) (*legacyVerdictRetirement, error) {
	if r == nil || r.repos == nil || r.repos.Events == nil {
		return nil, fmt.Errorf("gatekeeper event repository is not configured")
	}
	report := candidate.Report
	record, err := r.repos.Events.LatestByEntityAndEventType(ctx, "pull_request", fmt.Sprintf("%s#%d", report.Repo, report.PRNumber), legacyVerdictRetirementEventType)
	if err != nil {
		return nil, fmt.Errorf("list Gatekeeper retirement events: %w", err)
	}
	if record == nil {
		return nil, nil
	}
	var retirement legacyVerdictRetirement
	if err := json.Unmarshal([]byte(record.PayloadJSON), &retirement); err != nil {
		return nil, nil
	}
	if retirement.ReportEventID != "" && retirement.ReportEventID == candidate.EventID {
		return &retirement, nil
	}
	if retirement.ReportEventID == "" && retirement.ReportEvaluatedAt == report.EvaluatedAt && retirement.ReportFingerprint == report.SourceFingerprint {
		return &retirement, nil
	}
	return nil, nil
}

// usableRetirementCWD prevents an archived project's deleted checkout from
// turning an API-only migration into a permanent command-start failure. An
// empty value intentionally lets the configured GitHub gateway fallback CWD
// apply; a path can still disappear after this check and remains retryable.
func usableRetirementCWD(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ""
	}
	return path
}
