// Package triager turns fresh GitHub issue events into persisted, policy-
// validated routing reports.
package triager

import (
	"bytes"
	"context"
	"crypto/rand"
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
	EnrollmentEventType = "triage.enrolled"
	// ReportEventType is the Triage Report: Triager's durable structured
	// record of classification, scope, risk, confidence, missing information,
	// recommended next Role, rationale, the source Issue event, idempotency
	// key, and policy outcome. It is the semantic Authority for automatic
	// Planner routing; GitHub labels may project its outcome but cannot
	// replace it.
	ReportEventType       = "triage.report"
	ConfirmationEventType = "triage.confirmed"
	ProjectionEventType   = "triage.routed"
	RetirementEventType   = "triage.retired"

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
	defaultDecisionLimit                  = 1
	defaultSourceLookback                 = 5 * time.Minute
	autoRouteConfidence                   = 0.8
	reportEntityType                      = "github_issue"
	sourceEventNew        SourceEventKind = "new"
	sourceEventReopened   SourceEventKind = "reopened"
)

const DefaultDecisionLimit = defaultDecisionLimit

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
	// ConfirmationToken is minted before, and persisted with, the report. A
	// human must cite it in /plan, so a comment made before this report existed
	// cannot authorize Planner.
	ConfirmationToken string `json:"confirmationToken,omitempty"`
	CreatedAt         string `json:"createdAt"`
}

type Enrollment struct {
	Version        int         `json:"version"`
	IdempotencyKey string      `json:"idempotencyKey"`
	ProjectID      string      `json:"projectId"`
	Repo           string      `json:"repo"`
	IssueNumber    int64       `json:"issueNumber"`
	Source         SourceEvent `json:"source"`
	CommentID      int64       `json:"commentId"`
	EnrolledAt     string      `json:"enrolledAt"`
}

type Confirmation struct {
	ReportKey   string `json:"reportKey"`
	CommentID   int64  `json:"commentId"`
	Author      string `json:"author"`
	ConfirmedAt string `json:"confirmedAt"`
}

type Projection struct {
	ReportKey    string   `json:"reportKey"`
	QueueItemIDs []string `json:"queueItemIds,omitempty"`
	LoopIDs      []string `json:"loopIds,omitempty"`
	RoutedAt     string   `json:"routedAt"`
}

type Retirement struct {
	EnrollmentKey string `json:"enrollmentKey"`
	Reason        string `json:"reason"`
	RetiredAt     string `json:"retiredAt"`
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
	DecisionLimit  int
}

type Runner struct {
	repos          *storage.Repositories
	github         GitHubGateway
	llm            LLM
	planner        PlannerRouter
	now            func() time.Time
	sourceLookback time.Duration
	decisionLimit  int
}

type DiscoveryInput struct {
	ProjectID string
	Repo      string
	Snapshot  *githubinfra.DiscoverySnapshot
	// DecisionBudget is the tick-wide decision cap shared by every project in the
	// tick. Nil means uncapped.
	DecisionBudget *DecisionBudget
}

