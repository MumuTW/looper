package reproducer

import (
	"context"
	"fmt"

	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/triager"
)

// Gate is the pre-Planner half of the Role: it decides whether an accepted
// Triage Report may reach Planner yet.
//
// Bug-only, by construction. A feature, docs, refactor or chore report is
// allowed through untouched, because Planner's spec and acceptance criteria
// already serve that work and gating it here would only rename Planner.
type Gate struct {
	repos *storage.Repositories
}

// NewGate builds the Triager-side gate.
func NewGate(repos *storage.Repositories) *Gate {
	return &Gate{repos: repos}
}

// PlannerAllowed implements triager.ReproductionGate.
func (g *Gate) PlannerAllowed(ctx context.Context, report triager.Report) (bool, error) {
	if g == nil || g.repos == nil {
		return false, fmt.Errorf("reproduction gate repositories are not configured")
	}
	if report.Decision.Classification != triager.ClassificationBug {
		return true, nil
	}
	status, err := reproduction.LoadStatus(ctx, g.repos, report.ProjectID, report.Repo, report.IssueNumber)
	if err != nil {
		return false, err
	}
	return status.PlannerAllowed(), nil
}
