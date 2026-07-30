package storage

import (
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/domain"
)

// TargetLeaseKeyFromLoop returns the durable checkout-ownership key for a
// loop. Pull requests and issues belong to their target across roles; project
// workers use a per-loop branch and checkout, so they remain independent.
func TargetLeaseKeyFromLoop(loop LoopRecord) string {
	if loop.Type == string(domain.LoopTypeWorker) && loop.TargetType == string(domain.LoopTargetTypeProject) {
		if strings.TrimSpace(loop.ProjectID) == "" || strings.TrimSpace(loop.ID) == "" {
			return ""
		}
		return fmt.Sprintf("%s|worker:%s", strings.TrimSpace(loop.ProjectID), strings.TrimSpace(loop.ID))
	}
	return targetLeaseKey(loop.ProjectID, loop.TargetType, loop.TargetID, loop.Repo, loop.PRNumber)
}

// TargetLeaseKeyFromQueue returns the same checkout-ownership key before a
// scheduler has materialized a runner. Queue LoopID supplies the independent
// project-worker identity.
func TargetLeaseKeyFromQueue(item QueueItemRecord) string {
	if item.Type == string(domain.LoopTypeWorker) && item.TargetType == string(domain.LoopTargetTypeProject) {
		if item.ProjectID == nil || item.LoopID == nil || strings.TrimSpace(*item.ProjectID) == "" || strings.TrimSpace(*item.LoopID) == "" {
			return ""
		}
		return fmt.Sprintf("%s|worker:%s", strings.TrimSpace(*item.ProjectID), strings.TrimSpace(*item.LoopID))
	}
	projectID := ""
	if item.ProjectID != nil {
		projectID = *item.ProjectID
	}
	return targetLeaseKey(projectID, item.TargetType, &item.TargetID, item.Repo, item.PRNumber)
}

func targetLeaseKey(projectID, targetType string, targetID, repo *string, prNumber *int64) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ""
	}
	if targetType == string(domain.LoopTargetTypePullRequest) {
		if repo == nil || prNumber == nil || strings.TrimSpace(*repo) == "" || *prNumber <= 0 {
			return ""
		}
		return fmt.Sprintf("%s|pull_request:%s:%d", projectID, strings.TrimSpace(*repo), *prNumber)
	}
	if targetType == string(domain.LoopTargetTypeIssue) && targetID != nil && strings.TrimSpace(*targetID) != "" {
		return fmt.Sprintf("%s|%s", projectID, strings.TrimSpace(*targetID))
	}
	return ""
}