type DiscoveryResult struct {
	Enrolled             int
	DecisionsAttempted   int
	ReportsPersisted     int
	Routed               int
	AwaitingConfirmation int
	Confirmed            int
	Skipped              int
	Retired              int
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
	decisionLimit := options.DecisionLimit
	if decisionLimit <= 0 {
		decisionLimit = defaultDecisionLimit
	}
	return &Runner{
		repos: options.Repos, github: options.GitHub, llm: options.LLM,
		planner: options.Planner, now: now, sourceLookback: sourceLookback, decisionLimit: decisionLimit,
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
	states, err := r.loadSourceStates(ctx, input.ProjectID, input.Repo)
	if err != nil {
		return DiscoveryResult{}, err
	}
	result := DiscoveryResult{}
	cutoff := r.now().UTC().Add(-r.sourceLookback)
	issues, err := r.github.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{
		Repo: input.Repo, CWD: project.RepoPath, Limit: defaultIssueLimit,
		Search: fmt.Sprintf("updated:>=%s sort:updated-desc", cutoff.Format(time.RFC3339)),
	})
	if err != nil {
		return DiscoveryResult{}, err
	}
	// Issues returned here are exactly those updated inside the lookback window, so
	// this set is a free, exact answer to "could a confirmation have arrived?".
	touched := make(map[int64]struct{}, len(issues))
	for _, summary := range issues {
		touched[summary.Number] = struct{}{}
	}
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
		if !r.sourceIsRecent(source, cutoff) {
			result.Skipped++
			continue
		}
		if _, exists := states[key]; exists {
			continue
		}
		enrollment := Enrollment{
			Version: 1, IdempotencyKey: key, ProjectID: input.ProjectID, Repo: input.Repo,
			IssueNumber: detail.Number, Source: source, CommentID: latestCommentID(detail.Comments),
			EnrolledAt: r.now().UTC().Format(time.RFC3339Nano),
		}
		if err := r.persistEnrollment(ctx, enrollment); err != nil {
			return result, err
		}
		states[key] = &sourceState{enrollment: enrollment}
		result.Enrolled++
	}

	pending := pendingSourceStates(states)
	// A source parked on a human cannot change unless someone touches its issue, but
	// re-verifying it costs a ViewIssue and a ListIssueTimeline every tick. Left
	// uncapped that grows without bound, because nothing ever removes an awaiting
	// entry: the oldest here had been re-verified every tick for over seven hours.
	awaitingRechecks := selectAwaitingRechecks(pending, touched, awaitingRecheckBudget)
	for _, state := range pending {
		if awaitingExpired(state, r.now()) {
			// Bound the set. A retired source re-enrolls if its issue sees new activity,
			// so this drops the backlog rather than the work.
			if err := r.retireSource(ctx, state, RetirementReasonConfirmationTimeout, &result); err != nil {
				return result, err
			}
			continue
		}
		if awaitsHumanConfirmation(state) && !shouldProcessAwaiting(state, touched, awaitingRechecks) {
			result.Skipped++
			continue
		}
		if err := r.processSourceState(ctx, *project, input.Repo, state, input.DecisionBudget, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

type sourceState struct {
	enrollment Enrollment
	report     *Report
	projected  bool
	retired    bool
}

func (r *Runner) loadSourceStates(ctx context.Context, projectID, repo string) (map[string]*sourceState, error) {
	events, err := r.repos.Events.ListByProjectAndEntityType(ctx, projectID, reportEntityType)
	if err != nil {
		return nil, err
	}
	states := map[string]*sourceState{}
	for _, event := range events {
		switch event.EventType {
		case EnrollmentEventType:
			var enrollment Enrollment
			if err := json.Unmarshal([]byte(event.PayloadJSON), &enrollment); err != nil {
				return nil, fmt.Errorf("decode triage enrollment: %w", err)
			}
			if enrollment.ProjectID != projectID || !strings.EqualFold(strings.TrimSpace(enrollment.Repo), strings.TrimSpace(repo)) {
				continue
			}
			state := states[enrollment.IdempotencyKey]
			if state == nil {
				state = &sourceState{}
				states[enrollment.IdempotencyKey] = state
			}
			state.enrollment = enrollment
		case ReportEventType:
			var report Report
			if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
				return nil, fmt.Errorf("decode persisted triage report: %w", err)
			}
			if report.ProjectID != projectID || !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(repo)) {
				continue
			}
			state := states[report.IdempotencyKey]
			if state == nil {
				state = &sourceState{}
				states[report.IdempotencyKey] = state
			}
			copy := report
			state.report = &copy
			if state.enrollment.IdempotencyKey == "" {
				state.enrollment = Enrollment{
					Version: 1, IdempotencyKey: report.IdempotencyKey, ProjectID: report.ProjectID,
					Repo: report.Repo, IssueNumber: report.IssueNumber, Source: report.Source,
					EnrolledAt: report.CreatedAt,
				}
			}
		case ProjectionEventType:
			var projection Projection
			if err := json.Unmarshal([]byte(event.PayloadJSON), &projection); err != nil {
				return nil, fmt.Errorf("decode triage projection: %w", err)
			}
			state := states[projection.ReportKey]
			if state == nil {
				state = &sourceState{}
				states[projection.ReportKey] = state
			}
			state.projected = true
		case RetirementEventType:
			var retirement Retirement
			if err := json.Unmarshal([]byte(event.PayloadJSON), &retirement); err != nil {
				return nil, fmt.Errorf("decode triage retirement: %w", err)
			}
			state := states[retirement.EnrollmentKey]
			if state == nil {
				state = &sourceState{}
				states[retirement.EnrollmentKey] = state
			}
			state.retired = true
		}
	}
	for key, state := range states {
		if state.enrollment.IdempotencyKey == "" {
			delete(states, key)
		}
	}
	return states, nil
}

