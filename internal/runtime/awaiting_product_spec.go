package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/planedoc"
)

// shouldResumeForProductSpec decides whether a paused planner loop that was waiting
// for a product spec (flowchart node E2) should now be resumed: it is a planner loop,
// currently paused, has NOT yet opened a spec PR (so it's still at the product-spec
// gate, not paused for a later reason), and its work item now has a product spec.
func shouldResumeForProductSpec(loopType, status string, hasSpecPR, hasProductSpec bool) bool {
	return strings.EqualFold(strings.TrimSpace(loopType), "planner") &&
		strings.EqualFold(strings.TrimSpace(status), "paused") &&
		!hasSpecPR &&
		hasProductSpec
}

// loopIssueURLAndSpecPR reads a planner loop's originating issue URL + whether it has
// already opened a spec PR, from the loop metadata.
func loopIssueURLAndSpecPR(metadataJSON *string) (issueURL string, hasSpecPR bool) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return "", false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return "", false
	}
	if s, ok := meta["issueUrl"].(string); ok {
		issueURL = strings.TrimSpace(s)
	} else if s, ok := meta["issueURL"].(string); ok {
		issueURL = strings.TrimSpace(s)
	}
	// Worker loops carry only prUrl at the top level and nest the originating
	// work-item URL under worker.issueUrl. Planner loops record it top-level (above).
	// Without this fallback, setPlaneWorkItemState reads "" for every worker loop →
	// WorkItemIDFromURL("") == "" → the Plane state never advances to In Review / Done
	// for a worker-opened PR (the item stays Backlog/Todo forever).
	if issueURL == "" {
		if w, ok := meta["worker"].(map[string]any); ok {
			if s, ok := w["issueUrl"].(string); ok {
				issueURL = strings.TrimSpace(s)
			} else if s, ok := w["issueURL"].(string); ok {
				issueURL = strings.TrimSpace(s)
			}
		}
	}
	if s, ok := meta["prUrl"].(string); ok && strings.TrimSpace(s) != "" {
		hasSpecPR = true
	}
	return issueURL, hasSpecPR
}

// reconcileAwaitingProductSpec resumes planner loops that were held at the product-spec
// gate (node D/E) once their work item's product spec appears in Plane (node E2). It
// polls — looper has no Plane webhook — over paused planner loops on Plane projects,
// re-checks the spec, and re-queues the loop so the planner re-runs (this time past
// the gate). Best-effort; a no-op when storage isn't configured.
func (r *Runtime) reconcileAwaitingProductSpec(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil || repositories.Queue == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	paused, err := repositories.Loops.ListByStatuses(ctx, []string{"paused"})
	if err != nil {
		return
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	for _, loop := range paused {
		if !strings.EqualFold(strings.TrimSpace(loop.Type), "planner") {
			continue
		}
		gateway, planeProjectID, ok := planeDocForProject(&cfg, loop.ProjectID)
		if !ok || gateway == nil {
			continue
		}
		issueURL, hasSpecPR := loopIssueURLAndSpecPR(loop.MetadataJSON)
		workItemID := planedoc.WorkItemIDFromURL(issueURL)
		if workItemID == "" {
			continue
		}
		present, _, err := gateway.HasProductSpec(ctx, planeProjectID, workItemID)
		if err != nil {
			continue
		}
		if !shouldResumeForProductSpec(loop.Type, loop.Status, hasSpecPR, present) {
			continue
		}
		if err := ensureRecoveryQueueItem(ctx, repositories, loop, nowISO, int64(cfg.Scheduler.RetryMaxAttempts)); err != nil {
			if logger != nil {
				logger.Warn("reconcile awaiting product spec: requeue failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if logger != nil {
			logger.Info("resumed planner loop — product spec supplied", map[string]any{"loopId": loop.ID, "workItem": workItemID})
		}
	}
}
