// Package triager turns fresh GitHub issue events into persisted, policy-
// validated routing reports.
package triager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	ReportEventType       = "triage.report"
	ConfirmationEventType = "triage.confirmed"

	ActionRoutePlanner Action = "route_planner"
	ActionAwaitHuman   Action = "await_human_confirmation"

	ClassificationBug      Classification = "bug"
	ClassificationFeature  Classification = "feature"
	ClassificationDocs     Classification = "docs"
	ClassificationRefactor Classification = "refactor"
	ClassificationChore    Classification = "chore"

	ScopeInScope    Scope = "in_scope"
	ScopeOutOfScope Scope = "out_of_scope"

	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"

	NextRolePlanner NextRole = "planner"
	NextRoleHuman   NextRole = "human"

	defaultIssueLimit                     = 100
	defaultSourceLookback                 = 5 * time.Minute
	autoRouteConfidence                   = 0.8
	reportEntityType                      = "github_issue"
	sourceEventNew        SourceEventKind = "new"
	sourceEventReopened   SourceEventKind = "reopened"
)

type Classification string
type Scope string
type Risk string
type NextRole string
type Action string
type SourceEventKind string

type Decision struct {
	Classification      Classification `json:"classification"`
	Scope               Scope          `json:"scope"`
	Risk                Risk           `json:"risk"`
	Confidence          float64        `json:"confidence"`
	MissingInformation  []string       `json:"missingInformation"`
	RecommendedNextRole NextRole       `json:"recommendedNextRole"`
	Rationale           string         `json:"rationale"`
}

type PolicyDecision struct {
	Action  Action   `json:"action"`
	Reasons []string `json:"reasons,omitempty"`
}

type SourceEvent struct {
	Kind       SourceEventKind `json:"kind"`
	OccurredAt string          `json:"occurredAt"`
	EventID    string          `json:"eventId,omitempty"`
}

// Report is the durable semantic authority for a triage route. GitHub labels
// are intentionally absent: they may be projected later, but do not authorize
// Planner.
type Report struct {
	Version        int            `json:"version"`
	IdempotencyKey string         `json:"idempotencyKey"`
	ProjectID      string         `json:"projectId"`
	Repo           string         `json:"repo"`
	IssueNumber    int64          `json:"issueNumber"`
	Source         SourceEvent    `json:"source"`
	Decision       Decision       `json:"decision"`
	Policy         PolicyDecision `json:"policy"`
	CreatedAt      string         `json:"createdAt"`
}

type Confirmation struct {
	ReportKey   string `json:"reportKey"`
	CommentID   int64  `json:"commentId"`
	Author      string `json:"author"`
	ConfirmedAt string `json:"confirmedAt"`
}

type Request struct {
	Prompt           string
	WorkingDirectory string
}

type LLM interface {
	Complete(context.Context, Request) (string, error)
}

type GitHubGateway interface {
	ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error)
	ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error)
	ListIssueTimeline(context.Context, githubinfra.IssueTimelineInput) ([]map[string]any, error)
	GetRepositoryPermission(context.Context, githubinfra.RepositoryPermissionInput) (string, error)
}

type PlannerRouter interface {
	RouteIssue(context.Context, planner.RouteIssueInput) (planner.DiscoveryResult, error)
}

type Options struct {
	Repos          *storage.Repositories
	GitHub         GitHubGateway
	LLM            LLM
	Planner        PlannerRouter
	Now            func() time.Time
	SourceLookback time.Duration
}

type Runner struct {
	repos          *storage.Repositories
	github         GitHubGateway
	llm            LLM
	planner        PlannerRouter
	now            func() time.Time
	sourceLookback time.Duration
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Snapshot  *githubinfra.DiscoverySnapshot
}

