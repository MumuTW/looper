package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// shepherdReconcileLockTTL matches the worker's shepherd lock TTL; the reconciler
// refreshes it every tick (tick << TTL) so a same-account fixer keeps skipping.
const shepherdReconcileLockTTL = 20 * time.Minute

// shepherdReconcileInterval is the dedicated shepherd poll cadence. looper has no
// per-PR-event webhook seam, so a worker loop driving its PR to merge polls on
// this cadence (well under the lock TTL) to pick up a re-review / CI change /
// conflict / a human's merge promptly — independent of the slower 5-min
// wake-reconcile.
const shepherdReconcileInterval = 60 * time.Second

// shepherdMaxPasses caps how many agent passes one PR may consume before the
// shepherd stops waking and leaves it for a human — a backstop against a flapping
// PR burning agent quota. Counts only agent-waking passes.
const shepherdMaxPasses = 25

// reconcileWorkerShepherd drives a worker loop that is shepherding its own impl
// PR toward merge. Polling (looper has no per-PR-event webhook seam): for each
// loop in status "shepherding" it refreshes the coordination lock, reads live PR
// state via the full GitHub gateway, and:
//   - state MERGED (a HUMAN colleague merged) → terminal completed+outcome=merged;
//     CLOSED (not merged) → completed+outcome=abandoned. Releases the lock.
//   - else, when the folded signal changed AND something is actionable
//     (CHANGES_REQUESTED / failing CI / conflict), enqueue ONE worker pass (the
//     resolver forces stepShepherd via the durable marker). No wake when nothing
//     is actionable — the bot NEVER merges; it just keeps the PR healthy and
//     waits for a human to merge.
//
// Best-effort; a no-op without storage or the GitHub gateway.
func (r *Runtime) reconcileWorkerShepherd(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	now := r.now
	logger := r.logger
	gateway := r.githubGateway
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil || repositories.Queue == nil || gateway == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	shepherding, err := repositories.Loops.ListByStatuses(ctx, []string{"shepherding"})
	if err != nil {
		return
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	for _, loop := range shepherding {
		if !strings.EqualFold(strings.TrimSpace(loop.Type), "worker") {
			continue
		}
		marker, ok := loops.ReadShepherd(loop.MetadataJSON)
		if !ok || !marker.Active {
			continue
		}
		repo := strings.TrimSpace(derefString(loop.Repo))
		if repo == "" || loop.PRNumber == nil || *loop.PRNumber <= 0 {
			continue
		}
		prNumber := *loop.PRNumber
		cwd := ""
		if proj, perr := repositories.Projects.GetByID(ctx, loop.ProjectID); perr == nil && proj != nil {
			cwd = proj.RepoPath
		}
		r.refreshShepherdLock(ctx, repositories, loop.ID, repo, prNumber, now)

		// ViewPullRequestForReviewer (not ViewPullRequest) so the detail carries
		// reviews + statusCheckRollup — the plain view uses metadata-only fields
		// and would leave Checks/Reviews empty, blinding CI and review-round detection.
		pr, err := gateway.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: prNumber, CWD: cwd})
		if err != nil {
			continue
		}
		if outcome := shepherdTerminalOutcome(pr); outcome != "" {
			r.terminateShepherd(ctx, repositories, loop, repo, prNumber, outcome, nowISO, logger)
			continue
		}
		signal := foldShepherdSignal(pr)
		if signal == marker.LastSignal {
			continue
		}
		marker.LastSignal = signal
		marker.Phase = shepherdPhaseFor(pr)
		marker.HeadSHA = pr.HeadSHA
		if newMeta, werr := loops.WriteShepherd(loop.MetadataJSON, marker); werr == nil {
			r.persistLoopMetadata(ctx, repositories, loop.ID, newMeta, nowISO)
		}
		if shepherdActionable(pr) {
			if marker.PassCount >= shepherdMaxPasses {
				if logger != nil {
					logger.Info("shepherd: pass cap reached — leaving PR for a human", map[string]any{"loopId": loop.ID, "repo": repo, "prNumber": prNumber, "passCount": marker.PassCount})
				}
				continue
			}
			r.enqueueShepherdPass(ctx, repositories, loop, repo, prNumber, nowISO, logger)
		}
	}
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// shepherdTerminalOutcome returns "merged"/"abandoned"/"" for a PR.
func shepherdTerminalOutcome(pr githubinfra.PullRequestDetail) string {
	state := strings.ToUpper(strings.TrimSpace(pr.State))
	if state == "MERGED" || strings.TrimSpace(pr.MergedAt) != "" {
		return "merged"
	}
	if state == "CLOSED" {
		return "abandoned"
	}
	return ""
}

