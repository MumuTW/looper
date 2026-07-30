package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

// settleQueuedWorkerIssueTargets retires queued worker items whose target issue
// has closed while the item waited in the queue, so the worker is not claimed
// hours later only to fail on "is closed; worker issue targets must reference an
// open GitHub issue". It reuses the open-issue list the discovery pass already
// fetched for this project's snapshot — no per-item forge call.
//
// Absence from the snapshot's open-issue set only proves closure when the set is
// complete (the forge returned fewer issues than the fetch limit). An incomplete
// list cannot distinguish a closed issue from one past the limit, so the pass
// no-ops rather than retiring work that might still be open. This is the
// deliberately bounded half of the issue: a merged pull request that closes the
// issue is caught here (the issue leaves the open set); a merged PR that does
// not close the issue is not — see the PR description for what it still misses.
func settleQueuedWorkerIssueTargets(ctx context.Context, repos *storage.Repositories, snapshot *github.DiscoverySnapshot, projectID, repo string, nowISO string, logger func(string, map[string]any)) {
	if repos == nil || repos.Queue == nil || repos.Loops == nil || snapshot == nil {
		return
	}
	openIssues, complete, ok := snapshot.OpenIssueNumbers()
	if !ok || !complete {
		return
	}
	queued, err := repos.Queue.ListByStatuses(ctx, []string{"queued"})
	if err != nil {
		if logger != nil {
			logger("queue settle skipped", map[string]any{"projectId": projectID, "error": err.Error()})
		}
		return
	}
	for _, item := range queued {
		if item.Type != string(domain.LoopTypeWorker) || item.TargetType != string(domain.LoopTargetTypeIssue) {
			continue
		}
		if item.LoopID == nil || strings.TrimSpace(*item.LoopID) == "" {
			continue
		}
		itemRepo := derefString(item.Repo)
		if itemRepo == "" {
			continue
		}
		if itemRepo != repo {
			continue
		}
		itemProjectID := derefString(item.ProjectID)
		if itemProjectID != projectID {
			continue
		}
		issueNumber, parseErr := parseIssueNumberFromTargetID(item.TargetID)
		if parseErr != nil || issueNumber <= 0 {
			continue
		}
		if _, stillOpen := openIssues[issueNumber]; stillOpen {
			continue
		}
		reference := fmt.Sprintf("%s#%d", itemRepo, issueNumber)
		reason := fmt.Sprintf("Worker retired from queue: %s is no longer an open issue", reference)
		if _, cancelErr := repos.Queue.CancelByLoop(ctx, *item.LoopID, nowISO, &reason); cancelErr != nil {
			if logger != nil {
				logger("queue settle cancel failed", map[string]any{"projectId": projectID, "loopId": *item.LoopID, "error": cancelErr.Error()})
			}
			continue
		}
		// Move the loop out of queued so the dashboard does not show stale
		// queued work for a target that is gone. Paused (not failed) because the
		// worker never ran — there is no run to mark failed, and reusing pause
		// keeps it eligible for a human-driven requeue if the closure was wrong.
		if loop, loopErr := repos.Loops.GetByID(ctx, *item.LoopID); loopErr == nil && loop != nil {
			if domain.LoopStatus(loop.Status) == domain.LoopStatusQueued {
				loop.Status = string(domain.LoopStatusPaused)
				loop.NextRunAt = nil
				loop.UpdatedAt = nowISO
				_ = repos.Loops.Upsert(ctx, *loop)
			}
		}
		if logger != nil {
			logger("queue settled retired stale worker", map[string]any{"projectId": projectID, "loopId": *item.LoopID, "issue": reference})
		}
	}
}
