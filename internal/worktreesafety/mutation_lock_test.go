package worktreesafety

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorktreeMutationScopeResolvesSymlinkIdentity(t *testing.T) {
	root := t.TempDir()
	realContainer := filepath.Join(root, "real-container")
	if err := os.MkdirAll(realContainer, 0o755); err != nil {
		t.Fatal(err)
	}
	realRoot := filepath.Join(realContainer, "project")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(root, "alias-project")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}

	if got, want := WorktreeMutationScope(aliasRoot), WorktreeMutationScope(realRoot); got != want {
		t.Fatalf("WorktreeMutationScope(alias) = %q, want resolved scope %q", got, want)
	}
}

func TestWorktreeMutationScopeUsesEnclosingContainerForNestedRoot(t *testing.T) {
	looperHome := t.TempDir()
	t.Setenv("LOOPER_HOME", looperHome)
	sharedRoot := filepath.Join(looperHome, "worktrees")
	deepRoot := filepath.Join(sharedRoot, "custom", "team", "project")
	if err := os.MkdirAll(deepRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", deepRoot, err)
	}

	got := WorktreeMutationScope(deepRoot)
	want := NormalizePath(filepath.Join(sharedRoot, "custom"))
	if got != want {
		t.Fatalf("WorktreeMutationScope(%q) = %q, want enclosing container %q", deepRoot, got, want)
	}
}

func TestManagedMutationLockSerializesSameScopeAndAllowsIndependentScope(t *testing.T) {
	release := AcquireManagedMutationLock("/tmp/looper/repo-a")
	defer release()

	blocked := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(blocked)
		unlock := AcquireManagedMutationLock("/tmp/looper/repo-a")
		close(acquired)
		unlock()
	}()
	<-blocked
	select {
	case <-acquired:
		t.Fatal("same mutation scope acquired concurrently")
	case <-time.After(20 * time.Millisecond):
	}

	independent := make(chan struct{})
	go func() {
		unlock := AcquireManagedMutationLock("/tmp/looper/repo-b")
		close(independent)
		unlock()
	}()
	select {
	case <-independent:
	case <-time.After(time.Second):
		t.Fatal("independent mutation scope was blocked")
	}
	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("same mutation scope did not acquire after release")
	}
}
