package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/storage"
)

type blockedConditionReconciler func(context.Context, storage.LoopRecord, loopcondition.Record) (bool, error)

func (r *Runtime) reconcileBlockedConditions(ctx context.Context) {
	r.mu.RLock()
	repositories := r.services.Repositories
	cfg := r.config
	now := r.now
	logger := r.logger
	gateway := r.githubGateway
	r.mu.RUnlock()
	if repositories == nil || repositories.Loops == nil || repositories.Queue == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	registry := r.blockedConditionRegistry(&cfg, repositories, gateway)
	blocked, err := repositories.Loops.ListByStatuses(ctx, []string{"paused", "awaiting_human", "shepherding"})
	if err != nil {
		return
	}
	for _, loop := range blocked {
		record, inferred := effectiveBlockedCondition(loop)
		if !record.Valid() {
			continue
		}
		check := registry[record.Kind]
		if check == nil {
			continue
		}
		ready, checkErr := check(ctx, loop, record)
		if checkErr != nil {
			if logger != nil {
				logger.Warn("blocked condition reconcile failed", map[string]any{"loopId": loop.ID, "condition": record.Kind, "error": checkErr.Error()})
			}
			continue
		}
		if !ready {
			if inferred {
				r.persistInferredBlockedCondition(ctx, repositories, loop, record, now)
			}
			continue
		}
		if err := resumeBlockedLoop(ctx, repositories, loop, record, now); err != nil {
			if logger != nil {
				logger.Warn("blocked condition cleared but loop requeue failed", map[string]any{"loopId": loop.ID, "condition": record.Kind, "error": err.Error()})
			}
			continue
		}
		if logger != nil {
			logger.Info("resumed loop after blocked condition cleared", map[string]any{"loopId": loop.ID, "condition": record.Kind})
		}
	}
}

func (r *Runtime) blockedConditionRegistry(cfg *config.Config, repositories *storage.Repositories, gateway *githubinfra.Gateway) map[loopcondition.Kind]blockedConditionReconciler {
	return map[loopcondition.Kind]blockedConditionReconciler{
		loopcondition.ProductSpec: func(ctx context.Context, loop storage.LoopRecord, _ loopcondition.Record) (bool, error) {
			planeGateway, planeProjectID, ok := planeDocForProject(cfg, loop.ProjectID)
			if !ok || planeGateway == nil {
				return false, nil
			}
			issueURL, hasSpecPR := loopIssueURLAndSpecPR(loop.MetadataJSON)
			workItemID := planedoc.WorkItemIDFromURL(issueURL)
			if workItemID == "" {
				return false, nil
			}
			present, _, err := planeGateway.HasProductSpec(ctx, planeProjectID, workItemID)
			return shouldResumeForProductSpec(loop.Type, loop.Status, hasSpecPR, present), err
		},
		loopcondition.DiskRecovered: func(_ context.Context, _ storage.LoopRecord, _ loopcondition.Record) (bool, error) {
			return diskConditionCleared(cfg)
		},
		loopcondition.CISettled: func(ctx context.Context, loop storage.LoopRecord, _ loopcondition.Record) (bool, error) {
			pr, err := pullRequestForBlockedCondition(ctx, repositories, gateway, loop)
			return err == nil && shepherdCIPhase(pr) != "pending", err
		},
		loopcondition.ReviewUpdated: func(ctx context.Context, loop storage.LoopRecord, condition loopcondition.Record) (bool, error) {
			pr, err := pullRequestForBlockedCondition(ctx, repositories, gateway, loop)
			if err != nil {
				return false, err
			}
			return condition.Fingerprint != "" && foldShepherdSignal(pr) != condition.Fingerprint, nil
		},
		loopcondition.HumanAnswered: func(_ context.Context, loop storage.LoopRecord, _ loopcondition.Record) (bool, error) {
			ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
			return ok && strings.EqualFold(strings.TrimSpace(ask.Status), "answered") && strings.TrimSpace(ask.Answer) != "", nil
		},
		loopcondition.InfraRecovered: func(ctx context.Context, loop storage.LoopRecord, condition loopcondition.Record) (bool, error) {
			projectID := loop.ProjectID
			return recoverableInfraConditionCleared(ctx, cfg, repositories, &projectID, condition.Fingerprint)
		},
	}
}

