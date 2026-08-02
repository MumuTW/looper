package worktreesafety

import (
	"path/filepath"
	"strings"
	"sync"
)

// WorktreeMutationScope returns the managed repository-container scope that
// must be shared by worktree creation/restoration and disk-sweep removal. A
// project root is one level below its container in the managed layout, so
// unrelated repositories do not contend while container removal still fences
// every checkout that it could delete.
func WorktreeMutationScope(worktreeRoot string) string {
	root := strings.TrimSpace(worktreeRoot)
	if root == "" {
		return ""
	}
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
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
