package gatekeeper

import (
	"path"
	"strings"
)

// matchedProtectedPaths returns changed paths that match at least one
// repository-relative protected glob. It preserves the provider's path order
// and never treats a pattern itself as evidence.
func matchedProtectedPaths(changedPaths, patterns []string) []string {
	matched := make([]string, 0)
	seen := make(map[string]struct{})
	for _, changed := range changedPaths {
		changed = normalizeRepositoryPath(changed)
		if changed == "" {
			continue
		}
		for _, pattern := range patterns {
			if protectedPathMatch(pattern, changed) {
				if _, exists := seen[changed]; !exists {
					seen[changed] = struct{}{}
					matched = append(matched, changed)
				}
				break
			}
		}
	}
	return matched
}

func protectedPathMatch(pattern, changed string) bool {
	pattern = normalizeRepositoryPath(pattern)
	changed = normalizeRepositoryPath(changed)
	if pattern == "" || changed == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		for _, segment := range strings.Split(changed, "/") {
			if ok, _ := path.Match(pattern, segment); ok {
				return true
			}
		}
		return false
	}
	return protectedPathSegments(strings.Split(pattern, "/"), strings.Split(changed, "/"))
}

func protectedPathSegments(pattern, changed []string) bool {
	if len(pattern) == 0 {
		return len(changed) == 0
	}
	if pattern[0] == "**" {
		if protectedPathSegments(pattern[1:], changed) {
			return true
		}
		return len(changed) > 0 && protectedPathSegments(pattern, changed[1:])
	}
	if len(changed) == 0 {
		return false
	}
	if ok, _ := path.Match(pattern[0], changed[0]); !ok {
		return false
	}
	return protectedPathSegments(pattern[1:], changed[1:])
}

func normalizeRepositoryPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	return strings.TrimPrefix(value, "./")
}