type DiscoveryResult struct {
	ReportsPersisted     int
	Routed               int
	AwaitingConfirmation int
	Confirmed            int
	Skipped              int
	QueueItems           []storage.QueueItemRecord
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	sourceLookback := options.SourceLookback
	if sourceLookback <= 0 {
		sourceLookback = defaultSourceLookback
	}
	return &Runner{
		repos: options.Repos, github: options.GitHub, llm: options.LLM,
		planner: options.Planner, now: now, sourceLookback: sourceLookback,
	}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	ctx = githubinfra.ContextWithDiscoverySnapshot(ctx, input.Snapshot)
	if r.repos == nil || r.repos.Projects == nil || r.repos.Events == nil {
		return DiscoveryResult{}, fmt.Errorf("triager repositories are not configured")
	}
	if r.github == nil || r.llm == nil || r.planner == nil {
		return DiscoveryResult{}, fmt.Errorf("triager dependencies are not configured")
	}
	project, err := r.repos.Projects.GetByID(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project == nil {
		return DiscoveryResult{}, fmt.Errorf("project not found: %s", input.ProjectID)
	}
	if project.Archived {
		return DiscoveryResult{Skipped: 1}, nil
	}
	cutoff := r.now().UTC().Add(-r.sourceLookback)
	issues, err := r.github.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{
		Repo: input.Repo, CWD: project.RepoPath, Limit: defaultIssueLimit,
		Search: fmt.Sprintf("updated:>=%s sort:updated-desc", cutoff.Format(time.RFC3339)),
	})
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	for _, summary := range issues {
		detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: input.Repo, IssueNumber: summary.Number, CWD: project.RepoPath})
		if err != nil {
			return result, err
		}
		if !eligibleTarget(detail) || domain.IsAutoLaneHeld(domain.LoopTypePlanner, detail.Labels) {
			result.Skipped++
			continue
		}
		timeline, err := r.github.ListIssueTimeline(ctx, githubinfra.IssueTimelineInput{Repo: input.Repo, IssueNumber: detail.Number, CWD: project.RepoPath})
		if err != nil {
			return result, err
		}
		source, ok := sourceEvent(detail, timeline)
		if !ok {
			result.Skipped++
			continue
		}
		key := buildIdempotencyKey(input.ProjectID, input.Repo, detail.Number, source)
		report, found, err := r.findReport(ctx, input.ProjectID, input.Repo, detail.Number, key)
		if err != nil {
			return result, err
		}
		if !found {
			if !r.sourceIsRecent(source, cutoff) {
				result.Skipped++
				continue
			}
			decision, err := r.decide(ctx, *project, input.Repo, detail)
			if err != nil {
				return result, err
			}
			report = Report{
				Version: 1, IdempotencyKey: key, ProjectID: input.ProjectID, Repo: input.Repo,
				IssueNumber: detail.Number, Source: source, Decision: decision,
				Policy: validateDecision(decision), CreatedAt: r.now().UTC().Format(time.RFC3339Nano),
			}
			if err := r.persistReport(ctx, report); err != nil {
				return result, err
			}
			result.ReportsPersisted++
		}
		action := report.Policy.Action
		if action == ActionAwaitHuman {
			confirmed, created, err := r.confirmedByHuman(ctx, input.Repo, project.RepoPath, detail, report)
			if err != nil {
				return result, err
			}
			if !confirmed {
				result.AwaitingConfirmation++
				continue
			}
			if created {
				result.Confirmed++
			}
			action = ActionRoutePlanner
		}
		switch action {
		case ActionRoutePlanner:
			route, err := r.planner.RouteIssue(ctx, planner.RouteIssueInput{
				ProjectID: input.ProjectID, Repo: input.Repo, Authority: report.IdempotencyKey,
				Issue: planner.IssueSummary{
					Number: detail.Number, Title: detail.Title, Body: detail.Body, URL: detail.URL,
					Assignees: append([]string(nil), detail.Assignees...), Labels: append([]string(nil), detail.Labels...),
				},
			})
			if err != nil {
				return result, err
			}
			result.Routed++
			result.QueueItems = append(result.QueueItems, route.QueueItems...)
		default:
			return result, fmt.Errorf("triage report %s has unknown policy action %q", report.IdempotencyKey, action)
		}
	}
	return result, nil
}

