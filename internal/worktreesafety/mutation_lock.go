package worktreesafety

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/MumuTW/looper/internal/config"
)

// WorktreeMutationScope returns the managed repository-container scope that
// must be shared by worktree creation/restoration and disk-sweep removal. A
// project root may be nested below an explicit container, so derive the first
// segment below the canonical shared root when possible; the parent fallback
// preserves callers that operate outside the managed shared-root layout.
func WorktreeMutationScope(worktreeRoot string) string {
	root := strings.TrimSpace(worktreeRoot)
	if root == "" {
		return ""
	}
	// Use the same symlink-aware identity as the safety and sweep authorities.
	// A lexical alias must share the lock with its resolved root or creation can
	// race a sweep that is mutating the same physical checkout.
	root = NormalizePath(root)
	if sharedRoot, err := config.DefaultWorktreeRoot(); err == nil {
		shared := NormalizePath(sharedRoot)
		if rel, err := filepath.Rel(shared, root); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) >= 2 && parts[0] != "" {
				return filepath.Join(shared, parts[0])
			}
		}
	}
	return filepath.Clean(filepath.Dir(root))
}

type mutationLockRegistry struct {
	mu      sync.Mutex
	entries map[string]*mutationLockEntry
}

type mutationLockEntry struct {
	mu   sync.Mutex
	refs int
}

var managedMutationLocks = mutationLockRegistry{entries: make(map[string]*mutationLockEntry)}

// AcquireManagedMutationLock serializes destructive worktree mutations within
// one managed repository container. The lock is process-wide so independent
// Gateway instances and the runtime disk sweeper share the same fence.
func AcquireManagedMutationLock(scope string) func() {
	key := strings.TrimSpace(scope)
	if key == "" {
		return func() {}
	}
	if absolute, err := filepath.Abs(key); err == nil {
		key = filepath.Clean(absolute)
	} else {
		key = filepath.Clean(key)
	}
	managedMutationLocks.mu.Lock()
	entry := managedMutationLocks.entries[key]
	if entry == nil {
		entry = &mutationLockEntry{}
		managedMutationLocks.entries[key] = entry
	}
	entry.refs++
	managedMutationLocks.mu.Unlock()

	entry.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			managedMutationLocks.mu.Lock()
			entry.refs--
			if entry.refs == 0 && managedMutationLocks.entries[key] == entry {
				delete(managedMutationLocks.entries, key)
			}
			managedMutationLocks.mu.Unlock()
		})
	}
}
