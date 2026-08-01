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
// returned unchanged.
func PreferReleaseCurrentExecutable(executable string) string {
	path := strings.TrimSpace(executable)
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	cleaned := filepath.Clean(path)
	releasesMarker := string(filepath.Separator) + "releases" + string(filepath.Separator)
	idx := strings.Index(cleaned, releasesMarker)
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
	if _, err := os.Lstat(currentLink); err != nil {
		// No current pointer yet (first install before any activate). Keep the
		// concrete release path rather than inventing a missing current.
		return path
	}
	return filepath.Join(root, "current", parts[1])
}
