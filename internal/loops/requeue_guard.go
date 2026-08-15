package loops

import (
	"fmt"
	"strings"
	"sync"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/storage"
)

// guardRegistry hands out per-key mutexes and reclaims entries when the last
// holder or waiter releases, so a long-running daemon churning through
// one-shot loops does not grow the registries monotonically.
//
// The registry map (under registryMu) is the single mutex authority per key:
// an entry is present iff refs > 0, refs counts holders plus waiters, and an
// entry is deleted only at refs == 0 under registryMu — so a later lock on
// the same key can never mint a second live mutex for a goroutine still using
// the old one.
type guardRegistry struct {
	registryMu sync.Mutex
	entries    map[string]*guardEntry
}

type guardEntry struct {
	mu   sync.Mutex
	refs int
}

func newGuardRegistry() *guardRegistry {
	return &guardRegistry{entries: make(map[string]*guardEntry)}
}

func (r *guardRegistry) lock(key string) func() {
	r.registryMu.Lock()
	entry, ok := r.entries[key]
	if !ok {
		entry = &guardEntry{}
		r.entries[key] = entry
	}
	entry.refs++
	r.registryMu.Unlock()

	entry.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			r.registryMu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(r.entries, key)
			}
			r.registryMu.Unlock()
		})
	}
}

// loopRequeueGuards serializes per-loop queue rearm across the API discard/retry
// path and runtime free-text / HITL / recovery / discovery requeues. Without a
// process-wide mutex, concurrent requeue can land after API preflight and
// before git reset, wiping the worktree for a continuation that then loses the
// retry transaction to an active-queue conflict.
var loopRequeueGuards = newGuardRegistry()

// loopTargetGuards serializes same worktree-target mutations across API
// discard/retry/start/create and runtime recovery/discovery/HITL requeues.
// Per-loop locks alone cannot block a *different* loop on the shared PR
// worktree from requeuing between discard preflight and git reset.
var loopTargetGuards = newGuardRegistry()

// LockLoopRequeue acquires the process-wide per-loop requeue mutex shared by:
//   - API retry/start/reuse
//   - runtime HITL free-text / answer requeues
//   - runtime deferred/startup recovery requeues
//   - reviewer/fixer discovery enqueue for an existing loop
//
// Callers must unlock via the returned function (typically defer). Nested
// acquisitions on the same loop from the same goroutine deadlock — do not
// call requeue helpers while already holding this lock for that loopID.
// Call order with LockLoopTarget: take the per-loop lock first, then the
// target lock.
func LockLoopRequeue(loopID string) func() {
	return loopRequeueGuards.lock(loopID)
}

// LoopTargetGuardKey builds the process-wide target mutex key shared by API
// discard/retry/start/create and runtime recovery/discovery/HITL requeues.
//
// Empty string means no lock (concurrent project-scoped workers are exempt).
// Pull-request targets omit loop type so fixer/reviewer/worker share one key —
// they share the managed PR worktree (looper-fix-<project>-pr-N).
func LoopTargetGuardKey(projectID, loopType, targetType, targetKey string) string {
	projectID = strings.TrimSpace(projectID)
	loopType = strings.TrimSpace(loopType)
	targetType = strings.TrimSpace(targetType)
	targetKey = strings.TrimSpace(targetKey)
	if targetKey == "" {
		return ""
	}
	if loopType == string(domain.LoopTypeWorker) && targetType == string(domain.LoopTargetTypeProject) {
		return ""
	}
	if targetType == string(domain.LoopTargetTypePullRequest) {
		return fmt.Sprintf("%s|%s", projectID, targetKey)
	}
	return fmt.Sprintf("%s|%s|%s", projectID, loopType, targetKey)
}

// LoopTargetGuardKeyFromRecord derives LoopTargetGuardKey from a stored loop.
// Key formatting matches API loopTargetKeyFromRecordCompat / loopTargetKeyCompat
// so API and runtime share the same mutex entries.
func LoopTargetGuardKeyFromRecord(loop storage.LoopRecord) string {
	return LoopTargetGuardKey(loop.ProjectID, loop.Type, loop.TargetType, TargetKeyFromLoopRecord(loop))
}

// PullRequestTargetGuardKey builds the shared PR worktree target mutex key from
// project + repo + PR number (loop type omitted). Use when discovery creates or
// requeues a reviewer/fixer for a PR before a loop record is available.
func PullRequestTargetGuardKey(projectID, repo string, prNumber int64) string {
	projectID = strings.TrimSpace(projectID)
	repo = strings.TrimSpace(repo)
	if projectID == "" || repo == "" || prNumber <= 0 {
		return ""
	}
	return LoopTargetGuardKey(projectID, string(domain.LoopTypeReviewer), string(domain.LoopTargetTypePullRequest), fmt.Sprintf("pull_request:%s:%d", NormalizeRepoForGuardKey(repo), prNumber))
}

// TargetKeyFromLoopRecord returns the canonical target key for a stored loop
// (project:/issue:/pull_request:...), matching API loopTargetKeyFromRecordCompat.
func TargetKeyFromLoopRecord(loop storage.LoopRecord) string {
	switch loop.TargetType {
	case string(domain.LoopTargetTypeProject):
		if loop.TargetID == nil {
			return "project:"
		}
		normalized := strings.TrimSpace(*loop.TargetID)
		for strings.HasPrefix(normalized, "project:") {
			normalized = strings.TrimPrefix(normalized, "project:")
		}
		return "project:" + normalized
	case string(domain.LoopTargetTypeIssue):
		if loop.TargetID == nil {
			return "issue:"
		}
		return *loop.TargetID
	default:
		if loop.Repo == nil || loop.PRNumber == nil {
			return "pull_request:"
		}
		return fmt.Sprintf("pull_request:%s:%d", NormalizeRepoForGuardKey(*loop.Repo), *loop.PRNumber)
	}
}

// NormalizeRepoForGuardKey lower-cases the repository identifier so that
// sibling loops spelling the same GitHub repo with different casing
// (acme/repo vs Acme/Repo) take the same target mutex key. This matches the
// case-insensitive identity used by storage.ListByRepoAndPR (COLLATE NOCASE)
// and storage forge lock keys (strings.ToLower).
func NormalizeRepoForGuardKey(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}

// LockLoopTarget acquires the process-wide same-target mutex. An empty key is a
// no-op. Callers that also take LockLoopRequeue must acquire the per-loop lock
// first to avoid deadlocks with API discard+retry.
func LockLoopTarget(key string) func() {
	if strings.TrimSpace(key) == "" {
		return func() {}
	}
	return loopTargetGuards.lock(key)
}
