package daemonservice

import (
	"os"
	"path/filepath"
	"strings"
)

// PreferReleaseCurrentExecutable rewrites a looperd path that lives under a
// release tree (root/releases/<id>/looperd) to root/current/looperd when that
// atomic pointer exists. Supervised services must launch through current so
// activate-release can switch binaries without rewriting unit files.
//
// Paths outside a release tree, or trees without a current pointer yet, are
// returned unchanged. When multiple "releases" path segments exist, the
// innermost releases/<id>/<binary> suffix wins (e.g. /srv/releases/looper/...).
func PreferReleaseCurrentExecutable(executable string) string {
	path := strings.TrimSpace(executable)
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	cleaned := filepath.Clean(path)
	releasesMarker := string(filepath.Separator) + "releases" + string(filepath.Separator)
	// Innermost marker: avoid treating an ancestor .../releases/... as the tree root.
	idx := strings.LastIndex(cleaned, releasesMarker)
	if idx < 0 {
		return path
	}
	root := cleaned[:idx]
	if root == "" {
		return path
	}
	// Expect .../releases/<id>/<basename>
	rest := cleaned[idx+len(releasesMarker):]
	parts := strings.Split(rest, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return path
	}
	currentLink := filepath.Join(root, "current")
	if info, err := os.Lstat(currentLink); err != nil {
		// No current pointer yet (first install before any activate). Keep the
		// concrete release path rather than inventing a missing current.
		return path
	} else if info.Mode()&os.ModeSymlink == 0 {
		// Prefer a real current symlink; a plain directory is ambiguous.
		return path
	}
	// Require current to resolve inside this release tree and name an existing
	// executable before rewriting the unit — dangling or external targets must
	// not produce a service that cannot boot.
	resolved, err := filepath.EvalSymlinks(currentLink)
	if err != nil {
		return path
	}
	releasesRoot, err := filepath.EvalSymlinks(filepath.Join(root, "releases"))
	if err != nil {
		releasesRoot = filepath.Clean(filepath.Join(root, "releases"))
	}
	rel, err := filepath.Rel(filepath.Clean(releasesRoot), filepath.Clean(resolved))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	expectedRelease, err := filepath.EvalSymlinks(filepath.Join(root, "releases", parts[0]))
	if err != nil || filepath.Clean(resolved) != filepath.Clean(expectedRelease) {
		return path
	}
	releaseInfo, err := os.Lstat(filepath.Join(root, "releases", parts[0]))
	if err != nil || releaseInfo.Mode()&os.ModeSymlink != 0 || !releaseInfo.IsDir() {
		return path
	}
	candidate := filepath.Join(root, "current", parts[1])
	if info, err := os.Lstat(candidate); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return path
	}
	return candidate
}
