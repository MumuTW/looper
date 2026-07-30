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
// A detector is a marker *and* the account that posts it. The marker alone is
// body text, and body text is not authority: anyone quoting the notice — or
// typing it — would otherwise be recorded as having refused to review a change
// they never had any part in. This is the same identity rule the owned verdict
// comment already uses, where marker plus author is the comment's identity.
//
// This is a list of one rather than a single test because "Review limit
// reached" is what CodeRabbit posts today. It is the current signal from one
// reviewer, not the definition of refusal, and the recorded Detector says which
// signal matched so a later addition does not retroactively reinterpret rows
// already written.
type refusalDetector struct {
	ID string
	// Account is the reviewer's login as GitHub spells it, with no `[bot]`
	// suffix: the CodeRabbit account is `coderabbitai`, and REST decorates it to
	// `coderabbitai[bot]` while gh's GraphQL projections do not. Matching strips
	// the suffix rather than testing for it, because the suffix is a property of
	// which API answered, not of the account.
	Account string
	Marker  string
}

var refusalDetectors = []refusalDetector{
	{ID: "coderabbit_review_limit", Account: "coderabbitai", Marker: "Review limit reached"},
}

// RefusalCommentMarkers are the body markers a refusal can carry. They are
// exported so the comment read can be projected down to matching comments before
// it crosses the shell capture boundary — the detector still decides, and it
// decides on the author as well as the body.
func RefusalCommentMarkers() []string {
	markers := make([]string, 0, len(refusalDetectors))
	for _, detector := range refusalDetectors {
		markers = append(markers, detector.Marker)
	}
	return markers
}

// accountLogin normalises a login for comparison against a detector's account:
// case-insensitive, and without the `[bot]` suffix REST appends to app accounts.
func accountLogin(login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	return strings.TrimSuffix(login, "[bot]")
}

// detectRefusal reports which detector matched a comment, requiring the marker,
// the account, and GitHub's own classification of that account as a bot.
//
// The bot classification is what makes the account check mean anything: a login
// is a string a human can also hold, especially on an enterprise install, and
// the account type is the forge's own answer rather than an inference from the
// name.
func detectRefusal(comment githubinfra.CommentInfo) (string, bool) {
	login := accountLogin(comment.Author)
	for _, detector := range refusalDetectors {
		if login != detector.Account || !comment.IsBot {
			continue
		}
		if strings.Contains(comment.Body, detector.Marker) {
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

// reviewsObservedFrom records the submitted reviews and, from them alone, the
// status they settle.
//
// Completed reviews are the whole of the `reviewed` answer: comments only ever
// distinguish `refused` from `absent`, and only when nothing was reviewed. So
// this is decidable without them, which is what lets a comment read that fails
// leave a known `reviewed` standing instead of collapsing it to `unknown`.
//
// Order is the forge's order, which is chronological and stable, so the record
// reads as the sequence of what happened rather than a re-sorted summary of it.
func reviewsObservedFrom(reviews []githubinfra.ReviewSummary) ReviewProvenance {
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
	if provenance.CompletedReviews > 0 {
		provenance.Status = ReviewProvenanceReviewed
	} else {
		provenance.Status = ReviewProvenanceAbsent
	}
	return provenance
}

// withRefusalsFrom adds the refusals the discussion carries. It can only move
// `absent` to `refused`: a pull request somebody reviewed stays reviewed, and
// the refusal is still recorded alongside because "a bot declined and a human
// reviewed anyway" is the thing the operator wants to be able to read.
func withRefusalsFrom(provenance ReviewProvenance, comments []githubinfra.CommentInfo) ReviewProvenance {
	for _, comment := range comments {
		detector, ok := detectRefusal(comment)
		if !ok {
			continue
		}
		provenance.Refusals = append(provenance.Refusals, ReviewRefusal{
			Login: strings.TrimSpace(comment.Author), Detector: detector, CommentID: comment.ID,
		})
	}
	if provenance.Status == ReviewProvenanceAbsent && len(provenance.Refusals) > 0 {
		provenance.Status = ReviewProvenanceRefused
	}
	return provenance
}