func (r *Runner) confirmedByHuman(ctx context.Context, repo, cwd string, issue githubinfra.IssueDetail, report Report) (confirmed, created bool, err error) {
	events, err := r.repos.Events.ListByEntity(ctx, reportEntityType, reportEntityID(report.ProjectID, report.Repo, report.IssueNumber))
	if err != nil {
		return false, false, err
	}
	for _, event := range events {
		if event.EventType != ConfirmationEventType {
			continue
		}
		var confirmation Confirmation
		if err := json.Unmarshal([]byte(event.PayloadJSON), &confirmation); err != nil {
			return false, false, fmt.Errorf("decode triage confirmation: %w", err)
		}
		if confirmation.ReportKey == report.IdempotencyKey {
			return true, false, nil
		}
	}
	reportedAt, err := time.Parse(time.RFC3339Nano, report.CreatedAt)
	if err != nil {
		return false, false, fmt.Errorf("parse triage report timestamp: %w", err)
	}
	for _, comment := range issue.Comments {
		if strings.TrimSpace(comment.Body) != "/plan" || strings.TrimSpace(comment.Author) == "" {
			continue
		}
		commentedAt, err := time.Parse(time.RFC3339Nano, comment.CreatedAt)
		if err != nil || !commentedAt.After(reportedAt) {
			continue
		}
		permission, err := r.github.GetRepositoryPermission(ctx, githubinfra.RepositoryPermissionInput{
			Repo: repo, User: comment.Author, CWD: cwd,
		})
		if err != nil {
			return false, false, err
		}
		if !githubinfra.RepositoryPermissionAllowsWrite(permission) {
			continue
		}
		confirmation := Confirmation{
			ReportKey: report.IdempotencyKey, CommentID: comment.ID,
			Author: comment.Author, ConfirmedAt: comment.CreatedAt,
		}
		if err := r.persistConfirmation(ctx, report, confirmation); err != nil {
			return false, false, err
		}
		return true, true, nil
	}
	return false, false, nil
}

func (r *Runner) sourceIsRecent(source SourceEvent, cutoff time.Time) bool {
	occurredAt, err := time.Parse(time.RFC3339Nano, source.OccurredAt)
	return err == nil && !occurredAt.Before(cutoff)
}

func eligibleTarget(issue githubinfra.IssueDetail) bool {
	return issue.Number > 0 &&
		strings.EqualFold(strings.TrimSpace(issue.State), "open") &&
		!issue.IsPullRequest
}

func sourceEvent(issue githubinfra.IssueDetail, timeline []map[string]any) (SourceEvent, bool) {
	source := SourceEvent{Kind: sourceEventNew, OccurredAt: strings.TrimSpace(issue.CreatedAt)}
	latest, err := time.Parse(time.RFC3339Nano, source.OccurredAt)
	if err != nil {
		return SourceEvent{}, false
	}
	for _, event := range timeline {
		if !strings.EqualFold(strings.TrimSpace(stringValue(event["event"])), string(sourceEventReopened)) {
			continue
		}
		occurredAt := firstNonEmpty(stringValue(event["created_at"]), stringValue(event["createdAt"]))
		when, err := time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil || !when.After(latest) {
			continue
		}
		latest = when
		source = SourceEvent{Kind: sourceEventReopened, OccurredAt: occurredAt, EventID: eventID(event["id"])}
	}
	return source, true
}

