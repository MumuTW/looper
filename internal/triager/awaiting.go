package triager

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/storage"
)

const (
	// awaitingRecheckBudget bounds how many quiet awaiting-confirmation sources are
	// re-verified per tick. Each re-verification costs a ViewIssue and a
	// ListIssueTimeline, so the uncapped behaviour made this lane's cost grow
	// linearly with the number of issues waiting on a human — permanently, since a
	// source waiting for confirmation never resolves on its own.
	//
	// Sources whose issue was touched in this tick's update window are always
	// re-verified and do not consume this budget; the budget exists only so a
	// confirmation that arrived during a gap in that window is still noticed.
	awaitingRecheckBudget = 3

	// awaitingConfirmationTTL is how long a source may wait for a human before it is
	// retired. Without it the awaiting set only grows: nothing in the system ever
	// removes an entry, so every issue that ever asked for confirmation is
	// re-verified forever. A retired source is re-enrolled if its issue sees new
	// activity, so this loses nothing except the unbounded backlog.
	awaitingConfirmationTTL = 7 * 24 * time.Hour

	RetirementReasonConfirmationTimeout = "confirmation_timeout"
)

// awaitsHumanConfirmation reports whether this source is parked on a human, which
// is what makes re-verifying it every tick pointless: the answer can only change
// when someone comments on the issue.
func awaitsHumanConfirmation(state *sourceState) bool {
	return state != nil && state.report != nil && state.report.Policy.Action == ActionAwaitHuman
}

// awaitingExpired reports whether a source has waited past awaitingConfirmationTTL.
func awaitingExpired(state *sourceState, now time.Time) bool {
	if !awaitsHumanConfirmation(state) {
		return false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, state.report.CreatedAt)
	if err != nil {
		return false
	}
	return now.UTC().Sub(createdAt.UTC()) >= awaitingConfirmationTTL
}

type awaitingRecheckCursor struct {
	enrolledAt string
	key        string
}

func (r *Runner) selectAwaitingRechecks(projectID, repo string, pending []*sourceState, touched map[int64]struct{}, budget int) map[string]struct{} {
	cursorKey := strings.TrimSpace(projectID) + "\x00" + strings.ToLower(strings.TrimSpace(repo))
	r.awaitingMu.Lock()
	defer r.awaitingMu.Unlock()
	selected, next := selectAwaitingRechecks(pending, touched, budget, r.awaitingCursor[cursorKey])
	if len(selected) > 0 {
		r.awaitingCursor[cursorKey] = next
	}
	return selected
}

// selectAwaitingRechecks decides which quiet awaiting-confirmation sources to
// re-verify this tick and advances after the last source actually selected.
// The cursor is only a scheduling hint: event-log lifecycle remains authority,
// and a daemon restart safely begins another pass from the oldest source.
func selectAwaitingRechecks(pending []*sourceState, touched map[int64]struct{}, budget int, after awaitingRecheckCursor) (map[string]struct{}, awaitingRecheckCursor) {
	selected := make(map[string]struct{})
	if budget <= 0 {
		return selected, after
	}
	candidates := make([]*sourceState, 0, len(pending))
	for _, state := range pending {
		if !awaitsHumanConfirmation(state) {
			continue
		}
		if _, ok := touched[state.enrollment.IssueNumber]; ok {
			// Touched sources are re-verified unconditionally, so they must not spend
			// budget meant for the quiet ones.
			continue
		}
		candidates = append(candidates, state)
	}
	if len(candidates) == 0 {
		return selected, after
	}
	start := 0
	for index, state := range candidates {
		candidate := awaitingRecheckCursor{enrolledAt: state.enrollment.EnrolledAt, key: state.enrollment.IdempotencyKey}
		if candidate.enrolledAt > after.enrolledAt || (candidate.enrolledAt == after.enrolledAt && candidate.key > after.key) {
			start = index
			break
		}
		if index == len(candidates)-1 {
			start = 0
		}
	}
	count := budget
	if count > len(candidates) {
		count = len(candidates)
	}
	next := after
	for offset := 0; offset < count; offset++ {
		state := candidates[(start+offset)%len(candidates)]
		selected[state.enrollment.IdempotencyKey] = struct{}{}
		next = awaitingRecheckCursor{enrolledAt: state.enrollment.EnrolledAt, key: state.enrollment.IdempotencyKey}
	}
	return selected, next
}

// shouldProcessAwaiting reports whether an awaiting-confirmation source is worth a
// forge round trip this tick.
func shouldProcessAwaiting(state *sourceState, touched map[int64]struct{}, rechecks map[string]struct{}) bool {
	if _, ok := touched[state.enrollment.IssueNumber]; ok {
		return true
	}
	_, selected := rechecks[state.enrollment.IdempotencyKey]
	return selected
}

// AwaitingConfirmationSource is a read-only status projection of one triage
// report that still needs a human confirmation. CreatedAt and AgeSeconds are
// derived from the report already persisted as the semantic authority; neither
// field introduces a second lifecycle record.
type AwaitingConfirmationSource struct {
	ProjectID   string `json:"projectId"`
	Repo        string `json:"repo"`
	IssueNumber int64  `json:"issueNumber"`
	CreatedAt   string `json:"createdAt"`
	AgeSeconds  int64  `json:"ageSeconds"`
	// Command is the exact comment that confirms this source, carrying the
	// report's confirmation token. Reporting that a source is waiting is only
	// half the ask: the token is minted per report and lives nowhere an operator
	// reads, so without it the verdict stays unreachable — which is the defect
	// in #255, not the missing counter.
	//
	// It is empty only for a report persisted without a token, which no
	// operator could confirm anyway.
	Command string `json:"command,omitempty"`
}

