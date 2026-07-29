package worktreesafety

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsMissingOrUnusableFixerWorktree reports whether a checkpoint worktree path
// cannot be prepared in place and should be recreated. Authority is the local
// checkout only: path missing/gone, or prepare failed and a non-remote local
// probe shows the path is not a usable git checkout.
//
// Prepare/fetch/ssh stderr must never be treated as proof of local unusability
// by itself. Remote helpers can emit "fatal: not a git repository" while the
// managed worktree remains valid; force CleanupWorktree would then destroy
// interrupted agent dirt. When error text merely *looks* like a local integrity
// failure, confirm with LocalFixerWorktreeCheckoutUsable before recreating.
func IsMissingOrUnusableFixerWorktree(path string, prepErr error) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return true
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return true
		}
		// Permission/IO errors are not "missing"; let prepare surface them.
	}
	if prepErr == nil {
		// Path present: try prepare in place. Local usability is decided only
		// after prepare fails (via a non-remote probe, not error text alone).
		return false
	}
	// Path may have vanished during prepare; re-stat the checkout itself.
	if _, err := os.Stat(path); err != nil {
		return os.IsNotExist(err)
	}
	msg := strings.ToLower(prepErr.Error())
	// Integrity-looking phrases are necessary but not sufficient: remote
	// helpers/servers can emit the same text. Do not substring-match generic
	// existence phrases either — external dependency errors share that wording.
	// "invalid gitfile format" is Git's distinct message for a .git file that is
	// not a gitdir: pointer (malformed retained metadata).
	looksLikeLocalIntegrity := strings.Contains(msg, "not a working tree") ||
		strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "invalid gitfile format")
	if !looksLikeLocalIntegrity {
		return false
	}
	// Confirm with a non-remote local probe before force-cleaning.
	return !LocalFixerWorktreeCheckoutUsable(path)
}

// LocalFixerWorktreeCheckoutUsable reports whether path has local git metadata
// without contacting remotes. Linked worktrees use a .git file (gitdir pointer);
// ordinary checkouts use a .git directory. Metadata presence alone is not enough:
// ordinary repos and the linked common repository need full non-remote integrity
// (HEAD + objects/ + refs/), and linked private gitdirs need HEAD plus a
// resolvable usable common repo via commondir. Empty or corrupt metadata makes
// real Git report "not a git repository", so prepare can recreate instead of
// retrying forever.
//
// A non-empty .git file that is not a gitdir: pointer is also unusable: real Git
// reports "fatal: invalid gitfile format" and prepare must recreate rather than
// retry forever on retained broken metadata.
func LocalFixerWorktreeCheckoutUsable(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	gitMeta := filepath.Join(path, ".git")
	info, err := os.Lstat(gitMeta)
	if err != nil {
		return false
	}
	if info.IsDir() {
		// Ordinary checkout: require full local repository metadata.
		return LocalGitRepositoryMetadataUsable(gitMeta)
	}
	// Linked worktree or gitfile: .git is a regular file (or symlink to one).
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Stat(gitMeta)
		if err != nil {
			return false
		}
		if target.IsDir() {
			return LocalGitRepositoryMetadataUsable(gitMeta)
		}
	}
	data, err := os.ReadFile(gitMeta)
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		return false
	}
	const prefix = "gitdir:"
	if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
		// Malformed gitfile: real Git rejects non-gitdir: content as
		// "invalid gitfile format". Treat as unusable so prepare can recreate.
		return false
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if gitdir == "" {
		return false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	// Linked private gitdir must look like a real git dir (HEAD), not merely exist.
	// An empty/corrupt gitdir still makes `git` report "not a git repository"; if
	// we only checked existence, prepare would retry forever without recreating.
	privateHead, err := os.Stat(filepath.Join(gitdir, "HEAD"))
	if err != nil || !privateHead.Mode().IsRegular() {
		return false
	}
	// commondir is required for linked worktrees: without it (or when it does not
	// resolve to a common repo with full local integrity), Git 2.43+ still fails
	// with "fatal: not a git repository" even though private HEAD remains.
	return linkedPrivateGitdirCommonUsable(gitdir)
}

// LocalGitRepositoryMetadataUsable is a non-remote integrity probe for a Git
// repository directory (ordinary .git or a linked worktree common repo).
// HEAD alone is insufficient: Git 2.43+ also requires objects/ and refs/ and
// reports "fatal: not a git repository" when either directory is missing.
func LocalGitRepositoryMetadataUsable(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	head, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	objects, err := os.Stat(filepath.Join(dir, "objects"))
	if err != nil || !objects.IsDir() {
		return false
	}
	refs, err := os.Stat(filepath.Join(dir, "refs"))
	if err != nil || !refs.IsDir() {
		return false
	}
	return true
}

// linkedPrivateGitdirCommonUsable reports whether a linked worktree private
// gitdir has a readable commondir that resolves to a common repository with
// non-remote integrity (HEAD + objects/ + refs/).
func linkedPrivateGitdirCommonUsable(gitdir string) bool {
	data, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return false
	}
	common := strings.TrimSpace(string(data))
	if common == "" {
		return false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	common = filepath.Clean(common)
	return LocalGitRepositoryMetadataUsable(common)
}

// ErrUnusableFixerWorktreePreserved signals that an unusable worktree path still
// holds content after CleanupWorktree and must not be recreated over.
var ErrUnusableFixerWorktreePreserved = errors.New("unusable fixer worktree path preserved")

// ClearUnusableFixerWorktreePath handles filesystem leftovers after CleanupWorktree
// on a missing/unusable checkout. Git's `worktree remove --force` only removes
// registered worktrees; unregistered directories are left intact, which then
// blocks CreateWorktree when the managed path already exists.
//
// Policy: remove empty directories (safe stale), and remove directories whose only
// content is unusable local git metadata (corrupt/empty linked .git) so Create
// can reuse the managed path. Preserve any other populated path for manual
// intervention (may hold interrupted agent output even without a usable .git).
func ClearUnusableFixerWorktreePath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("fixer worktree path %s is unusable and not empty; manual intervention required: %w", path, ErrUnusableFixerWorktreePreserved)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	// Only corrupt/empty git metadata: safe to remove so prepare can recreate.
	if onlyUnusableLocalGitMetadata(path, entries) {
		if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return fmt.Errorf("fixer worktree path %s is unusable and not empty; manual intervention required: %w", path, ErrUnusableFixerWorktreePreserved)
}

// onlyUnusableLocalGitMetadata is true when path holds nothing but a non-usable
// .git file/dir (and optional nested empties under an ordinary .git dir). Any
// other entry is treated as possible agent dirt and must be preserved.
func onlyUnusableLocalGitMetadata(path string, entries []os.DirEntry) bool {
	if len(entries) != 1 || entries[0].Name() != ".git" {
		return false
	}
	return !LocalFixerWorktreeCheckoutUsable(path)
}
