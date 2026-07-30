package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

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
func settleQueuedWorkerIssueTargets(ctx context.Context, repos *storage.Repositories, snapshot *github.DiscoverySnapshot, claimBoundary *sync.RWMutex, projectID, repo string, nowISO string, logger func(string, map[string]any)) error {
	if repos == nil || repos.Queue == nil || repos.Loops == nil || snapshot == nil {
		return nil
	}
	// The same boundary that guards ClaimNext* protects the fresh read and the
	// retirement it authorizes, so a claim cannot slip between them.
	if claimBoundary != nil {
		claimBoundary.Lock()
		defer claimBoundary.Unlock()
	}
	if err := snapshot.EnsureFreshOpenIssues(ctx, github.ListOpenIssuesInput{Repo: repo}); err != nil {
		return fmt.Errorf("refresh open issues before queue settlement: %w", err)
	}
	openIssues, complete, ok := snapshot.OpenIssueNumbers()
	if !ok || !complete {
		return nil
	}
	queued, err := repos.Queue.ListByStatuses(ctx, []string{"queued"})
	if err != nil {
		if logger != nil {
			logger("queue settle skipped", map[string]any{"projectId": projectID, "error": err.Error()})
		}
		return err
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
		unlockLoop := LockLoopRequeue(*item.LoopID)
		loop, loopErr := repos.Loops.GetByID(ctx, *item.LoopID)
		if loopErr != nil || loop == nil {
			unlockLoop()
			continue
		}
		unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*loop))
		// Re-check under both existing requeue guards: an API retry/requeue may
		// have changed either record while this scan waited for the locks.
		current, currentErr := repos.Queue.GetByID(ctx, item.ID)
		if currentErr != nil || current == nil || current.Status != "queued" || loop.Status != "queued" {
			unlockTarget()
			unlockLoop()
			continue
		}
		reference := fmt.Sprintf("%s#%d", itemRepo, issueNumber)
		reason := fmt.Sprintf("Worker retired from queue: %s is no longer an open issue", reference)
		retired, retireErr := repos.RetireQueuedLoop(ctx, *item.LoopID, item.ID, nowISO, &reason)
		unlockTarget()
		unlockLoop()
		if retireErr != nil {
			if logger != nil {
				logger("queue settle retirement failed", map[string]any{"projectId": projectID, "loopId": *item.LoopID, "error": retireErr.Error()})
			}
			continue
		}
		if !retired {
			continue
		}
		if logger != nil {
			logger("queue settled retired stale worker", map[string]any{"projectId": projectID, "loopId": *item.LoopID, "issue": reference})
		}
	}
	return nil
}