// shepherdCIPhase folds statusCheckRollup into a coarse settled/pending phase so
// CI wakes the loop exactly once (when it settles), not once per check.
func shepherdCIPhase(pr githubinfra.PullRequestDetail) string {
	pending := false
	failed := false
	for _, c := range pr.Checks {
		concl := strings.ToUpper(strings.TrimSpace(stringFromAny(c["conclusion"])))
		status := strings.ToUpper(strings.TrimSpace(stringFromAny(c["status"])))
		if concl == "" && status != "COMPLETED" {
			pending = true
			continue
		}
		switch concl {
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
			failed = true
		}
	}
	switch {
	case pending:
		return "pending"
	case failed:
		return "failed"
	default:
		return "passed"
	}
}

// latestReviewMarker is the newest top-level review timestamp (across all
// reviewers). It changes when a reviewer submits a NEW review round — even when
// the aggregate reviewDecision stays CHANGES_REQUESTED and the head is unchanged
// — but NOT on the agent's own thread replies (those are review comments, not
// top-level reviews) or pushes, so it catches re-reviews without self-retrigger.
func latestReviewMarker(pr githubinfra.PullRequestDetail) string {
	last := ""
	for _, review := range pr.Reviews {
		at := stringFromAny(review["submittedAt"])
		if at == "" {
			at = stringFromAny(review["updatedAt"])
		}
		if at > last {
			last = at
		}
	}
	return last
}

// foldShepherdSignal is the wake key: stable semantic fields only. It deliberately
// omits pr.UpdatedAt (the agent's own comments bump it → self-retrigger forever)
// but includes latestReviewMarker so a new review round wakes a pass even at the
// same head with the same aggregate decision.
func foldShepherdSignal(pr githubinfra.PullRequestDetail) string {
	return fmt.Sprintf("%s|%s|%s|%v|%s|%s", strings.ToUpper(pr.State), strings.ToUpper(pr.ReviewDecision), shepherdCIPhase(pr), pr.HasConflicts, pr.HeadSHA, latestReviewMarker(pr))
}

// changesRequestedOnCurrentHead reports whether a reviewer has a CHANGES_REQUESTED
// review whose commit IS the current head. This — not the aggregate
// reviewDecision — is the actionable review signal: a CHANGES_REQUESTED sticks in
// reviewDecision until the reviewer approves, so after we push a fix the reviewer
// hasn't re-reviewed yet, reviewDecision stays CHANGES_REQUESTED but there is
// nothing new to do (we already responded and are WAITING for the re-review).
// Keying on "changes requested AT the current head" stops the wasteful spin of
// re-fixing a stale review round.
func changesRequestedOnCurrentHead(pr githubinfra.PullRequestDetail) bool {
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		return false
	}
	for _, review := range pr.Reviews {
		if !strings.EqualFold(strings.TrimSpace(stringFromAny(review["state"])), "CHANGES_REQUESTED") {
			continue
		}
		commit, _ := review["commit"].(map[string]any)
		if commit != nil && strings.TrimSpace(stringFromAny(commit["oid"])) == head {
			return true
		}
	}
	return false
}

// shepherdActionable reports whether the agent has something to do THIS wake:
// changes requested AT THE CURRENT HEAD, failing CI, or a conflict. Approved (or
// a fix already pushed and awaiting the reviewer's re-review) is NOT actionable —
// the bot waits; it never merges.
func shepherdActionable(pr githubinfra.PullRequestDetail) bool {
	if pr.HasConflicts {
		return true
	}
	if shepherdCIPhase(pr) == "failed" {
		return true
	}
	return changesRequestedOnCurrentHead(pr)
}

// shepherdPhaseFor maps PR state to the card phase.
func shepherdPhaseFor(pr githubinfra.PullRequestDetail) string {
	if shepherdActionable(pr) {
		return "fixing"
	}
	if strings.EqualFold(strings.TrimSpace(pr.ReviewDecision), "APPROVED") && shepherdCIPhase(pr) == "passed" && !pr.HasConflicts {
		return "awaiting_merge"
	}
	return "reviewing"
}

