package worktreecleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
)

type Service struct {
	Repos  *storage.Repositories
	Config config.WorktreeCleanupConfig
	Now    func() time.Time
}

type Plan struct {
	DryRun  bool          `json:"dryRun"`
	Summary PlanSummary   `json:"summary"`
	Items   []PlanItem    `json:"items"`
	Failed  []PlanFailure `json:"failed,omitempty"`
}

type PlanSummary struct {
	Scanned    int `json:"scanned"`
	Candidates int `json:"candidates"`
	WouldClean int `json:"wouldClean"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
}

type PlanItem struct {
	Worktree          storage.WorktreeRecord `json:"worktree"`
	Decision          string                 `json:"decision"`
	Reason            string                 `json:"reason,omitempty"`
	EffectiveLastUsed string                 `json:"effectiveLastUsed,omitempty"`
	ReferencedLoopIDs []string               `json:"referencedLoopIds,omitempty"`
	ReferencedRunIDs  []string               `json:"referencedRunIds,omitempty"`
	Orphan            bool                   `json:"orphan"`
}

type PlanFailure struct {
	RunID     string `json:"runId"`
	LoopID    string `json:"loopId"`
	ProjectID string `json:"projectId"`
	Message   string `json:"message"`
}

const (
	DecisionWouldClean = "would_clean"
	DecisionSkipped    = "skipped"
)

type checkpointEnvelope struct {
	Worktree *checkpointWorktree `json:"worktree,omitempty"`
}

type checkpointWorktree struct {
	ID     string `json:"id,omitempty"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
}

type worktreeRef struct {
	loop storage.LoopRecord
	run  storage.RunRecord
}

type planCandidate struct {
	item         PlanItem
	lastUsedTime time.Time
}

func (s *Service) Plan(ctx context.Context) (Plan, error) {
	if s.Repos == nil || s.Repos.Projects == nil || s.Repos.Worktrees == nil || s.Repos.Loops == nil || s.Repos.Runs == nil || s.Repos.Queue == nil {
		return Plan{}, fmt.Errorf("worktree cleanup service is not configured")
	}

	cfg := s.Config
	retentionCutoff := s.now().Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)

	worktrees, err := s.listActiveWorktrees(ctx)
	if err != nil {
		return Plan{}, err
	}
	loops, err := s.Repos.Loops.List(ctx)
	if err != nil {
		return Plan{}, err
	}
	runs, err := s.Repos.Runs.List(ctx)
	if err != nil {
		return Plan{}, err
	}
	queueItems, err := s.Repos.Queue.List(ctx)
	if err != nil {
		return Plan{}, err
	}

	loopsByID := map[string]storage.LoopRecord{}
	for _, loop := range loops {
		loopsByID[loop.ID] = loop
	}
	refsByWorktreeKey, failures := s.indexCheckpointReferences(runs, loopsByID)
	activeQueueByLoopID := indexActiveQueueByLoopID(queueItems)

	plan := Plan{DryRun: cfg.DryRun, Failed: failures}
	plan.Summary.Scanned = len(worktrees)
	plan.Summary.Failed = len(failures)

	candidates := make([]planCandidate, 0, len(worktrees))
	for _, worktree := range worktrees {
		item := PlanItem{Worktree: worktree}
		refs := matchingRefs(worktree, refsByWorktreeKey)
		item.ReferencedLoopIDs, item.ReferencedRunIDs = refIDs(refs)
		item.Orphan = len(refs) == 0
		lastUsed := effectiveLastUsed(worktree, refs, activeQueueByLoopID)
		item.EffectiveLastUsed = formatOptionalTime(lastUsed)

		if len(failures) > 0 && projectHasParseFailure(worktree.ProjectID, failures) {
			plan.Summary.Skipped++
			item.Decision = DecisionSkipped
			item.Reason = "checkpoint_parse_failed"
			plan.Items = append(plan.Items, item)
			continue
		}
		if item.Orphan && !cfg.IncludeOrphans {
			plan.Summary.Skipped++
			item.Decision = DecisionSkipped
			item.Reason = "orphan"
			plan.Items = append(plan.Items, item)
			continue
		}
		if reason := protectionReason(refs, activeQueueByLoopID); reason != "" {
			plan.Summary.Skipped++
			item.Decision = DecisionSkipped
			item.Reason = reason
			plan.Items = append(plan.Items, item)
			continue
		}
		if lastUsed.After(retentionCutoff) {
			plan.Summary.Skipped++
			item.Decision = DecisionSkipped
			item.Reason = "retention"
			plan.Items = append(plan.Items, item)
			continue
		}

		candidates = append(candidates, planCandidate{item: item, lastUsedTime: lastUsed})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].lastUsedTime.Equal(candidates[j].lastUsedTime) {
			return candidates[i].lastUsedTime.Before(candidates[j].lastUsedTime)
		}
		return candidates[i].item.Worktree.ID < candidates[j].item.Worktree.ID
	})

	plan.Summary.Candidates = len(candidates)
	for index, candidate := range candidates {
		item := candidate.item
		if index >= cfg.MaxPerTick {
			plan.Summary.Skipped++
			item.Decision = DecisionSkipped
			item.Reason = "max_per_tick"
		} else {
			plan.Summary.WouldClean++
			item.Decision = DecisionWouldClean
		}
		plan.Items = append(plan.Items, item)
	}

	return plan, nil
}

