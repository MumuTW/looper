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
	gitdir, ok := parseGitdirPointer(path, data)
	if !ok {
		// Malformed gitfile: real Git rejects non-gitdir: content as
		// "invalid gitfile format". Treat as unusable so prepare can recreate.
		return false
	}
	// Linked private gitdir must have a syntactically valid HEAD, not merely a
	// regular file. An empty/corrupt HEAD still makes `git` report "not a git
	// repository"; if we only checked presence, prepare would retry forever
	// without recreating.
	if !localGitHEADUsable(gitdir) {
		return false
	}
	// commondir is required for linked worktrees: without it (or when it does not
	// resolve to a common repo with full local integrity), Git 2.43+ still fails
	// with "fatal: not a git repository" even though private HEAD remains.
	return linkedPrivateGitdirCommonUsable(gitdir)
}

// IsLinkedWorktree recognizes Git's gitdir-pointer form even when the private
// or common metadata it names is temporarily unavailable. The pointer itself
// is Git-administered state; destructive sweeps must preserve it rather than
// interpreting a failed usability probe as proof of disposable debris.
func IsLinkedWorktree(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	gitMeta := filepath.Join(path, ".git")
	info, err := os.Lstat(gitMeta)
	if err != nil || info.IsDir() {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, statErr := os.Stat(gitMeta)
		if statErr != nil || target.IsDir() {
			return false
		}
	}
	data, err := os.ReadFile(gitMeta)
	if err != nil {
		return false
	}
	_, ok := parseGitdirPointer(path, data)
	return ok
}

// IsStandaloneGitRepository reports whether path has ordinary repository
// metadata in a .git directory. Unlike a linked worktree's gitdir pointer, the
// directory may contain commits and objects that are not reachable elsewhere;
// cleanup must preserve it even when `git status` reports clean.
func IsStandaloneGitRepository(path string) bool {
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
		return true
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, statErr := os.Stat(gitMeta)
		return statErr == nil && target.IsDir()
	}
	return false
}

// IsUsableStandaloneGitRepository reports whether path is an ordinary checkout
// whose local Git metadata is complete enough for status checks. Incomplete
// metadata follows the existing recovery/indeterminate policy instead of being
// classified as a standalone repository solely because a .git directory is
// present.
func IsUsableStandaloneGitRepository(path string) bool {
	return IsStandaloneGitRepository(path) && LocalFixerWorktreeCheckoutUsable(path)
}

// HasMalformedLocalGitHEAD reports only syntactically malformed, readable HEAD
// metadata. It deliberately does not classify absent or incomplete .git data:
// callers must still let prepare/fetch errors distinguish a partial path from a
// remote transport failure before removing anything.
func HasMalformedLocalGitHEAD(path string) bool {
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
		return localGitHEADMalformed(gitMeta)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Stat(gitMeta)
		if err != nil || target.IsDir() {
			return err == nil && localGitHEADMalformed(gitMeta)
		}
	}
	data, err := os.ReadFile(gitMeta)
	if err != nil {
		return false
	}
	gitdir, ok := parseGitdirPointer(path, data)
	if !ok {
		return false
	}
	return localGitHEADMalformed(gitdir)
}

func parseGitdirPointer(worktreePath string, data []byte) (string, bool) {
	const prefix = "gitdir: "
	content := string(data)
	if !strings.HasPrefix(content, prefix) {
		return "", false
	}
	gitdir := strings.TrimSpace(content[len(prefix):])
	if gitdir == "" {
		return "", false
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktreePath, gitdir)
	}
	return gitdir, true
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
	if !localGitHEADUsable(dir) {
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

// LocalGitMetadataIndeterminate reports whether local Git metadata exists but
// could not be inspected safely. Sweep callers must fail closed for this case:
// an I/O/permission error or a symlinked object entry can hide recoverable
// objects and must not be treated as an empty repository.
func LocalGitMetadataIndeterminate(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	gitMeta := filepath.Join(path, ".git")
	info, err := os.Lstat(gitMeta)
	if err != nil {
		return false
	}
	if info.Mode().IsRegular() {
		data, readErr := os.ReadFile(gitMeta)
		if readErr != nil {
			return !os.IsNotExist(readErr)
		}
		gitdir, ok := parseGitdirPointer(path, data)
		if !ok {
			return false
		}
		return localGitMetadataIndeterminate(gitdir)
	}
	if info.IsDir() {
		return localGitMetadataIndeterminate(gitMeta)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, statErr := os.Stat(gitMeta)
		if statErr != nil {
			return !os.IsNotExist(statErr)
		}
		if target.IsDir() {
			return localGitMetadataIndeterminate(gitMeta)
		}
	}
	return false
}

func localGitMetadataIndeterminate(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil && !os.IsNotExist(err) {
		return true
	}
	objects, err := os.Stat(filepath.Join(dir, "objects"))
	if err != nil {
		return !os.IsNotExist(err)
	}
	if !objects.IsDir() {
		return false
	}
	if hasIndeterminateObjectEntries(filepath.Join(dir, "objects")) {
		return true
	}
	refs, err := os.Stat(filepath.Join(dir, "refs"))
	if err != nil {
		return !os.IsNotExist(err)
	}
	if !refs.IsDir() {
		return false
	}
	return false
}

func hasIndeterminateObjectEntries(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return true
		}
		if entry.IsDir() && hasIndeterminateObjectEntries(filepath.Join(root, entry.Name())) {
			return true
		}
	}
	return false
}

