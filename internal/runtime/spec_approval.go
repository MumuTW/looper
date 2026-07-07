package runtime

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/storage"
)

// workerReadyLabel is the work-item label the worker discovers on (node I). node H
// stamps it once the tech spec is approved, handing off from planner to worker.
const workerReadyLabel = "looper:worker-ready"

// loopSpecApprovalState reads a completed planner loop's node H flags from metadata:
// whether it is awaiting spec approval, whether the worker was already dispatched, and
// the originating work-item URL.
func loopSpecApprovalState(metadataJSON *string) (awaiting, dispatched bool, issueURL string) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return false, false, ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return false, false, ""
	}
	awaiting, _ = meta["awaitingSpecApproval"].(bool)
	dispatched, _ = meta["specApprovedDispatched"].(bool)
	if s, ok := meta["issueUrl"].(string); ok {
		issueURL = strings.TrimSpace(s)
	} else if s, ok := meta["issueURL"].(string); ok {
		issueURL = strings.TrimSpace(s)
	}
	return awaiting, dispatched, issueURL
}

// reconcileSpecApproval drives flowchart node H → node I. For completed planner loops
// on Plane projects whose tech spec is awaiting review, it polls the spec page for a
// HUMAN approval (a non-[looper] approve comment) and — once approved — stamps
// looper:worker-ready on the work item so the worker picks it up (node I), drops an
// audit comment on the page, and marks the loop dispatched so it fires exactly once.
// Polling, since looper has no Plane webhook. Best-effort; a no-op without storage.
func (r *Runtime) reconcileSpecApproval(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	completed, err := repositories.Loops.ListByStatuses(ctx, []string{"completed"})
	if err != nil {
		return
	}
	nowISO := formatJavaScriptISOString(now().UTC())
	for _, loop := range completed {
		if !strings.EqualFold(strings.TrimSpace(loop.Type), "planner") {
			continue
		}
		awaiting, dispatched, issueURL := loopSpecApprovalState(loop.MetadataJSON)
		if !awaiting || dispatched {
			continue
		}
		gateway, planeProjectID, ok := planeDocForProject(&cfg, loop.ProjectID)
		if !ok || gateway == nil {
			continue
		}
		workItemID := planedoc.WorkItemIDFromURL(issueURL)
		if workItemID == "" {
			continue
		}
		specURL, found, err := gateway.FindSpecLink(ctx, planeProjectID, workItemID, planedoc.TechSpecLinkTitle)
		if err != nil || !found {
			continue
		}
		approved, by, err := gateway.SpecApprovedOnPage(ctx, planeProjectID, specURL)
		if err != nil || !approved {
			continue
		}
		// node H → I: approved. Stamp worker-ready so the worker discovers the item,
		// drop an audit comment on the spec page, and mark dispatched (exactly once).
		if err := gateway.AddWorkItemLabel(ctx, planeProjectID, workItemID, workerReadyLabel); err != nil {
			if logger != nil {
				logger.Warn("spec approval: stamp worker-ready failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		audit := planedoc.SignComment("<p>✅ 已由 "+html.EscapeString(strings.TrimSpace(by))+" 批准,进入实现(node I)。</p>", "reviewer", "")
		if err := gateway.CommentOnPageURL(ctx, planeProjectID, specURL, audit); err != nil && logger != nil {
			logger.Warn("spec approval: audit comment failed (continuing)", map[string]any{"loopId": loop.ID, "error": err.Error()})
		}
		if err := markSpecApprovalDispatched(ctx, repositories, loop.ID, nowISO); err != nil {
			if logger != nil {
				logger.Warn("spec approval: mark dispatched failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if logger != nil {
			logger.Info("spec approved — dispatched worker", map[string]any{"loopId": loop.ID, "workItem": workItemID, "approvedBy": by})
		}
	}
}

// markSpecApprovalDispatched flips the loop's specApprovedDispatched metadata flag so
// the reconcile hands off to the worker exactly once.
func markSpecApprovalDispatched(ctx context.Context, repos *storage.Repositories, loopID, nowISO string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta["specApprovedDispatched"] = true
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	updated := *loop
	s := string(encoded)
	updated.MetadataJSON = &s
	updated.UpdatedAt = nowISO
	return repos.Loops.Upsert(ctx, updated)
}
