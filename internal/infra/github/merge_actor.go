package github

import "strings"

// IsMergifyMergeActor reports whether a forge merge actor is one of the
// repository's recognized Mergify accounts. Gatekeeper and Coordinator both
// use this provider observation before attributing a merge to the Mergify
// route; a non-empty merge timestamp alone is not route evidence because a
// maintainer can merge the pull request directly.
func IsMergifyMergeActor(login string) bool {
	switch strings.ToLower(strings.TrimSpace(login)) {
	case "mergify", "mergify[bot]", "mergifyio", "mergifyio[bot]":
		return true
	default:
		return false
	}
}
