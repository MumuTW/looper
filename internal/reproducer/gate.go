package reproducer

import (
	"context"
	"fmt"

	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/triager"
)

// Gate decides whether an Issue may reach Planner yet.
//
// It answers the same question through two doors, because Planner has two.
// Triager's explicit route asks about a report it already holds; Planner's own
// label/assignee discovery asks about a bare Issue number and has to find the
// governing report itself. Wiring only the first door left the second one open,
// which is the whole reason both live here rather than one being a special case
// of the other.
//
// Bug-only, by construction. A feature, docs, refactor or chore report is
// allowed through untouched, because Planner's spec and acceptance criteria
// already serve that work and gating it here would only rename Planner. An
// Issue with no accepted report at all is likewise untouched: it reached
// discovery through labels or an assignee and keeps today's path exactly.
type Gate struct {
	repos *storage.Repositories
}

// NewGate builds the gate.
func NewGate(repos *storage.Repositories) *Gate {
	return &Gate{repos: repos}
}

// PlannerAllowed implements triager.ReproductionGate.
func (g *Gate) PlannerAllowed(ctx context.Context, report triager.Report) (bool, error) {
	if g == nil || g.repos == nil {
		return false, fmt.Errorf("reproduction gate repositories are not configured")
	}
	return g.allowedForReport(ctx, report)
}

// IssueAllowed implements planner.ReproductionGate: it answers for an Issue
// discovered by label or assignee rather than routed from a report.
//
// The lookup is report-first on purpose. Discovery hands us nothing but a
// number, so the gate re-derives which accepted report governs that Issue and
// then asks exactly the question the Triager door asks. An Issue with no
// accepted bug report is allowed through, which is what keeps the gate inert
// for every repository that does not run Triager.
func (g *Gate) IssueAllowed(ctx context.Context, projectID, repo string, issueNumber int64) (bool, error) {
	if g == nil || g.repos == nil {
		return false, fmt.Errorf("reproduction gate repositories are not configured")
	}
	if issueNumber <= 0 {
		return true, nil
	}
	reports, err := triager.LoadAcceptedReports(ctx, g.repos, projectID, repo)
	if err != nil {
		return false, err
	}
	governing, found := latestReportForIssue(reports, issueNumber)
	if !found {
		return true, nil
	}
	return g.allowedForReport(ctx, governing)
}

// latestReportForIssue picks the newest accepted report for one Issue. Reports
// are ordered oldest-first by LoadAcceptedReports, so the last match is the one
// a superseding comment or edit produced.
func latestReportForIssue(reports []triager.Report, issueNumber int64) (triager.Report, bool) {
	var latest triager.Report
	found := false
	for _, report := range reports {
		if report.IssueNumber == issueNumber {
			latest, found = report, true
		}
	}
	return latest, found
}

func (g *Gate) allowedForReport(ctx context.Context, report triager.Report) (bool, error) {
	if report.Decision.Classification != triager.ClassificationBug {
		return true, nil
	}
	status, err := reproduction.LoadStatus(ctx, g.repos, report.ProjectID, report.Repo, report.IssueNumber)
	if err != nil {
		return false, err
	}
	// The key binds the decision to this report. A record or waiver minted
	// against a superseded report authorizes nothing here.
	key := reproduction.IdempotencyKey(report.ProjectID, report.Repo, report.IssueNumber, report.IdempotencyKey)
	return status.PlannerAllowed(key), nil
}
