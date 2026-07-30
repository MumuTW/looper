package triager

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nexu-io/looper/internal/storage"
)

// LoadAcceptedReports returns the persisted Triage Reports for one project and
// repository that are authorized to reach Planner and have not reached it yet.
//
// It exists so a Role scheduled between Triager and Planner can read Triager's
// durable Authority directly instead of re-deriving a classification of its
// own. A report held by policy counts as accepted only once its
// write-authorized `triage.confirmed` record exists, which is the same rule
// Triager itself applies before routing.
//
// A report that already has a `triage.routed` projection is excluded, because
// "before Planner" is the only position from which a pre-Planner Role can act.
// Turning the Role on in a repository with open, in-flight bug reports would
// otherwise hand it work Planner had already started: it would restore the same
// branch and worktree and could commit a reproduction into active work. In the
// normal flow nothing is lost, since routing is itself gated on the Role having
// settled the report first.
func LoadAcceptedReports(ctx context.Context, repos *storage.Repositories, projectID, repo string) ([]Report, error) {
	if repos == nil || repos.Events == nil {
		return nil, fmt.Errorf("events repository is not configured")
	}
	events, err := repos.Events.ListByProjectAndEntityType(ctx, projectID, reportEntityType)
	if err != nil {
		return nil, err
	}
	reports := map[string]Report{}
	confirmed := map[string]struct{}{}
	retired := map[string]struct{}{}
	projected := map[string]struct{}{}
	for _, event := range events {
		switch event.EventType {
		case ReportEventType:
			var report Report
			if err := json.Unmarshal([]byte(event.PayloadJSON), &report); err != nil {
				return nil, fmt.Errorf("decode persisted triage report: %w", err)
			}
			if report.ProjectID != projectID || !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(repo)) {
				continue
			}
			reports[report.IdempotencyKey] = report
		case ConfirmationEventType:
			var confirmation Confirmation
			if err := json.Unmarshal([]byte(event.PayloadJSON), &confirmation); err != nil {
				return nil, fmt.Errorf("decode triage confirmation: %w", err)
			}
			confirmed[confirmation.ReportKey] = struct{}{}
		case RetirementEventType:
			var retirement Retirement
			if err := json.Unmarshal([]byte(event.PayloadJSON), &retirement); err != nil {
				return nil, fmt.Errorf("decode triage retirement: %w", err)
			}
			retired[retirement.EnrollmentKey] = struct{}{}
		case ProjectionEventType:
			var projection Projection
			if err := json.Unmarshal([]byte(event.PayloadJSON), &projection); err != nil {
				return nil, fmt.Errorf("decode triage projection: %w", err)
			}
			projected[projection.ReportKey] = struct{}{}
		}
	}
	accepted := make([]Report, 0, len(reports))
	for key, report := range reports {
		if _, gone := retired[key]; gone {
			continue
		}
		if _, routed := projected[key]; routed {
			continue
		}
		if report.Policy.Action == ActionRoutePlanner {
			accepted = append(accepted, report)
			continue
		}
		if _, ok := confirmed[key]; ok {
			accepted = append(accepted, report)
		}
	}
	sort.Slice(accepted, func(i, j int) bool {
		if accepted[i].CreatedAt != accepted[j].CreatedAt {
			return accepted[i].CreatedAt < accepted[j].CreatedAt
		}
		return accepted[i].IdempotencyKey < accepted[j].IdempotencyKey
	})
	return accepted, nil
}
