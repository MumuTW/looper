package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DeployCheckoutInput materializes one exact commit for a deploy.
type DeployCheckoutInput struct {
	RepoPath string
	// SHA is the exact commit to check out. A branch name is rejected: the point
	// of this operation is that what runs is what was recorded, and a branch can
	// move between resolving it and using it.
	SHA string
	// Root is the directory ephemeral deploy checkouts live under.
	Root   string
	Remote string
}

// DeployCheckout is a materialized commit plus the means to release it.
type DeployCheckout struct {
	Path string
	SHA  string
}

// CreateDeployCheckout materializes an exact commit in a detached worktree.
//
// A deploy must run against the commit it reports as deployed. Executing in the
// project's own checkout would run whatever that directory happens to contain —
// another branch, stale contents, uncommitted edits — and then record the remote
// SHA as deployed, making the deployment record untrue.
//
// The worktree is deliberately outside the tracked worktree lifecycle: it exists
// for the duration of one deploy and is removed afterwards, so it has no loop,
// branch, or reuse semantics to record. Its path is derived from the SHA so a
// retry after a crash reuses and repairs the same directory rather than
// accumulating new ones.
func (g *Gateway) CreateDeployCheckout(ctx context.Context, input DeployCheckoutInput) (DeployCheckout, error) {
	sha := strings.TrimSpace(input.SHA)
	if !isFullCommitSHA(sha) {
		return DeployCheckout{}, fmt.Errorf("deploy checkout requires a full commit sha, got %q", input.SHA)
	}
	repoPath := strings.TrimSpace(input.RepoPath)
	if repoPath == "" {
		return DeployCheckout{}, fmt.Errorf("repo path is required")
	}
	root := strings.TrimSpace(input.Root)
	if root == "" {
		return DeployCheckout{}, fmt.Errorf("deploy checkout root is required")
	}
	remote := strings.TrimSpace(input.Remote)
	if remote == "" {
		remote = "origin"
	}

	path := filepath.Join(root, "deploy-"+sha[:12])
	// A directory left behind by an interrupted deploy is not a usable checkout:
	// remove it before recreating so the contents are known.
	if err := g.RemoveDeployCheckout(ctx, DeployCheckout{Path: path}, repoPath); err != nil {
		return DeployCheckout{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return DeployCheckout{}, fmt.Errorf("create deploy checkout root: %w", err)
	}

	// Fetch the commit itself rather than a branch: the branch may already have
	// moved past the commit being deployed.
	if err := g.runGit(ctx, repoPath, nil, "fetch", "--no-tags", remote, sha); err != nil {
		return DeployCheckout{}, fmt.Errorf("fetch %s: %w", sha, err)
	}
	if err := g.runGit(ctx, repoPath, nil, "worktree", "add", "--detach", path, sha); err != nil {
		return DeployCheckout{}, fmt.Errorf("materialize %s: %w", sha, err)
	}

	// Prove the checkout is at the requested commit rather than trusting that the
	// previous command did what it said.
	result, err := g.runGitResult(ctx, path, nil, "rev-parse", "HEAD")
	if err != nil {
		return DeployCheckout{}, fmt.Errorf("verify deploy checkout: %w", err)
	}
	if head := strings.TrimSpace(result.Stdout); head != sha {
		return DeployCheckout{}, fmt.Errorf("deploy checkout is at %s, want %s", head, sha)
	}
	return DeployCheckout{Path: path, SHA: sha}, nil
}

// RemoveDeployCheckout releases a deploy checkout. A checkout that is not there
// is not an error: the desired end state is that it is gone.
func (g *Gateway) RemoveDeployCheckout(ctx context.Context, checkout DeployCheckout, repoPath string) error {
	path := strings.TrimSpace(checkout.Path)
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Still prune the administrative entry: git keeps one after the directory
		// is gone, and it would block re-adding the same path.
		_ = g.runGit(ctx, repoPath, nil, "worktree", "prune")
		return nil
	}
	if err := g.runGit(ctx, repoPath, nil, "worktree", "remove", "--force", path); err != nil {
		// Fall back to removing the directory: a worktree git no longer recognises
		// must not strand every future deploy of that commit.
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return fmt.Errorf("remove deploy checkout %s: %w", path, rmErr)
		}
		_ = g.runGit(ctx, repoPath, nil, "worktree", "prune")
	}
	return nil
}

// isFullCommitSHA reports whether value is a 40-character hexadecimal object
// name. Abbreviations are rejected because they are ambiguous across time.
func isFullCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