func (s *Service) listActiveWorktrees(ctx context.Context) ([]storage.WorktreeRecord, error) {
	projects, err := s.Repos.Projects.List(ctx)
	if err != nil {
		return nil, err
	}
	worktrees := make([]storage.WorktreeRecord, 0)
	for _, project := range projects {
		records, err := s.Repos.Worktrees.ListByProject(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if record.CleanedAt == nil && record.Status == "active" {
				worktrees = append(worktrees, record)
			}
		}
	}
	return worktrees, nil
}

func (s *Service) indexCheckpointReferences(runs []storage.RunRecord, loopsByID map[string]storage.LoopRecord) (map[string][]worktreeRef, []PlanFailure) {
	refsByWorktreeKey := map[string][]worktreeRef{}
	failures := make([]PlanFailure, 0)
	for _, run := range runs {
		if run.CheckpointJSON == nil || strings.TrimSpace(*run.CheckpointJSON) == "" {
			continue
		}
		loop, ok := loopsByID[run.LoopID]
		if !ok {
			continue
		}
		var checkpoint checkpointEnvelope
		if err := json.Unmarshal([]byte(*run.CheckpointJSON), &checkpoint); err != nil {
			failures = append(failures, PlanFailure{
				RunID:     run.ID,
				LoopID:    run.LoopID,
				ProjectID: loop.ProjectID,
				Message:   err.Error(),
			})
			continue
		}
		if checkpoint.Worktree == nil {
			continue
		}
		ref := worktreeRef{loop: loop, run: run}
		for _, key := range worktreeKeys(loop.ProjectID, checkpoint.Worktree.ID, checkpoint.Worktree.Path, checkpoint.Worktree.Branch) {
			refsByWorktreeKey[key] = append(refsByWorktreeKey[key], ref)
		}
	}
	return refsByWorktreeKey, failures
}

func matchingRefs(worktree storage.WorktreeRecord, refsByWorktreeKey map[string][]worktreeRef) []worktreeRef {
	seen := map[string]struct{}{}
	refs := make([]worktreeRef, 0)
	for _, key := range worktreeKeys(worktree.ProjectID, worktree.ID, worktree.WorktreePath, worktree.Branch) {
		for _, ref := range refsByWorktreeKey[key] {
			identity := ref.loop.ID + "\x00" + ref.run.ID
			if _, ok := seen[identity]; ok {
				continue
			}
			seen[identity] = struct{}{}
			refs = append(refs, ref)
		}
	}
	return refs
}