// localGitHEADUsable validates the local HEAD encoding without contacting Git
// or any remote. HEAD is either a symbolic ref with Git's "ref: " prefix or a
// detached 40/64-hex object ID. A merely non-empty HEAD is not sufficient:
// values such as "not-a-valid-head" make Git reject the checkout as invalid.
func localGitHEADUsable(dir string) bool {
	headPath := filepath.Join(dir, "HEAD")
	head, err := os.Stat(headPath)
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(headPath)
	if err != nil {
		return false
	}
	value := strings.TrimSpace(string(data))
	if strings.HasPrefix(value, "ref: ") {
		return localGitRefNameUsable(strings.TrimSpace(strings.TrimPrefix(value, "ref: ")))
	}
	return localGitObjectIDUsable(value)
}

func localGitHEADMalformed(dir string) bool {
	headPath := filepath.Join(dir, "HEAD")
	head, err := os.Stat(headPath)
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(headPath)
	if err != nil {
		return false
	}
	value := strings.TrimSpace(string(data))
	if strings.HasPrefix(value, "ref: ") {
		return !localGitRefNameUsable(strings.TrimSpace(strings.TrimPrefix(value, "ref: ")))
	}
	return value != "" && !localGitObjectIDUsable(value)
}

func localGitRefNameUsable(ref string) bool {
	// Equivalent to `git check-ref-format` for symbolic HEAD refs. Keep this
	// local and allocation-free: this probe runs during recovery and must honor
	// an explicitly configured Git binary that may not be on PATH.
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, ".") || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") {
		return false
	}
	for _, segment := range strings.Split(ref, "/") {
		// Git rejects empty components, hidden components, and lock-file names
		// because an update could otherwise collide with its ref lock protocol.
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	for _, runeValue := range ref {
		if runeValue <= ' ' || runeValue == 0x7f || strings.ContainsRune("~^:?*[\\\\", runeValue) {
			return false
		}
	}
	return true
}

func localGitObjectIDUsable(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, runeValue := range value {
		if !((runeValue >= '0' && runeValue <= '9') || (runeValue >= 'a' && runeValue <= 'f') || (runeValue >= 'A' && runeValue <= 'F')) {
			return false
		}
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

// SafeToRemoveUnusableWorktreePath applies the non-mutating form of the
// unusable-path cleanup policy. Empty directories and directories containing
// only unusable local Git metadata are disposable; any other populated path
// may contain interrupted agent work and must be preserved. Directory read
// errors are returned so callers can fail closed.
func SafeToRemoveUnusableWorktreePath(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return true, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return true, nil
	}
	// The disk sweep is more conservative than fixer path recovery: an
	// ordinary corrupt repository may still contain the only copy of local
	// commits, so object-bearing `.git` directories are not disposable debris.
	gitMeta := filepath.Join(path, ".git")
	if len(entries) == 1 && entries[0].Name() == ".git" {
		if info, statErr := os.Stat(gitMeta); statErr == nil && info.IsDir() && localGitRepositoryContainsObjects(gitMeta) {
			return false, nil
		}
	}
	return onlyUnusableLocalGitMetadata(path, entries), nil
}

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

// onlyUnusableLocalGitMetadata is true when path's single entry is a non-usable
// .git file/dir. Any additional entry is treated as possible agent dirt and
// must be preserved.
func onlyUnusableLocalGitMetadata(path string, entries []os.DirEntry) bool {
	if len(entries) != 1 || entries[0].Name() != ".git" {
		return false
	}
	return !LocalFixerWorktreeCheckoutUsable(path)
}

// localGitRepositoryContainsObjects reports conservatively whether a Git
// repository's object database contains data. A regular loose/pack file or a
// symlink is enough to preserve the repository; unreadable object metadata is
// also treated as present because deleting it would destroy state we cannot
// inspect.
func localGitRepositoryContainsObjects(gitDir string) bool {
	return objectEntriesContainData(filepath.Join(gitDir, "objects"))
}

func objectEntriesContainData(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.Type().IsRegular() {
			return true
		}
		if entry.IsDir() && objectEntriesContainData(filepath.Join(root, entry.Name())) {
			return true
		}
	}
	return false
}
