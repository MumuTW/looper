package gatekeeper

import (
	"fmt"
	"path"
	"strings"
)

const maxProtectedPathReasonSubject = 2048

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
	type state struct{ pattern, changed int }
	memo := make(map[state]bool)
	seen := make(map[state]bool)
	var match func(int, int) bool
	match = func(patternIndex, changedIndex int) bool {
		key := state{pattern: patternIndex, changed: changedIndex}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		var result bool
		switch {
		case patternIndex == len(pattern):
			result = changedIndex == len(changed)
		case pattern[patternIndex] == "**":
			result = match(patternIndex+1, changedIndex) || (changedIndex < len(changed) && match(patternIndex, changedIndex+1))
		case changedIndex < len(changed):
			matched, _ := path.Match(pattern[patternIndex], changed[changedIndex])
			result = matched && match(patternIndex+1, changedIndex+1)
		}
		memo[key] = result
		return result
	}
	return match(0, 0)
}

func protectedPathReasonSubject(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	full := strings.Join(paths, ", ")
	if len(full) <= maxProtectedPathReasonSubject {
		return full
	}
	for included := len(paths); included > 0; included-- {
		suffix := fmt.Sprintf(" … (+%d more)", len(paths)-included)
		prefix := strings.Join(paths[:included], ", ")
		if len(prefix)+len(suffix) <= maxProtectedPathReasonSubject {
			return prefix + suffix
		}
	}
	return fmt.Sprintf("%d protected paths", len(paths))
}

func normalizeRepositoryPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	return strings.TrimPrefix(value, "./")
}