func worktreeKeys(projectID, id, path, branch string) []string {
	keys := make([]string, 0, 3)
	if strings.TrimSpace(id) != "" {
		keys = append(keys, "id:"+projectID+":"+strings.TrimSpace(id))
	}
	if strings.TrimSpace(path) != "" {
		keys = append(keys, "path:"+projectID+":"+strings.TrimSpace(path))
	}
	if strings.TrimSpace(branch) != "" {
		keys = append(keys, "branch:"+projectID+":"+strings.TrimSpace(branch))
	}
	return keys
}

func indexActiveQueueByLoopID(queueItems []storage.QueueItemRecord) map[string]storage.QueueItemRecord {
	active := map[string]storage.QueueItemRecord{}
	for _, item := range queueItems {
		if item.LoopID == nil || (item.Status != "queued" && item.Status != "running") {
			continue
		}
		existing, ok := active[*item.LoopID]
		if !ok || item.UpdatedAt > existing.UpdatedAt {
			active[*item.LoopID] = item
		}
	}
	return active
}

func refIDs(refs []worktreeRef) ([]string, []string) {
	loopSeen := map[string]struct{}{}
	runSeen := map[string]struct{}{}
	loopIDs := make([]string, 0)
	runIDs := make([]string, 0)
	for _, ref := range refs {
		if _, ok := loopSeen[ref.loop.ID]; !ok {
			loopSeen[ref.loop.ID] = struct{}{}
			loopIDs = append(loopIDs, ref.loop.ID)
		}
		if _, ok := runSeen[ref.run.ID]; !ok {
			runSeen[ref.run.ID] = struct{}{}
			runIDs = append(runIDs, ref.run.ID)
		}
	}
	sort.Strings(loopIDs)
	sort.Strings(runIDs)
	return loopIDs, runIDs
}

func effectiveLastUsed(worktree storage.WorktreeRecord, refs []worktreeRef, activeQueueByLoopID map[string]storage.QueueItemRecord) time.Time {
	lastUsed := parseStorageTime(worktree.UpdatedAt)
	for _, ref := range refs {
		lastUsed = maxTime(lastUsed, parseStorageTime(ref.loop.UpdatedAt))
		if ref.loop.LastRunAt != nil {
			lastUsed = maxTime(lastUsed, parseStorageTime(*ref.loop.LastRunAt))
		}
		lastUsed = maxTime(lastUsed, parseStorageTime(ref.run.UpdatedAt))
		if ref.run.LastHeartbeatAt != nil {
			lastUsed = maxTime(lastUsed, parseStorageTime(*ref.run.LastHeartbeatAt))
		}
		if ref.run.EndedAt != nil {
			lastUsed = maxTime(lastUsed, parseStorageTime(*ref.run.EndedAt))
		}
		if queueItem, ok := activeQueueByLoopID[ref.loop.ID]; ok {
			lastUsed = maxTime(lastUsed, parseStorageTime(queueItem.UpdatedAt))
		}
	}
	return lastUsed
}

func protectionReason(refs []worktreeRef, activeQueueByLoopID map[string]storage.QueueItemRecord) string {
	for _, ref := range refs {
		if _, ok := activeQueueByLoopID[ref.loop.ID]; ok {
			return "active_queue_item"
		}
		if ref.run.Status == string(domain.RunStatusRunning) {
			return "running_run"
		}
		switch domain.LoopStatus(ref.loop.Status) {
		case domain.LoopStatusQueued, domain.LoopStatusRunning, domain.LoopStatusWaiting, domain.LoopStatusPaused, domain.LoopStatusFailed, domain.LoopStatusInterrupted:
			return "protected_loop_status:" + ref.loop.Status
		}
	}
	return ""
}

func projectHasParseFailure(projectID string, failures []PlanFailure) bool {
	for _, failure := range failures {
		if failure.ProjectID == projectID {
			return true
		}
	}
	return false
}

func parseStorageTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