func effectiveBlockedCondition(loop storage.LoopRecord) (loopcondition.Record, bool) {
	if record, ok := loopcondition.Read(loop.MetadataJSON); ok {
		return record, false
	}
	if loop.Status == "paused" && loop.Type == "planner" && metadataBool(loop.MetadataJSON, "awaitingProductSpec") {
		return loopcondition.Record{Kind: loopcondition.ProductSpec}, true
	}
	if loop.Status == "awaiting_human" {
		if _, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok {
			return loopcondition.Record{Kind: loopcondition.HumanAnswered}, true
		}
	}
	return loopcondition.Record{}, false
}

func metadataBool(metadataJSON *string, key string) bool {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return false
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(*metadataJSON), &metadata) != nil {
		return false
	}
	value, _ := metadata[key].(bool)
	return value
}

func (r *Runtime) persistInferredBlockedCondition(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, record loopcondition.Record, now func() time.Time) {
	record.Since = formatJavaScriptISOString(now().UTC())
	metadata, err := loopcondition.Set(loop.MetadataJSON, record)
	if err != nil {
		return
	}
	loop.MetadataJSON = &metadata
	loop.UpdatedAt = record.Since
	_ = repositories.Loops.Upsert(ctx, loop)
}

func resumeBlockedLoop(ctx context.Context, repositories *storage.Repositories, loop storage.LoopRecord, record loopcondition.Record, now func() time.Time) error {
	nowTime := now().UTC()
	nowISO := formatJavaScriptISOString(nowTime)
	// Give the loop-state write a short lead over the queue claim so the scheduler
	// cannot observe a still-parked loop and discard its freshly recovered item.
	availableAt := formatJavaScriptISOString(nowTime.Add(time.Second))
	if err := ensureRecoveryQueueItem(ctx, repositories, loop, availableAt, -1); err != nil {
		return err
	}
	current, err := repositories.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("blocked loop disappeared: %s", loop.ID)
	}
	switch current.Status {
	case "paused", "awaiting_human", "shepherding":
	default:
		return nil
	}
	metadata, err := loopcondition.Clear(current.MetadataJSON)
	if err != nil {
		return err
	}
	current.MetadataJSON = &metadata
	current.Status = "queued"
	current.NextRunAt = &availableAt
	current.UpdatedAt = nowISO
	if err := repositories.Loops.Upsert(ctx, *current); err != nil {
		return err
	}
	payload := mustMarshalJSON(map[string]any{"condition": record.Kind, "clearedAt": nowISO})
	return appendSystemEvent(ctx, repositories, storage.EventLogRecord{
		ID:          newRuntimeEventID(),
		EventType:   "loop.condition.cleared",
		ProjectID:   &current.ProjectID,
		LoopID:      &current.ID,
		EntityType:  stringPtr("loop"),
		EntityID:    &current.ID,
		PayloadJSON: payload,
		CreatedAt:   nowISO,
	})
}

func diskConditionCleared(cfg *config.Config) (bool, error) {
	if cfg == nil || !cfg.Daemon.DiskBackpressure.Enabled {
		return true, nil
	}
	path := strings.TrimSpace(cfg.Daemon.DiskBackpressure.Path)
	if path == "" {
		var err error
		path, err = config.DefaultWorktreeRoot()
		if err != nil {
			return false, err
		}
	}
	usage, err := diskUsageStat(path)
	if err != nil {
		return false, err
	}
	high := cfg.Daemon.DiskBackpressure.HighWatermarkPercent
	return high <= 0 || usage.UsedPercent < high, nil
}

func pullRequestForBlockedCondition(ctx context.Context, repositories *storage.Repositories, gateway *githubinfra.Gateway, loop storage.LoopRecord) (githubinfra.PullRequestDetail, error) {
	if gateway == nil || loop.Repo == nil || loop.PRNumber == nil || *loop.PRNumber <= 0 {
		return githubinfra.PullRequestDetail{}, fmt.Errorf("condition %s has no GitHub pull request", loop.ID)
	}
	cwd := ""
	if repositories != nil && repositories.Projects != nil {
		project, err := repositories.Projects.GetByID(ctx, loop.ProjectID)
		if err != nil {
			return githubinfra.PullRequestDetail{}, err
		}
		if project != nil {
			cwd = project.RepoPath
		}
	}
	return gateway.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: *loop.Repo, PRNumber: *loop.PRNumber, CWD: cwd})
}
