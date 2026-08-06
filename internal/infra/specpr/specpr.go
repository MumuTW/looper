package specpr

import (
	"regexp"
	"strings"

	"github.com/MumuTW/looper/internal/labels"
)

type PullRequestPhase string

const (
	PhaseSpec           PullRequestPhase = "spec"
	PhaseImplementation PullRequestPhase = "implementation"
)

var pathPattern = regexp.MustCompile(`(?mi)^Spec:\s*(.+)$`)

func ResolvePullRequestPhase(prLabels []string) PullRequestPhase {
	return ResolvePullRequestPhaseForNamespace(prLabels, labels.DefaultNamespace())
}

func ResolvePullRequestPhaseForNamespace(prLabels []string, namespace labels.Namespace) PullRequestPhase {
	if labels.Has(prLabels, namespace.SpecReviewing()) {
		return PhaseSpec
	}
	return PhaseImplementation
}

func ParseSpecPathFromPullRequestBody(body string) string {
	matches := pathPattern.FindStringSubmatch(body)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func CountUnresolvedReviewThreads(comments []map[string]any) int {
	count := 0
	for _, comment := range comments {
		state, _ := comment["state"].(string)
		if state != "" {
			if !strings.EqualFold(state, "RESOLVED") {
				count++
			}
			continue
		}
		isResolved, _ := comment["isResolved"].(bool)
		if !isResolved {
			count++
		}
	}
	return count
}

func IsReviewClean(reviewDecision string, comments []map[string]any) bool {
	return CountUnresolvedReviewThreads(comments) == 0 && !strings.EqualFold(reviewDecision, "CHANGES_REQUESTED")
}