func (r *Runner) decide(ctx context.Context, project storage.ProjectRecord, repo string, issue githubinfra.IssueDetail) (Decision, error) {
	raw, err := r.llm.Complete(ctx, Request{
		WorkingDirectory: project.RepoPath,
		Prompt:           buildPrompt(repo, issue),
	})
	if err != nil {
		return Decision{}, err
	}
	var decision Decision
	decoder := json.NewDecoder(bytes.NewBufferString(strings.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return Decision{}, fmt.Errorf("decode triage decision: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Decision{}, fmt.Errorf("decode triage decision: trailing content")
	}
	decision.Rationale = strings.TrimSpace(decision.Rationale)
	decision.MissingInformation = compactStrings(decision.MissingInformation)
	return decision, nil
}

func validateDecision(decision Decision) PolicyDecision {
	reasons := make([]string, 0, 7)
	if !validClassification(decision.Classification) {
		reasons = append(reasons, "unsupported_classification")
	}
	if decision.Scope != ScopeInScope {
		reasons = append(reasons, "scope_not_in")
	}
	if decision.Risk != RiskLow {
		reasons = append(reasons, "risk_not_low")
	}
	if decision.Confidence < autoRouteConfidence || decision.Confidence > 1 {
		reasons = append(reasons, "confidence_below_threshold")
	}
	if len(decision.MissingInformation) > 0 {
		reasons = append(reasons, "missing_information")
	}
	if decision.RecommendedNextRole != NextRolePlanner {
		reasons = append(reasons, "next_role_not_planner")
	}
	if decision.Rationale == "" {
		reasons = append(reasons, "missing_rationale")
	}
	if len(reasons) > 0 {
		return PolicyDecision{Action: ActionAwaitHuman, Reasons: reasons}
	}
	return PolicyDecision{Action: ActionRoutePlanner}
}

func validClassification(classification Classification) bool {
	switch classification {
	case ClassificationBug, ClassificationFeature, ClassificationDocs, ClassificationRefactor, ClassificationChore:
		return true
	default:
		return false
	}
}

func buildPrompt(repo string, issue githubinfra.IssueDetail) string {
	return fmt.Sprintf(`You are Looper's internal Triager. Return one strict JSON object and no prose.
Schema:
{"classification":"bug|feature|docs|refactor|chore","scope":"in_scope|out_of_scope","risk":"low|medium|high","confidence":0.0,"missingInformation":["string"],"recommendedNextRole":"planner|human","rationale":"string"}

Only recommend Planner when the report is sufficiently specific for planning. Treat security-sensitive, destructive, credential, billing, permission, migration, and production-data work as medium or high risk.

Repository: %s
Issue: #%d %s

%s`, repo, issue.Number, strings.TrimSpace(issue.Title), strings.TrimSpace(issue.Body))
}

func (r *Runner) persistReport(ctx context.Context, report Report) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	projectID := report.ProjectID
	entityType := reportEntityType
	entityID := reportEntityID(report.ProjectID, report.Repo, report.IssueNumber)
	actorType, actorID := "system", "triager"
	return r.repos.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID("triage"), EventType: ReportEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(payload), CreatedAt: report.CreatedAt,
	})
}

func (r *Runner) persistConfirmation(ctx context.Context, report Report, confirmation Confirmation) error {
	payload, err := json.Marshal(confirmation)
	if err != nil {
		return err
	}
	projectID := report.ProjectID
	entityType := reportEntityType
	entityID := reportEntityID(report.ProjectID, report.Repo, report.IssueNumber)
	actorType, actorID := "human", confirmation.Author
	return r.repos.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID("triage-confirm"), EventType: ConfirmationEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(payload), CreatedAt: confirmation.ConfirmedAt,
	})
}

func (r *Runner) findReport(ctx context.Context, projectID, repo string, issueNumber int64, key string) (Report, bool, error) {
	events, err := r.repos.Events.ListByEntity(ctx, reportEntityType, reportEntityID(projectID, repo, issueNumber))
	if err != nil {
		return Report{}, false, err
	}
	for _, event := range events {
		if event.EventType != ReportEventType {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
			return Report{}, false, fmt.Errorf("decode persisted triage report: %w", err)
		}
		if report.IdempotencyKey == key {
			return report, true, nil
		}
	}
	return Report{}, false, nil
}

func buildIdempotencyKey(projectID, repo string, issueNumber int64, source SourceEvent) string {
	raw := fmt.Sprintf("triager:v1:%s:%s:%d:%s:%s:%s", projectID, strings.ToLower(strings.TrimSpace(repo)), issueNumber, source.Kind, source.OccurredAt, source.EventID)
	sum := sha256.Sum256([]byte(raw))
	return "triage:" + hex.EncodeToString(sum[:])
}

func reportEntityID(projectID, repo string, issueNumber int64) string {
	return fmt.Sprintf("%s:%s#%d", projectID, strings.ToLower(strings.TrimSpace(repo)), issueNumber)
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func eventID(value any) string {
	if text := stringValue(value); text != "" {
		return text
	}
	switch v := value.(type) {
	case float64:
		return fmt.Sprintf("%.0f", v)
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