// AwaitingConfirmationSummary is the operator-facing live projection of
// triage reports that are waiting on a human. Count is repeated deliberately so
// status consumers can render a cheap headline without inferring it from a
// truncated roster; Sources remains the complete read result.
type AwaitingConfirmationSummary struct {
	Count   int                          `json:"count"`
	Sources []AwaitingConfirmationSource `json:"sources"`
}

type awaitingConfirmationState struct {
	report    *Report
	confirmed bool
	projected bool
	retired   bool
}

// AwaitingConfirmationStatus derives the outstanding human-confirmation roster
// from the existing triage lifecycle events. It is intentionally read-only:
// the report is the authority, while this projection only tells an operator
// which reports need attention.
func AwaitingConfirmationStatus(ctx context.Context, repositories *storage.Repositories, now time.Time) (AwaitingConfirmationSummary, error) {
	summary := AwaitingConfirmationSummary{Sources: []AwaitingConfirmationSource{}}
	if repositories == nil || repositories.Events == nil || repositories.Projects == nil {
		return summary, fmt.Errorf("triager repositories are not configured")
	}
	projects, err := repositories.Projects.List(ctx)
	if err != nil {
		return summary, fmt.Errorf("list projects for triage status: %w", err)
	}
	activeProjectIDs := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		if !project.Archived {
			activeProjectIDs[project.ID] = struct{}{}
		}
	}
	events, err := repositories.Events.ListByEntityTypeAndEventTypes(ctx, reportEntityType, []string{
		ReportEventType,
		ConfirmationEventType,
		ProjectionEventType,
		RetirementEventType,
	})
	if err != nil {
		return summary, fmt.Errorf("list triage lifecycle events: %w", err)
	}
	states := map[string]*awaitingConfirmationState{}
	stateFor := func(key string) *awaitingConfirmationState {
		state := states[key]
		if state == nil {
			state = &awaitingConfirmationState{}
			states[key] = state
		}
		return state
	}
	for _, event := range events {
		switch event.EventType {
		case ReportEventType:
			var report Report
			if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
				return summary, fmt.Errorf("decode triage report for status: %w", err)
			}
			if report.IdempotencyKey == "" {
				return summary, fmt.Errorf("decode triage report for status: missing idempotency key")
			}
			copy := report
			stateFor(report.IdempotencyKey).report = &copy
		case ConfirmationEventType:
			var confirmation Confirmation
			if err := json.Unmarshal([]byte(event.PayloadJSON), &confirmation); err != nil {
				return summary, fmt.Errorf("decode triage confirmation for status: %w", err)
			}
			if confirmation.ReportKey != "" {
				stateFor(confirmation.ReportKey).confirmed = true
			}
		case ProjectionEventType:
			var projection Projection
			if err := json.Unmarshal([]byte(event.PayloadJSON), &projection); err != nil {
				return summary, fmt.Errorf("decode triage projection for status: %w", err)
			}
			if projection.ReportKey != "" {
				stateFor(projection.ReportKey).projected = true
			}
		case RetirementEventType:
			var retirement Retirement
			if err := json.Unmarshal([]byte(event.PayloadJSON), &retirement); err != nil {
				return summary, fmt.Errorf("decode triage retirement for status: %w", err)
			}
			if retirement.EnrollmentKey != "" {
				stateFor(retirement.EnrollmentKey).retired = true
			}
		}
	}
	for _, state := range states {
		if state.report == nil || state.confirmed || state.projected || state.retired || state.report.Policy.Action != ActionAwaitHuman {
			continue
		}
		if _, active := activeProjectIDs[state.report.ProjectID]; !active {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, state.report.CreatedAt)
		if err != nil {
			return summary, fmt.Errorf("parse awaiting triage report %s createdAt: %w", state.report.IdempotencyKey, err)
		}
		age := now.UTC().Sub(createdAt.UTC())
		if age < 0 {
			age = 0
		}
		source := AwaitingConfirmationSource{
			ProjectID: state.report.ProjectID, Repo: state.report.Repo, IssueNumber: state.report.IssueNumber,
			CreatedAt: state.report.CreatedAt, AgeSeconds: int64(age / time.Second),
		}
		if token := strings.TrimSpace(state.report.ConfirmationToken); token != "" {
			source.Command = confirmationCommand(token)
		}
		summary.Sources = append(summary.Sources, source)
	}
	sort.Slice(summary.Sources, func(i, j int) bool {
		if summary.Sources[i].CreatedAt != summary.Sources[j].CreatedAt {
			return summary.Sources[i].CreatedAt < summary.Sources[j].CreatedAt
		}
		if summary.Sources[i].Repo != summary.Sources[j].Repo {
			return summary.Sources[i].Repo < summary.Sources[j].Repo
		}
		return summary.Sources[i].IssueNumber < summary.Sources[j].IssueNumber
	})
	summary.Count = len(summary.Sources)
	return summary, nil
}
