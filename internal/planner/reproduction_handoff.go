package planner

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// BranchForIssue is the branch Planner will work on for an Issue.
//
// It is exported so a Role that runs *before* Planner (Reproducer) can prepare
// exactly the branch Planner later adopts. Planner's worktree creation already
// attaches to an existing branch rather than failing on it, so a reproduction
// commit placed here needs no change to Planner's step machine — the spec is
// simply authored on top of a demonstrated failure.
func BranchForIssue(issueNumber int64, title string) string {
	return buildPlannerBranch(issueNumber, title)
}

// ParkIssueInput describes an Issue that must wait for a human before Planner
// does any work on it.
type ParkIssueInput struct {
	Project   storage.ProjectRecord
	Repo      string
	Issue     IssueSummary
	Authority string
	Ask       loops.HITLAsk
}

// ParkIssueForHuman materializes the Planner loop for an Issue directly into
// `awaiting_human` and does not enqueue any Planner work.
//
// This is the "stop before paying for planning" exit. It deliberately reuses
// the existing HITL suspension rather than inventing a parallel one: the loop
// is a normal Planner loop, `/respond` resumes it through the ordinary path,
// and discovery already treats `awaiting_human` as non-requeueable, so the
// Issue neither retries nor reads as a crash.
func (r *Runner) ParkIssueForHuman(ctx context.Context, input ParkIssueInput) (storage.LoopRecord, error) {
	if r.repos == nil || r.repos.Loops == nil {
		return storage.LoopRecord{}, fmt.Errorf("planner repositories are not configured")
	}
	upsert, err := r.ensureLoopForIssueWithAuthority(ctx, input.Project, input.Repo, input.Issue, "", input.Authority)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if upsert.blocked {
		return upsert.record, nil
	}
	loop := upsert.record
	if loop.Status == string(domain.LoopStatusAwaitingHuman) {
		return loop, nil
	}
	ask := input.Ask
	if strings.TrimSpace(ask.Status) == "" {
		ask.Status = "awaiting"
	}
	if strings.TrimSpace(ask.AskedAt) == "" {
		ask.AskedAt = r.nowISO()
	}
	metadataJSON, err := loops.WriteHITLAsk(loop.MetadataJSON, ask)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	updated := loop
	updated.MetadataJSON = &metadataJSON
	updated.Status = string(domain.LoopStatusAwaitingHuman)
	updated.NextRunAt = nil
	updated.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, updated); err != nil {
		return storage.LoopRecord{}, err
	}
	r.appendEvent(ctx, eventInput{
		eventType: "loop.awaiting_human", projectID: input.Project.ID, loopID: updated.ID,
		entityType: "loop", entityID: updated.ID,
		payload: map[string]any{"reason": "reproduction_unavailable", "repo": input.Repo, "issueNumber": input.Issue.Number},
	})
	return updated, nil
}
