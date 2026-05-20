package worktreecleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

type Service struct {
	Repos *storage.Repositories
	Now   func() time.Time
}

type PlanOptions struct {
	Config config.WorktreeCleanupConfig
}

type Plan struct {
	Summary PlanSummary
	Items   []PlanItem
}

type PlanSummary struct {
	Scanned    int
	Candidates int
	WouldClean int
	Skipped    int
	Failed     int
}

type PlanItem struct {
	WorktreeID       string
	ProjectID        string
	RepoPath         string
	WorktreePath     string
	Branch           string
	Action           PlanAction
	Reason           string
	LastUsedAt       *time.Time
	ReferencedLoopID *string
	ReferencedRunID  *string
}

type PlanAction string

const (
	PlanActionWouldClean PlanAction = "would_clean"
	PlanActionSkipped    PlanAction = "skipped"
	PlanActionFailed     PlanAction = "failed"
)

type worktreeUse struct {
	loopIDs        map[string]struct{}
	runIDs         map[string]struct{}
	protected      bool
	protection     string
	lastUsedAt     time.Time
	hasLastUsedAt  bool
	checkpointRefs int
}

type checkpointWorktree struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

type checkpointEnvelope struct {
	Worktree *checkpointWorktree `json:"worktree"`
}

func (s Service) Plan(ctx context.Context, options PlanOptions) (Plan, error) {
	if s.Repos == nil || s.Repos.Worktrees == nil || s.Repos.Loops == nil || s.Repos.Runs == nil || s.Repos.Queue == nil {
		return Plan{}, fmt.Errorf("worktree cleanup requires worktree, loop, run, and queue repositories")
	}

	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	retentionCutoff := now.AddDate(0, 0, -options.Config.RetentionDays)

	worktrees, err := s.Repos.Worktrees.ListActive(ctx)
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

	worktreeByID := make(map[string]storage.WorktreeRecord, len(worktrees))
	worktreeByPath := make(map[string]storage.WorktreeRecord, len(worktrees))
	for _, worktree := range worktrees {
		worktreeByID[worktree.ID] = worktree
		worktreeByPath[normalizePathKey(worktree.WorktreePath)] = worktree
	}

	loopByID := make(map[string]storage.LoopRecord, len(loops))
	projectHasParseFailure := map[string]struct{}{}
	for _, loop := range loops {
		loopByID[loop.ID] = loop
	}

	uses := make(map[string]*worktreeUse, len(worktrees))
	for _, worktree := range worktrees {
		uses[worktree.ID] = &worktreeUse{
			loopIDs: map[string]struct{}{},
			runIDs:  map[string]struct{}{},
		}
		if at, ok := parseBestTime(worktree.UpdatedAt, worktree.CreatedAt); ok {
			recordUse(uses[worktree.ID], at)
		}
	}

	for _, run := range runs {
		loop, hasLoop := loopByID[run.LoopID]
		if run.CheckpointJSON != nil && strings.TrimSpace(*run.CheckpointJSON) != "" {
			ref, parseErr := parseCheckpointWorktree(*run.CheckpointJSON)
			if parseErr != nil {
				if hasLoop {
					projectHasParseFailure[loop.ProjectID] = struct{}{}
				}
			} else if ref != nil {
				if worktree, ok := resolveCheckpointWorktree(*ref, worktreeByID, worktreeByPath); ok {
					use := uses[worktree.ID]
					use.checkpointRefs++
					use.loopIDs[run.LoopID] = struct{}{}
					use.runIDs[run.ID] = struct{}{}
					if hasLoop {
						if isProtectedLoopStatus(loop.Status) {
							use.protected = true
							use.protection = "referenced by " + loop.Status + " loop"
						}
						if at, ok := parseBestTime(loop.UpdatedAt, stringPtrValue(loop.LastRunAt), loop.CreatedAt); ok {
							recordUse(use, at)
						}
					}
					if isRunningRunStatus(run.Status) {
						use.protected = true
						use.protection = "referenced by running run"
					}
					if at, ok := parseBestTime(stringPtrValue(run.EndedAt), run.UpdatedAt, stringPtrValue(run.LastHeartbeatAt), run.StartedAt, run.CreatedAt); ok {
						recordUse(use, at)
					}
				}
			}
		}
	}

	for _, queueItem := range queueItems {
		if queueItem.LoopID == nil || !isActiveQueueStatus(queueItem.Status) {
			continue
		}
		for worktreeID, use := range uses {
			if _, ok := use.loopIDs[*queueItem.LoopID]; !ok {
				continue
			}
			use.protected = true
			use.protection = "referenced by active queue item"
			if at, ok := parseBestTime(queueItem.UpdatedAt, stringPtrValue(queueItem.StartedAt), queueItem.AvailableAt, queueItem.CreatedAt); ok {
				recordUse(uses[worktreeID], at)
			}
		}
	}

	plan := Plan{Summary: PlanSummary{Scanned: len(worktrees)}, Items: make([]PlanItem, 0, len(worktrees))}
	for _, worktree := range worktrees {
		use := uses[worktree.ID]
		item := PlanItem{
			WorktreeID:   worktree.ID,
			ProjectID:    worktree.ProjectID,
			RepoPath:     worktree.RepoPath,
			WorktreePath: worktree.WorktreePath,
			Branch:       worktree.Branch,
		}
		if use.hasLastUsedAt {
			lastUsedAt := use.lastUsedAt
			item.LastUsedAt = &lastUsedAt
		}
		item.ReferencedLoopID = firstMapKey(use.loopIDs)
		item.ReferencedRunID = firstMapKey(use.runIDs)

		switch {
		case hasProjectParseFailure(worktree.ProjectID, projectHasParseFailure):
			item.Action = PlanActionSkipped
			item.Reason = "checkpoint parse failure in project"
		case use.protected:
			item.Action = PlanActionSkipped
			item.Reason = use.protection
		case use.checkpointRefs == 0 && !options.Config.IncludeOrphans:
			item.Action = PlanActionSkipped
			item.Reason = "orphan worktree"
		case use.hasLastUsedAt && use.lastUsedAt.After(retentionCutoff):
			item.Action = PlanActionSkipped
			item.Reason = "within retention window"
		default:
			item.Action = PlanActionWouldClean
			item.Reason = "eligible"
		}

		if item.Action == PlanActionWouldClean {
			plan.Summary.Candidates++
		}
		plan.Items = append(plan.Items, item)
	}

	sort.SliceStable(plan.Items, func(i, j int) bool {
		left, right := plan.Items[i], plan.Items[j]
		if left.Action != right.Action {
			return left.Action == PlanActionWouldClean
		}
		if left.LastUsedAt == nil || right.LastUsedAt == nil {
			return left.WorktreeID < right.WorktreeID
		}
		if !left.LastUsedAt.Equal(*right.LastUsedAt) {
			return left.LastUsedAt.Before(*right.LastUsedAt)
		}
		return left.WorktreeID < right.WorktreeID
	})

	if options.Config.MaxPerTick > 0 {
		selected := 0
		for index := range plan.Items {
			if plan.Items[index].Action != PlanActionWouldClean {
				continue
			}
			selected++
			if selected > options.Config.MaxPerTick {
				plan.Items[index].Action = PlanActionSkipped
				plan.Items[index].Reason = "max per tick reached"
			}
		}
	}

	for _, item := range plan.Items {
		switch item.Action {
		case PlanActionWouldClean:
			plan.Summary.WouldClean++
		case PlanActionSkipped:
			plan.Summary.Skipped++
		case PlanActionFailed:
			plan.Summary.Failed++
		}
	}

	return plan, nil
}

