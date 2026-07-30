package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PullRequest is the subset of `gh pr list --json` this audit consumes.
type PullRequest struct {
	Number     int64     `json:"number"`
	Title      string    `json:"title"`
	Additions  int64     `json:"additions"`
	Deletions  int64     `json:"deletions"`
	MergedAt   string    `json:"mergedAt"`
	HeadRefOID string    `json:"headRefOid"`
	Reviews    []Review  `json:"reviews"`
	Comments   []Comment `json:"comments"`
}

type Review struct {
	Author Author `json:"author"`
	State  string `json:"state"`
	Commit Commit `json:"commit"`
}

type Commit struct {
	OID string `json:"oid"`
}

type Comment struct {
	Author Author `json:"author"`
	Body   string `json:"body"`
}

type Author struct {
	Login string `json:"login"`
}

// Verdict classifies one merged PR's review coverage. The authority for
// every verdict is GitHub's own recorded state — submitted review objects
// and the trusted reviewer account's refusal notice — never inference
// over discussion content.
type Verdict string

const (
	// VerdictReviewed: at least one submitted review exists for the merged
	// head (or head binding is unavailable in the input; see Classify).
	VerdictReviewed Verdict = "reviewed"
	// VerdictStaleReviewed: reviews were submitted, but none for the head
	// that merged — later commits landed unreviewed.
	VerdictStaleReviewed Verdict = "stale-reviewed"
	// VerdictRefused: no review was submitted, and the trusted reviewer
	// account posted its rate-limit notice — the gate refused and left a
	// comment that reads like participation.
	VerdictRefused Verdict = "rate-limit-refused"
	// VerdictUnreviewed: no review and no recorded refusal at all.
	VerdictUnreviewed Verdict = "unreviewed"
)

// submittedReviewStates are the review states that represent scrutiny that
// actually happened. PENDING drafts were never submitted and do not count.
var submittedReviewStates = map[string]bool{
	"APPROVED":          true,
	"CHANGES_REQUESTED": true,
	"COMMENTED":         true,
	"DISMISSED":         true,
}

// refusalMarkers identify reviewer refusals posted as ordinary comments.
// CodeRabbit's rate-limit notice is the shape observed in #367.
var refusalMarkers = []string{
	"Review limit reached",
	"rate limited by coderabbit",
}

// trustedRefusalAuthors are the reviewer accounts whose comments may carry
// a refusal notice. A participant quoting the phrase is not a refusal.
var trustedRefusalAuthors = map[string]bool{
	"coderabbitai":      true,
	"coderabbitai[bot]": true,
}

// Finding is the audit result for one PR.
type Finding struct {
	PR        PullRequest
	Verdict   Verdict
	Reviewers []string
}

// ChangedLines is the size measure the audit reports and thresholds on.
func (f Finding) ChangedLines() int64 {
	return f.PR.Additions + f.PR.Deletions
}

// Classify audits one merged PR. When the input carries head and review
// commit OIDs, a review counts for the merged head only if it reviewed
// that exact commit; inputs without those fields degrade to counting any
// submitted review, which the report cannot distinguish — request
// headRefOid and reviews in the gh query to keep head binding active.
func Classify(pr PullRequest) Finding {
	headReviewers := map[string]bool{}
	staleReviewers := map[string]bool{}
	for _, review := range pr.Reviews {
		if !submittedReviewStates[strings.ToUpper(strings.TrimSpace(review.State))] {
			continue
		}
		login := strings.TrimSpace(review.Author.Login)
		if login == "" {
			login = "(unknown)"
		}
		if pr.HeadRefOID == "" || review.Commit.OID == "" || review.Commit.OID == pr.HeadRefOID {
			headReviewers[login] = true
		} else {
			staleReviewers[login] = true
		}
	}
	if len(headReviewers) > 0 {
		return Finding{PR: pr, Verdict: VerdictReviewed, Reviewers: sortedNames(headReviewers)}
	}
	if len(staleReviewers) > 0 {
		return Finding{PR: pr, Verdict: VerdictStaleReviewed, Reviewers: sortedNames(staleReviewers)}
	}
	for _, comment := range pr.Comments {
		if !trustedRefusalAuthors[strings.ToLower(strings.TrimSpace(comment.Author.Login))] {
			continue
		}
		for _, marker := range refusalMarkers {
			if strings.Contains(strings.ToLower(comment.Body), strings.ToLower(marker)) {
				return Finding{PR: pr, Verdict: VerdictRefused}
			}
		}
	}
	return Finding{PR: pr, Verdict: VerdictUnreviewed}
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Audit classifies every PR and renders the report. largeThreshold flags
// unreviewed or refused merges at or above that many changed lines; 0
// disables the flag. It returns the report text and whether any
// unreviewed-or-refused merge met the threshold.
func Audit(input []byte, largeThreshold int64) (string, bool, error) {
	if largeThreshold < 0 {
		return "", false, fmt.Errorf("-large must be >= 0, got %d: a negative threshold would silently disable the gate", largeThreshold)
	}
	var prs []PullRequest
	if err := json.Unmarshal(input, &prs); err != nil {
		return "", false, fmt.Errorf("parse gh pr list JSON: %w", err)
	}
	var b strings.Builder
	counts := map[Verdict]int{}
	flagged := false
	for _, pr := range prs {
		finding := Classify(pr)
		counts[finding.Verdict]++
		line := fmt.Sprintf("#%d\t%s\t+%d/-%d", pr.Number, finding.Verdict, pr.Additions, pr.Deletions)
		if len(finding.Reviewers) > 0 {
			line += "\t[" + strings.Join(finding.Reviewers, ",") + "]"
		}
		if largeThreshold > 0 && finding.Verdict != VerdictReviewed && finding.ChangedLines() >= largeThreshold {
			line += "\tLARGE-UNREVIEWED"
			flagged = true
		}
		line += "\t" + pr.Title
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("\ntotal=%d reviewed=%d stale-reviewed=%d rate-limit-refused=%d unreviewed=%d\n",
		len(prs), counts[VerdictReviewed], counts[VerdictStaleReviewed], counts[VerdictRefused], counts[VerdictUnreviewed]))
	return b.String(), flagged, nil
}
