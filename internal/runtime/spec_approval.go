package runtime

import (
	"context"
	"encoding/json"
	"html"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/notify"
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
		if err != nil {
			continue
		}
		if !approved {
			// goal #5: if a human hasn't approved for a while, nudge them in the thread.
			r.maybeNudgeSpecApproval(ctx, repositories, cfg, loop, specURL, now)
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

// specApprovalNudgeThreshold is how long a spec may sit awaiting a human's approval
// before the reconcile nudges them. Default 15m; override via LOOPER_SPEC_NUDGE_MINUTES
// (e.g. for tests).
func specApprovalNudgeThreshold() time.Duration {
	if v := strings.TrimSpace(os.Getenv("LOOPER_SPEC_NUDGE_MINUTES")); v != "" {
		if m, err := strconv.Atoi(v); err == nil && m >= 0 {
			return time.Duration(m) * time.Minute
		}
	}
	return 15 * time.Minute
}

// loopSpecApprovalNudgeState reads when the loop entered the human-approve wait and
// whether it has already been nudged.
func loopSpecApprovalNudgeState(metadataJSON *string) (since string, nudged bool) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return "", false
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return "", false
	}
	since, _ = meta["awaitingSpecApprovalSince"].(string)
	nudged, _ = meta["awaitingSpecNudged"].(bool)
	return strings.TrimSpace(since), nudged
}

// maybeNudgeSpecApproval posts a ONE-TIME follow-up nudge into the loop's Feishu thread,
// @-mentioning the product owner, when a tech spec has awaited a human's approval longer
// than the nudge threshold (goal #5). Idempotent via the awaitingSpecNudged flag.
func (r *Runtime) maybeNudgeSpecApproval(ctx context.Context, repositories *storage.Repositories, cfg config.Config, loop storage.LoopRecord, specURL string, now func() time.Time) {
	since, nudged := loopSpecApprovalNudgeState(loop.MetadataJSON)
	if nudged || since == "" {
		return
	}
	sinceT, err := time.Parse(time.RFC3339, since)
	if err != nil {
		if sinceT, err = time.Parse("2006-01-02T15:04:05.000Z07:00", since); err != nil {
			return
		}
	}
	if now().UTC().Sub(sinceT.UTC()) < specApprovalNudgeThreshold() {
		return // not been waiting long enough yet
	}
	gw := notify.NewGateway(notify.Options{Config: cfg.Notifications, Repositories: repositories, Now: now})
	var mentions []string
	if owner := strings.TrimSpace(config.ProjectProductOwner(cfg, loop.ProjectID).FeishuOpenID); owner != "" {
		mentions = []string{owner}
	}
	text := "🙋 这个方案已过 grill + review,在等你 approve —— 看没问题在方案页评论 approve / 同意 / 👍 即进入实现:" + specURL
	if err := gw.PostThreadNote(ctx, loop.ID, text, mentions); err != nil {
		return
	}
	_ = markSpecApprovalNudged(ctx, repositories, loop.ID, formatJavaScriptISOString(now().UTC()))
}

// markSpecApprovalNudged flips the loop's awaitingSpecNudged flag so the follow-up is
// sent exactly once.
func markSpecApprovalNudged(ctx context.Context, repos *storage.Repositories, loopID, nowISO string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	meta := map[string]any{}
	if loop.MetadataJSON != nil && strings.TrimSpace(*loop.MetadataJSON) != "" {
		_ = json.Unmarshal([]byte(*loop.MetadataJSON), &meta)
	}
	meta["awaitingSpecNudged"] = true
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
