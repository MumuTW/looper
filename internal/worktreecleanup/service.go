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
	Repos *storage.Repositories
	Now   func() time.Time
}

type PlanOptions struct {
	Config config.WorktreeCleanupConfig
	DryRun bool
}

type Plan struct {
	Scanned    int         `json:"scanned"`
	Candidates int         `json:"candidates"`
	WouldClean int         `json:"wouldClean"`
	Skipped    int         `json:"skipped"`
	Failed     int         `json:"failed"`
	Items      []PlanItem  `json:"items"`
	Summary    PlanSummary `json:"summary"`
}

type PlanSummary struct {
	Scanned    int `json:"scanned"`
	Candidates int `json:"candidates"`
	WouldClean int `json:"wouldClean"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Orphans    int `json:"orphans"`
}

type PlanItem struct {
	WorktreeID   string     `json:"worktreeId"`
	ProjectID    string     `json:"projectId"`
	RepoPath     string     `json:"repoPath"`
	WorktreePath string     `json:"worktreePath"`
	Branch       string     `json:"branch"`
	Decision     Decision   `json:"decision"`
	Reason       string     `json:"reason,omitempty"`
	LastUsedAt   *time.Time `json:"lastUsedAt,omitempty"`
	LoopID       string     `json:"loopId,omitempty"`
	RunID        string     `json:"runId,omitempty"`
	QueueItemID  string     `json:"queueItemId,omitempty"`
}

type Decision string

const (
	DecisionWouldClean Decision = "would_clean"
	DecisionSkipped    Decision = "skipped"
	DecisionFailed     Decision = "failed"
)

type worktreeState struct {
	record        storage.WorktreeRecord
	lastUsedAt    time.Time
	lastUsedValid bool
	loopID        string
	runID         string
	queueItemID   string
	protected     []string
	failed        []string
	referenced    bool
}

type worktreeRef struct {
	id   string
	path string
}

func (s *Service) Plan(ctx context.Context, options PlanOptions) (Plan, error) {
	if s == nil || s.Repos == nil || s.Repos.Worktrees == nil || s.Repos.Loops == nil || s.Repos.Runs == nil || s.Repos.Queue == nil {
		return Plan{}, fmt.Errorf("worktree cleanup service is not configured")
	}

	cfg := options.Config
	if options.DryRun {
		cfg.DryRun = true
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}

	worktrees, err := s.Repos.Worktrees.ListActive(ctx)
	if err != nil {
		return Plan{}, err
	}
	states := make(map[string]*worktreeState, len(worktrees))
	byID := make(map[string]*worktreeState, len(worktrees))
	byProject := map[string][]*worktreeState{}
	for _, worktree := range worktrees {
		state := &worktreeState{record: worktree}
		if parsed, ok := parseTime(worktree.UpdatedAt); ok {
			state.lastUsedAt = parsed
			state.lastUsedValid = true
		} else {
			state.failed = append(state.failed, "invalid_worktree_updated_at")
		}
		states[worktreeKey(worktree)] = state
		if strings.TrimSpace(worktree.ID) != "" {
			byID[worktree.ID] = state
		}
		byProject[worktree.ProjectID] = append(byProject[worktree.ProjectID], state)
	}

	loops, err := s.Repos.Loops.List(ctx)
	if err != nil {
		return Plan{}, err
	}
	loopsByID := make(map[string]storage.LoopRecord, len(loops))
	for _, loop := range loops {
		loopsByID[loop.ID] = loop
	}

	runs, err := s.Repos.Runs.List(ctx)
	if err != nil {
		return Plan{}, err
	}
	for _, run := range runs {
		loop := loopsByID[run.LoopID]
		refs, parseErr := checkpointWorktreeRefs(run.CheckpointJSON)
		if parseErr != nil {
			for _, state := range byProject[loop.ProjectID] {
				state.protect("checkpoint_parse_failed", loop.ID, run.ID, "")
			}
			continue
		}
		for _, ref := range refs {
			state := resolveState(ref, loop.ProjectID, states, byID)
			if state == nil {
				continue
			}
			state.referenced = true
			state.loopID = firstNonEmpty(state.loopID, loop.ID)
			state.runID = firstNonEmpty(state.runID, run.ID)
			state.bumpTime(run.StartedAt)
			state.bumpTime(run.UpdatedAt)
			if run.EndedAt != nil {
				state.bumpTime(*run.EndedAt)
			}
			if isProtectedLoopStatus(loop.Status) {
				state.protect("loop_"+loop.Status, loop.ID, run.ID, "")
			}
			if run.Status == string(domain.RunStatusRunning) {
				state.protect("run_running", loop.ID, run.ID, "")
			}
		}
	}

	queueItems, err := s.Repos.Queue.List(ctx)
	if err != nil {
		return Plan{}, err
	}
	for _, item := range queueItems {
		loop := storage.LoopRecord{}
		if item.LoopID != nil {
			loop = loopsByID[*item.LoopID]
		}
		refs, parseErr := queueWorktreeRefs(item.PayloadJSON)
		if parseErr != nil && isActiveQueueStatus(item.Status) {
			for _, state := range byProject[derefString(item.ProjectID, loop.ProjectID)] {
				state.protect("queue_payload_parse_failed", loop.ID, "", item.ID)
			}
			continue
		}
		if item.LoopID != nil {
			for _, state := range states {
				if state.loopID == *item.LoopID {
					state.bumpTime(item.UpdatedAt)
					if isActiveQueueStatus(item.Status) {
						state.protect("queue_"+item.Status, *item.LoopID, "", item.ID)
					}
				}
			}
		}
		for _, ref := range refs {
			state := resolveState(ref, derefString(item.ProjectID, loop.ProjectID), states, byID)
			if state == nil {
				continue
			}
			state.referenced = true
			state.queueItemID = firstNonEmpty(state.queueItemID, item.ID)
			state.bumpTime(item.UpdatedAt)
			if isActiveQueueStatus(item.Status) {
				state.protect("queue_"+item.Status, derefString(item.LoopID, loop.ID), "", item.ID)
			}
		}
	}

	retentionCutoff := now().UTC().Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
	plan := Plan{Scanned: len(worktrees)}
	for _, state := range states {
		item := PlanItem{
			WorktreeID:   state.record.ID,
			ProjectID:    state.record.ProjectID,
			RepoPath:     state.record.RepoPath,
			WorktreePath: state.record.WorktreePath,
			Branch:       state.record.Branch,
			LoopID:       state.loopID,
			RunID:        state.runID,
			QueueItemID:  state.queueItemID,
		}
		if state.lastUsedValid {
			lastUsed := state.lastUsedAt
			item.LastUsedAt = &lastUsed
		}
		if len(state.failed) > 0 {
			item.Decision = DecisionFailed
			item.Reason = strings.Join(dedupeStrings(state.failed), ",")
			plan.Failed++
		} else if len(state.protected) > 0 {
			item.Decision = DecisionSkipped
			item.Reason = strings.Join(dedupeStrings(state.protected), ",")
			plan.Skipped++
		} else if !state.referenced && !cfg.IncludeOrphans {
			item.Decision = DecisionSkipped
			item.Reason = "orphan"
			plan.Skipped++
			plan.Summary.Orphans++
		} else if state.lastUsedAt.After(retentionCutoff) {
			item.Decision = DecisionSkipped
			item.Reason = "retention"
			plan.Skipped++
		} else {
			item.Decision = DecisionWouldClean
			plan.Candidates++
			plan.WouldClean++
		}
		plan.Items = append(plan.Items, item)
	}
	sort.Slice(plan.Items, func(i, j int) bool {
		left, right := plan.Items[i], plan.Items[j]
		if left.Decision != right.Decision {
			return left.Decision < right.Decision
		}
		if left.LastUsedAt == nil || right.LastUsedAt == nil {
			return left.WorktreeID < right.WorktreeID
		}
		return left.LastUsedAt.Before(*right.LastUsedAt)
	})
	limitWouldClean(&plan, cfg.MaxPerTick)
	plan.Summary.Scanned = plan.Scanned
	plan.Summary.Candidates = plan.Candidates
	plan.Summary.WouldClean = plan.WouldClean
	plan.Summary.Skipped = plan.Skipped
	plan.Summary.Failed = plan.Failed
	return plan, nil
}

func (s *worktreeState) protect(reason, loopID, runID, queueItemID string) {
	s.protected = append(s.protected, reason)
	s.loopID = firstNonEmpty(s.loopID, loopID)
	s.runID = firstNonEmpty(s.runID, runID)
	s.queueItemID = firstNonEmpty(s.queueItemID, queueItemID)
}

func (s *worktreeState) bumpTime(value string) {
	if parsed, ok := parseTime(value); ok && (!s.lastUsedValid || parsed.After(s.lastUsedAt)) {
		s.lastUsedAt = parsed
		s.lastUsedValid = true
	}
}

func limitWouldClean(plan *Plan, maxPerTick int) {
	if maxPerTick < 1 || plan.WouldClean <= maxPerTick {
		return
	}
	kept := 0
	for i := range plan.Items {
		if plan.Items[i].Decision != DecisionWouldClean {
			continue
		}
		kept++
		if kept <= maxPerTick {
			continue
		}
		plan.Items[i].Decision = DecisionSkipped
		plan.Items[i].Reason = "max_per_tick"
		plan.WouldClean--
		plan.Skipped++
	}
}

func checkpointWorktreeRefs(raw *string) ([]worktreeRef, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal([]byte(*raw), &value); err != nil {
		return nil, err
	}
	return collectWorktreeRefs(value), nil
}

func queueWorktreeRefs(raw *string) ([]worktreeRef, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal([]byte(*raw), &value); err != nil {
		return nil, err
	}
	return collectWorktreeRefs(value), nil
}

func collectWorktreeRefs(value any) []worktreeRef {
	refs := []worktreeRef{}
	var walk func(any, string)
	walk = func(v any, key string) {
		switch typed := v.(type) {
		case map[string]any:
			if key == "worktree" {
				refs = append(refs, worktreeRef{id: stringField(typed, "id", "worktreeId"), path: stringField(typed, "path", "worktreePath")})
			}
			if path := stringField(typed, "worktreePath"); path != "" {
				refs = append(refs, worktreeRef{id: stringField(typed, "worktreeId"), path: path})
			}
			for childKey, child := range typed {
				walk(child, childKey)
			}
		case []any:
			for _, child := range typed {
				walk(child, "")
			}
		}
	}
	walk(value, "")
	return refs
}

func resolveState(ref worktreeRef, projectID string, states map[string]*worktreeState, byID map[string]*worktreeState) *worktreeState {
	if ref.id != "" {
		if state := byID[ref.id]; state != nil {
			return state
		}
	}
	if ref.path == "" {
		return nil
	}
	return states[projectID+"|"+normalizePath(ref.path)]
}

func worktreeKey(record storage.WorktreeRecord) string {
	return record.ProjectID + "|" + normalizePath(record.WorktreePath)
}

func normalizePath(path string) string {
	return strings.TrimSpace(path)
}

func isProtectedLoopStatus(status string) bool {
	switch domain.LoopStatus(status) {
	case domain.LoopStatusQueued, domain.LoopStatusRunning, domain.LoopStatusWaiting, domain.LoopStatusPaused, domain.LoopStatusFailed, domain.LoopStatusInterrupted:
		return true
	default:
		return false
	}
}

func isActiveQueueStatus(status string) bool {
	return status == "queued" || status == "running"
}

func parseTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func stringField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func derefString(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return firstNonEmpty(*value, fallback)
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