func parseCheckpointWorktree(raw string) (*checkpointWorktree, error) {
	var checkpoint checkpointEnvelope
	if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
		return nil, err
	}
	return checkpoint.Worktree, nil
}

func resolveCheckpointWorktree(ref checkpointWorktree, byID map[string]storage.WorktreeRecord, byPath map[string]storage.WorktreeRecord) (storage.WorktreeRecord, bool) {
	if ref.ID != "" {
		if worktree, ok := byID[ref.ID]; ok {
			return worktree, true
		}
	}
	if ref.Path != "" {
		if worktree, ok := byPath[normalizePathKey(ref.Path)]; ok {
			return worktree, true
		}
	}
	return storage.WorktreeRecord{}, false
}

func isProtectedLoopStatus(status string) bool {
	switch status {
	case "active", "queued", "running", "waiting", "paused", "failed", "interrupted":
		return true
	default:
		return false
	}
}

func isRunningRunStatus(status string) bool {
	return status == "running"
}

func isActiveQueueStatus(status string) bool {
	return status == "queued" || status == "running"
}

func recordUse(use *worktreeUse, at time.Time) {
	if !use.hasLastUsedAt || at.After(use.lastUsedAt) {
		use.lastUsedAt = at
		use.hasLastUsedAt = true
	}
}

func parseBestTime(values ...string) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			parsed = parsed.UTC()
			if !found || parsed.After(latest) {
				latest = parsed
				found = true
			}
		}
	}
	return latest, found
}

func normalizePathKey(path string) string {
	return filepath.Clean(strings.TrimSpace(path))
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func firstMapKey(values map[string]struct{}) *string {
	for value := range values {
		copied := value
		return &copied
	}
	return nil
}

func hasProjectParseFailure(projectID string, failures map[string]struct{}) bool {
	_, ok := failures[projectID]
	return ok
}