func pendingSourceStates(states map[string]*sourceState) []*sourceState {
	pending := make([]*sourceState, 0, len(states))
	for _, state := range states {
		if state.projected || state.retired {
			continue
		}
		pending = append(pending, state)
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].enrollment.EnrolledAt != pending[j].enrollment.EnrolledAt {
			return pending[i].enrollment.EnrolledAt < pending[j].enrollment.EnrolledAt
		}
		return pending[i].enrollment.IdempotencyKey < pending[j].enrollment.IdempotencyKey
	})
	return pending
}

func (r *Runner) processSourceState(ctx context.Context, project storage.ProjectRecord, repo string, state *sourceState, decisionBudget *DecisionBudget, result *DiscoveryResult) error {
	enrollment := state.enrollment
	detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: enrollment.IssueNumber, CWD: project.RepoPath})
	if err != nil {
		return err
	}
	if !eligibleTarget(detail) {
		return r.retireSource(ctx, state, "source_no_longer_eligible", result)
	}
	if domain.IsAutoLaneHeld(domain.LoopTypePlanner, detail.Labels) {
		result.Skipped++
		return nil
	}
	currentSource, matches, err := r.sourceStillCurrent(ctx, repo, project.RepoPath, detail, enrollment)
	if err != nil {
		return err
	}
	if !matches {
		return r.retireSource(ctx, state, "source_superseded", result)
	}
	detailSource := currentSource
	report := state.report
	if report == nil {
		// The per-runner limit is checked first because it is this project's own
		// count and must not consume a shared reservation. Reserve is the atomic
		// check-and-claim against the tick-wide budget: concurrent projects can no
		// longer each observe the same remaining count and all proceed.
		if result.DecisionsAttempted >= r.decisionLimit {
			return nil
		}
		if !decisionBudget.Reserve() {
			return nil
		}
		result.DecisionsAttempted++
		decision, err := r.decide(ctx, project, repo, detail)
		if err != nil {
			return err
		}
		refreshed, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: enrollment.IssueNumber, CWD: project.RepoPath})
		if err != nil {
			return err
		}
		if !eligibleTarget(refreshed) {
			return r.retireSource(ctx, state, "source_no_longer_eligible", result)
		}
		if domain.IsAutoLaneHeld(domain.LoopTypePlanner, refreshed.Labels) {
			result.Skipped++
			return nil
		}
		refreshedSource, matches, err := r.sourceStillCurrent(ctx, repo, project.RepoPath, refreshed, enrollment)
		if err != nil {
			return err
		}
		if !matches {
			return r.retireSource(ctx, state, "source_superseded", result)
		}
		detail = refreshed
		detailSource = refreshedSource
		created := r.now().UTC().Format(time.RFC3339Nano)
		value := Report{
			Version: 2, IdempotencyKey: enrollment.IdempotencyKey, ProjectID: enrollment.ProjectID, Repo: enrollment.Repo,
			IssueNumber: enrollment.IssueNumber, Source: detailSource, Decision: decision,
			Policy: validateDecision(decision), CreatedAt: created,
		}
		if value.Policy.Action == ActionAwaitHuman {
			token, err := newConfirmationToken()
			if err != nil {
				return err
			}
			value.ConfirmationToken = token
		}
		if err := r.persistReport(ctx, value); err != nil {
			return err
		}
		report = &value
		state.report = report
		result.ReportsPersisted++
	}
	action := report.Policy.Action
	if action == ActionAwaitHuman {
		confirmed, created, err := r.confirmedByHuman(ctx, repo, project.RepoPath, detail, *report)
		if err != nil {
			return err
		}
		if !confirmed {
			result.AwaitingConfirmation++
			return nil
		}
		if created {
			result.Confirmed++
		}
		action = ActionRoutePlanner
	}
	if action != ActionRoutePlanner {
		return fmt.Errorf("triage report %s has unknown policy action %q", report.IdempotencyKey, action)
	}
	route, err := r.planner.RouteIssue(ctx, planner.RouteIssueInput{
		ProjectID: enrollment.ProjectID, Repo: repo, Authority: report.IdempotencyKey,
		Issue: planner.IssueSummary{
			Number: detail.Number, Title: detail.Title, Body: detail.Body, URL: detail.URL,
			Assignees: append([]string(nil), detail.Assignees...), Labels: append([]string(nil), detail.Labels...),
		},
	})
	if err != nil {
		return err
	}
	if !route.ProjectionAccepted {
		result.Skipped++
		return nil
	}
	if err := r.persistProjection(ctx, *report, route); err != nil {
		return err
	}
	state.projected = true
	result.Routed++
	result.QueueItems = append(result.QueueItems, route.QueueItems...)
	return nil
}