// refreshShepherdLock keeps the pr:<repo>:<n> lock alive under the stable
// shepherd:<loopID> owner so a same-account fixer's discovery keeps skipping.
func (r *Runtime) refreshShepherdLock(ctx context.Context, repos *storage.Repositories, loopID, repo string, prNumber int64, now func() time.Time) {
	if repos.Locks == nil {
		return
	}
	key := fmt.Sprintf("pr:%s:%d", repo, prNumber)
	owner := "shepherd:" + loopID
	reason := "shepherd"
	nowISO := formatJavaScriptISOString(now().UTC())
	expiresAt := formatJavaScriptISOString(now().UTC().Add(shepherdReconcileLockTTL))
	if refreshed, err := repos.Locks.Refresh(ctx, storage.LockRecord{Key: key, Owner: owner, Reason: &reason, ExpiresAt: expiresAt, UpdatedAt: nowISO}); err == nil && refreshed {
		return
	}
	_, _ = repos.Locks.Acquire(ctx, storage.LockRecord{Key: key, Owner: owner, Reason: &reason, ExpiresAt: expiresAt, CreatedAt: nowISO, UpdatedAt: nowISO})
}

// terminateShepherd finishes a shepherding loop once its PR merged or closed:
// clears the marker, releases the coordination lock, and sets loop completed.
func (r *Runtime) terminateShepherd(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, repo string, prNumber int64, outcome, nowISO string, logger interface {
	Info(string, map[string]any)
}) {
	marker, _ := loops.ReadShepherd(loop.MetadataJSON)
	marker.Active = false
	marker.Outcome = outcome
	marker.Phase = "merged"
	newMeta, err := loops.WriteShepherd(loop.MetadataJSON, marker)
	if err != nil {
		return
	}
	fresh, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	updated := *fresh
	updated.Status = string(domain.LoopStatusCompleted)
	updated.MetadataJSON = &newMeta
	updated.NextRunAt = nil
	updated.LastRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return
	}
	if repos.Locks != nil {
		_ = repos.Locks.Release(ctx, fmt.Sprintf("pr:%s:%d", repo, prNumber))
	}
	if logger != nil {
		logger.Info("shepherd: PR reached terminal state", map[string]any{"loopId": loop.ID, "repo": repo, "prNumber": prNumber, "outcome": outcome})
	}
}

// persistLoopMetadata writes updated $.shepherd metadata without changing status.
func (r *Runtime) persistLoopMetadata(ctx context.Context, repos *storage.Repositories, loopID, metadataJSON, nowISO string) {
	fresh, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || fresh == nil {
		return
	}
	updated := *fresh
	updated.MetadataJSON = &metadataJSON
	updated.UpdatedAt = nowISO
	_ = repos.Loops.Upsert(ctx, updated)
}

// enqueueShepherdPass wakes ONE shepherd pass: a worker queue item bound to the
// loop (deduped by loop id so an in-flight pass is not double-dispatched). The
// resolver forces stepShepherd via the durable marker. The LockKey is recorded
// but not acquired at claim (the shepherd pass manages the pr lock itself).
func (r *Runtime) enqueueShepherdPass(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, repo string, prNumber int64, nowISO string, logger interface {
	Info(string, map[string]any)
}) {
	lockKey := fmt.Sprintf("pr:%s:%d", repo, prNumber)
	projectID := loop.ProjectID
	record := storage.QueueItemRecord{
		ID:          eventlog.NewEventID("queue"),
		ProjectID:   &projectID,
		LoopID:      &loop.ID,
		Type:        "worker",
		TargetType:  string(domain.LoopTargetTypePullRequest),
		TargetID:    lockKey,
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   fmt.Sprintf("worker:%s", loop.ID),
		Priority:    storage.QueuePriorityWorker,
		Status:      "queued",
		AvailableAt: nowISO,
		Attempts:    0,
		MaxAttempts: 3,
		LockKey:     &lockKey,
		CreatedAt:   nowISO,
		UpdatedAt:   nowISO,
	}
	_, created, err := repos.Queue.CreateOrGetActiveByDedupe(ctx, record)
	if err != nil {
		if logger != nil {
			logger.Info("shepherd: enqueue pass failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
		}
		return
	}
	if created {
		r.TriggerSchedulerClaim()
		if logger != nil {
			logger.Info("shepherd: woke a pass", map[string]any{"loopId": loop.ID, "repo": repo, "prNumber": prNumber})
		}
	}
}
