package worktreesafety

import (
	"testing"
	"time"
)

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
