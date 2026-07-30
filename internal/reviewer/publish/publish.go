// Package publish holds the reviewer's publishing authority for PR-facing
// review bodies: the clean-approval and criteria-failure comment rendering,
// the acceptance-criteria result formatting, and the hidden markers that
// comment deduplication keys on. It has no I/O — the reviewer package owns
// GitHub access and disclosure stamping, and calls into this package for
// what the published text says. Reviewer sibling of internal/fixer/publish
// (#336), under the decomposition tracked by issue #120.
package publish

import (
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/reviewer/criteria"
)

// Comment markers are dedup keys: postStampedPRCommentIfMissing scans PR
// comments for the exact string before posting, and comments already in
// the wild carry them. Changing a marker turns every future run into a
// duplicate poster.
const (
	CriteriaFailMarker          = "<!-- looper:reviewer:criteria-fail -->"
	AutoMergeRefusedMarker      = "<!-- looper:reviewer:automerge-refused -->"
	CriteriaVerificationHeading = "### Acceptance criteria verification"
)

// AuthorMention normalizes a login into an @mention, tolerating an already
// prefixed or blank login. A blank login renders as no mention at all.
func AuthorMention(login string) string {
	login = strings.TrimSpace(strings.TrimPrefix(login, "@"))
	if login == "" {
		return ""
	}
	return "@" + login
}

// CleanApprovalBody renders the approving review body: author mention and
// affirmation, an optional intro naming the verification path taken, the
// passing criteria under heading when one is given, and a warm closer.
func CleanApprovalBody(authorLogin string, heading string, results []criteria.CriterionResult, intro string) string {
	parts := []string{fmt.Sprintf("%s Thanks for the update — I reviewed the current PR head and it's ready to move forward.", AuthorMention(authorLogin))}
	if strings.TrimSpace(intro) != "" {
		parts = append(parts, intro)
	}
	if heading != "" {
		parts = append(parts, heading, CriteriaResults(results, true))
	}
	parts = append(parts, "Happy to see this tightened up — nice work.")
	return strings.Join(parts, "\n\n")
}

// CriteriaFailureBody renders the review body for a head whose acceptance
// criteria could not all be verified, ending with the dedup marker.
func CriteriaFailureBody(results []criteria.CriterionResult) string {
	return strings.Join([]string{"Acceptance criteria could not be fully verified for this PR head.", CriteriaVerificationHeading, CriteriaResults(results, false), CriteriaFailMarker}, "\n\n")
}

func CriteriaResults(results []criteria.CriterionResult, includeOnlyPass bool) string {
	if len(results) == 0 {
		if includeOnlyPass {
			return "- No explicit acceptance criteria were available to verify."
		}
		return "- No acceptance criteria results were recorded."
	}
	lines := make([]string, 0, len(results))
	for _, result := range results {
		if includeOnlyPass && result.Verdict != criteria.VerdictPass {
			continue
		}
		line := fmt.Sprintf("- **%s** — %s", result.Criterion, strings.ToUpper(string(result.Verdict)))
		if pointers := evidencePointers(result.Evidence); pointers != "" {
			line += " (" + pointers + ")"
		}
		if justification := strings.TrimSpace(result.Justification); justification != "" {
			line += ": " + justification
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 && includeOnlyPass {
		return "- No passing acceptance criteria were recorded."
	}
	return strings.Join(lines, "\n")
}

func evidencePointers(evidence []criteria.Evidence) string {
	parts := make([]string, 0, len(evidence))
	for _, entry := range evidence {
		if entry.FilePath == "" || entry.StartLine < 1 {
			continue
		}
		if entry.EndLine > entry.StartLine {
			parts = append(parts, fmt.Sprintf("%s:%d-%d", entry.FilePath, entry.StartLine, entry.EndLine))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%d", entry.FilePath, entry.StartLine))
	}
	return strings.Join(parts, ", ")
}
