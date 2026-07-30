package gatekeeper

import (
	"strings"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// ReviewProvenanceStatus is the distinction the Gate report could not previously
// draw.
//
// Evidence.ReviewDecision is GitHub's answer to "does branch protection still
// want a review", and it is the empty string on a repository that requires none.
// The gate's default arm then only records a reason when protection asks for
// reviews, so a pull request nobody looked at and a pull request where review was
// never required produced byte-identical evidence. That is why ten unreviewed
// merges in one day left no trace in the durable record.
//
// Review provenance is the separate observation: not "was review required" but
// "did review happen", answered independently of protection.
type ReviewProvenanceStatus string

const (
	// ReviewProvenanceUnobserved is the zero value. It means this report never
	// looked — it returned before the observation, or it was written by a build
	// that predates review provenance. It is spelled as the empty string so old
	// reports decode into it rather than into a claim they never made.
	ReviewProvenanceUnobserved ReviewProvenanceStatus = ""
	// ReviewProvenanceReviewed means at least one reviewer submitted a review.
	ReviewProvenanceReviewed ReviewProvenanceStatus = "reviewed"
	// ReviewProvenanceRefused means nobody submitted a review and at least one
	// reviewer said in the discussion that it would not. A rate-limit notice is a
	// refusal: the reviewer posted instead of reviewing.
	ReviewProvenanceRefused ReviewProvenanceStatus = "refused"
	// ReviewProvenanceAbsent means nobody submitted a review and nobody refused.
	// Nothing looked at this pull request at all.
	ReviewProvenanceAbsent ReviewProvenanceStatus = "absent"
	// ReviewProvenanceUnknown means the observation was attempted and the forge
	// did not answer. It is distinct from absent on purpose: recording a failed
	// read as "nobody reviewed this" would reintroduce, one level down, the exact
	// confusion this field exists to remove.
	ReviewProvenanceUnknown ReviewProvenanceStatus = "unknown"
)

// ReviewerObservation is one submitted review, attributed.
type ReviewerObservation struct {
	Login string `json:"login"`
	IsBot bool   `json:"isBot"`
	State string `json:"state"`
}

// ReviewRefusal is one reviewer declining to review, attributed. Detector names
// which signal matched, so a record written today stays readable when the set of
// signals changes.
type ReviewRefusal struct {
	Login     string `json:"login"`
	Detector  string `json:"detector"`
	CommentID int64  `json:"commentId,omitempty"`
}

// ReviewProvenance is what the Gate report says about who actually reviewed a
// pull request, independent of whether anyone was required to.
type ReviewProvenance struct {
	Status           ReviewProvenanceStatus `json:"status,omitempty"`
	CompletedReviews int                    `json:"completedReviews"`
	Reviewers        []ReviewerObservation  `json:"reviewers"`
	Refusals         []ReviewRefusal        `json:"refusals"`
}

// refusalDetector recognises a reviewer stating, in the pull request discussion,
// that it is not reviewing this pull request.
//
// This is a list of one rather than a single string test because "Review limit
// reached" is what CodeRabbit posts today. It is the current signal from one
// reviewer, not the definition of refusal, and the recorded Detector says which
// signal matched so a later addition does not retroactively reinterpret rows
// already written.
type refusalDetector struct {
	ID     string
	Marker string
}

var refusalDetectors = []refusalDetector{
	{ID: "coderabbit_review_limit", Marker: "Review limit reached"},
}

func detectRefusal(body string) (string, bool) {
	for _, detector := range refusalDetectors {
		if strings.Contains(body, detector.Marker) {
			return detector.ID, true
		}
	}
	return "", false
}

// isCompletedReviewState reports whether GitHub records this review as submitted.
// PENDING is a draft the reviewer never sent and DISMISSED is one the repository
// withdrew; neither is scrutiny anything received.
func isCompletedReviewState(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "APPROVED", "CHANGES_REQUESTED", "COMMENTED":
		return true
	default:
		return false
	}
}

// reviewProvenanceFrom builds the observation from the forge's own records: the
// reviews GitHub stores against the pull request, and the discussion comments
// that carry a refusal.
//
// Order is the forge's order, which is chronological and stable, so the record
// reads as the sequence of what happened rather than a re-sorted summary of it.
func reviewProvenanceFrom(reviews []githubinfra.ReviewSummary, comments []githubinfra.CommentInfo) ReviewProvenance {
	provenance := ReviewProvenance{
		Reviewers: make([]ReviewerObservation, 0, len(reviews)),
		Refusals:  make([]ReviewRefusal, 0),
	}
	for _, review := range reviews {
		state := strings.ToUpper(strings.TrimSpace(review.State))
		login := strings.TrimSpace(review.Author)
		if login == "" || !isCompletedReviewState(state) {
			continue
		}
		provenance.CompletedReviews++
		provenance.Reviewers = append(provenance.Reviewers, ReviewerObservation{Login: login, IsBot: review.IsBot, State: state})
	}
	for _, comment := range comments {
		detector, ok := detectRefusal(comment.Body)
		if !ok {
			continue
		}
		provenance.Refusals = append(provenance.Refusals, ReviewRefusal{
			Login: strings.TrimSpace(comment.Author), Detector: detector, CommentID: comment.ID,
		})
	}
	switch {
	case provenance.CompletedReviews > 0:
		provenance.Status = ReviewProvenanceReviewed
	case len(provenance.Refusals) > 0:
		provenance.Status = ReviewProvenanceRefused
	default:
		provenance.Status = ReviewProvenanceAbsent
	}
	return provenance
}
