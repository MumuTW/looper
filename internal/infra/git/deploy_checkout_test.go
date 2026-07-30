package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deployCheckoutRepo builds a remote plus a clone with two commits on main, and
// returns the clone path and both commit SHAs, oldest first.
func deployCheckoutRepo(t *testing.T) (repoPath string, first string, second string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	mustMkdirAll(t, remote)
	runGit(t, root, "init", "--bare", "-b", "main", remote)
	runGit(t, root, "clone", remote, work)
	configureRepo(t, work)

	writeFile(t, filepath.Join(work, "app.txt"), "v1\n")
	runGit(t, work, "add", "app.txt")
	runGit(t, work, "commit", "-m", "v1")
	first = strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))

	writeFile(t, filepath.Join(work, "app.txt"), "v2\n")
	runGit(t, work, "add", "app.txt")
	runGit(t, work, "commit", "-m", "v2")
	second = strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))

	runGit(t, work, "push", "origin", "main")
	return work, first, second
}

// The whole point of the operation: what runs is the commit that gets recorded,
// not whatever the project checkout happens to contain.
func TestCreateDeployCheckoutMaterializesTheRequestedCommit(t *testing.T) {
	repoPath, first, second := deployCheckoutRepo(t)
	root := filepath.Join(t.TempDir(), "checkouts")
	gateway := New(Options{})

	// Move the project checkout off the commit being deployed, which is the
	// situation that made the previous design report untrue deployments.
	runGit(t, repoPath, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(repoPath, "app.txt"), "uncommitted local edit\n")

	checkout, err := gateway.CreateDeployCheckout(context.Background(), DeployCheckoutInput{
		RepoPath: repoPath, SHA: first, Root: root,
	})
	if err != nil {
		t.Fatalf("CreateDeployCheckout() error = %v", err)
	}
	t.Cleanup(func() { _ = gateway.RemoveDeployCheckout(context.Background(), checkout, repoPath) })

	if head := strings.TrimSpace(runGit(t, checkout.Path, "rev-parse", "HEAD")); head != first {
		t.Fatalf("checkout HEAD = %s, want %s", head, first)
	}
	contents, err := os.ReadFile(filepath.Join(checkout.Path, "app.txt"))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if strings.TrimSpace(string(contents)) != "v1" {
		t.Fatalf("materialized contents = %q, want the requested commit's, not the working tree's", contents)
	}
	if second == first {
		t.Fatal("fixture produced identical commits")
	}
}

// An abbreviation is ambiguous across time, and a branch name can move between
// being resolved and being used — which is the bug this operation exists to stop.
func TestCreateDeployCheckoutRejectsAnythingButAFullSHA(t *testing.T) {
	repoPath, first, _ := deployCheckoutRepo(t)
	root := filepath.Join(t.TempDir(), "checkouts")
	gateway := New(Options{})

	for _, ref := range []string{"main", first[:8], "", "not-a-sha"} {
		if _, err := gateway.CreateDeployCheckout(context.Background(), DeployCheckoutInput{
			RepoPath: repoPath, SHA: ref, Root: root,
		}); err == nil {
			t.Fatalf("CreateDeployCheckout accepted %q", ref)
		}
	}
}

// A directory left by an interrupted deploy is not a usable checkout, and the
// path is derived from the commit so a retry lands on the same one.
func TestCreateDeployCheckoutRepairsAnInterruptedCheckout(t *testing.T) {
	repoPath, first, _ := deployCheckoutRepo(t)
	root := filepath.Join(t.TempDir(), "checkouts")
	gateway := New(Options{})
	ctx := context.Background()

	first1, err := gateway.CreateDeployCheckout(ctx, DeployCheckoutInput{RepoPath: repoPath, SHA: first, Root: root})
	if err != nil {
		t.Fatalf("first CreateDeployCheckout() error = %v", err)
	}
	// Simulate an interruption: the directory survives, the deploy did not.
	writeFile(t, filepath.Join(first1.Path, "leftover.txt"), "from a dead deploy\n")

	second, err := gateway.CreateDeployCheckout(ctx, DeployCheckoutInput{RepoPath: repoPath, SHA: first, Root: root})
	if err != nil {
		t.Fatalf("second CreateDeployCheckout() error = %v", err)
	}
	t.Cleanup(func() { _ = gateway.RemoveDeployCheckout(ctx, second, repoPath) })

	if second.Path != first1.Path {
		t.Fatalf("retry used %s, want the same path %s", second.Path, first1.Path)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "leftover.txt")); !os.IsNotExist(err) {
		t.Fatal("retry reused a directory still holding the dead deploy's files")
	}
}

// The desired end state is that the checkout is gone; being asked twice is not an
// error, and the deferred release in the lane runs unconditionally.
func TestRemoveDeployCheckoutIsIdempotent(t *testing.T) {
	repoPath, first, _ := deployCheckoutRepo(t)
	root := filepath.Join(t.TempDir(), "checkouts")
	gateway := New(Options{})
	ctx := context.Background()

	checkout, err := gateway.CreateDeployCheckout(ctx, DeployCheckoutInput{RepoPath: repoPath, SHA: first, Root: root})
	if err != nil {
		t.Fatalf("CreateDeployCheckout() error = %v", err)
	}
	if err := gateway.RemoveDeployCheckout(ctx, checkout, repoPath); err != nil {
		t.Fatalf("first RemoveDeployCheckout() error = %v", err)
	}
	if _, err := os.Stat(checkout.Path); !os.IsNotExist(err) {
		t.Fatalf("checkout still present after removal")
	}
	if err := gateway.RemoveDeployCheckout(ctx, checkout, repoPath); err != nil {
		t.Fatalf("second RemoveDeployCheckout() error = %v", err)
	}
}