func (r *Runner) sourceStillCurrent(ctx context.Context, repo, cwd string, detail githubinfra.IssueDetail, enrollment Enrollment) (SourceEvent, bool, error) {
	timeline, err := r.github.ListIssueTimeline(ctx, githubinfra.IssueTimelineInput{Repo: repo, IssueNumber: detail.Number, CWD: cwd})
	if err != nil {
		return SourceEvent{}, false, err
	}
	source, ok := sourceEvent(detail, timeline)
	if !ok {
		return SourceEvent{}, false, nil
	}
	key := buildIdempotencyKey(enrollment.ProjectID, enrollment.Repo, enrollment.IssueNumber, source)
	return source, key == enrollment.IdempotencyKey, nil
}

func (r *Runner) retireSource(ctx context.Context, state *sourceState, reason string, result *DiscoveryResult) error {
	retirement := Retirement{EnrollmentKey: state.enrollment.IdempotencyKey, Reason: reason, RetiredAt: r.now().UTC().Format(time.RFC3339Nano)}
	if err := r.persistRetirement(ctx, state.enrollment, retirement); err != nil {
		return err
	}
	state.retired = true
	result.Retired++
	result.Skipped++
	return nil
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
	confirmationToken := strings.TrimSpace(report.ConfirmationToken)
	if confirmationToken == "" {
		return false, false, fmt.Errorf("triage report %s is missing its confirmation token", report.IdempotencyKey)
	}
	for _, comment := range issue.Comments {
		if strings.TrimSpace(comment.Body) != confirmationCommand(confirmationToken) || strings.TrimSpace(comment.Author) == "" {
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

func newConfirmationToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate triage confirmation token: %w", err)
	}
	return "triage-confirm-" + hex.EncodeToString(value[:]), nil
}

func confirmationCommand(token string) string {
	return "/plan " + token
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

func (r *Runner) persistEnrollment(ctx context.Context, enrollment Enrollment) error {
	payload, err := json.Marshal(enrollment)
	if err != nil {
		return err
	}
	projectID := enrollment.ProjectID
	entityType := reportEntityType
	entityID := reportEntityID(enrollment.ProjectID, enrollment.Repo, enrollment.IssueNumber)
	actorType, actorID := "system", "triager"
	return r.repos.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID("triage-enroll"), EventType: EnrollmentEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(payload), CreatedAt: enrollment.EnrolledAt,
	})
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

func (r *Runner) persistProjection(ctx context.Context, report Report, route planner.DiscoveryResult) error {
	projection := Projection{ReportKey: report.IdempotencyKey, RoutedAt: r.now().UTC().Format(time.RFC3339Nano)}
	for _, item := range route.QueueItems {
		projection.QueueItemIDs = append(projection.QueueItemIDs, item.ID)
	}
	projection.LoopIDs = append(projection.LoopIDs, route.CreatedLoopIDs...)
	payload, err := json.Marshal(projection)
	if err != nil {
		return err
	}
	projectID := report.ProjectID
	entityType := reportEntityType
	entityID := reportEntityID(report.ProjectID, report.Repo, report.IssueNumber)
	actorType, actorID := "system", "triager"
	return r.repos.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID("triage-route"), EventType: ProjectionEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(payload), CreatedAt: projection.RoutedAt,
	})
}

func (r *Runner) persistRetirement(ctx context.Context, enrollment Enrollment, retirement Retirement) error {
	payload, err := json.Marshal(retirement)
	if err != nil {
		return err
	}
	projectID := enrollment.ProjectID
	entityType := reportEntityType
	entityID := reportEntityID(enrollment.ProjectID, enrollment.Repo, enrollment.IssueNumber)
	actorType, actorID := "system", "triager"
	return r.repos.Events.Append(ctx, storage.EventLogRecord{
		ID: eventlog.NewEventID("triage-retire"), EventType: RetirementEventType, ProjectID: &projectID,
		EntityType: &entityType, EntityID: &entityID, ActorType: &actorType, ActorID: &actorID,
		PayloadJSON: string(payload), CreatedAt: retirement.RetiredAt,
	})
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

func latestCommentID(comments []githubinfra.CommentInfo) int64 {
	var latest int64
	for _, comment := range comments {
		if comment.ID > latest {
			latest = comment.ID
		}
	}
	return latest
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
