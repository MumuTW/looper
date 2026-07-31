package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// PullRequest is the subset of `gh pr list --json` this audit consumes.
type PullRequest struct {
	Number     int64     `json:"number"`
	Title      string    `json:"title"`
	Author     Author    `json:"author"`
	Additions  *int64    `json:"additions"`
	Deletions  *int64    `json:"deletions"`
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
	// VerdictReviewed: at least one submitted review with commit provenance
	// exists for the merged head.
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
// actually happened. PENDING drafts were never submitted, and DISMISSED
// reviews were explicitly invalidated, so neither counts.
var submittedReviewStates = map[string]bool{
	"APPROVED":          true,
	"CHANGES_REQUESTED": true,
	"COMMENTED":         true,
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
	if f.PR.Additions == nil || f.PR.Deletions == nil {
		return 0
	}
	return *f.PR.Additions + *f.PR.Deletions
}

// Classify audits one merged PR. A review counts only when GitHub recorded
// both the merged head OID and the review's commit OID and they match. Missing
// provenance cannot demonstrate coverage, so it never degrades to reviewed;
// request headRefOid and reviews in the gh query to keep the audit sound.
func Classify(pr PullRequest) Finding {
	headReviewers := map[string]bool{}
	staleReviewers := map[string]bool{}
	prAuthor := strings.TrimSpace(pr.Author.Login)
	for _, review := range pr.Reviews {
		if !submittedReviewStates[strings.ToUpper(strings.TrimSpace(review.State))] {
			continue
		}
		login := strings.TrimSpace(review.Author.Login)
		// Review coverage must come from an identifiable account other than the
		// pull-request author. Without both identities GitHub's record cannot
		// demonstrate independent scrutiny, so the gate fails closed.
		if prAuthor == "" || login == "" || strings.EqualFold(login, prAuthor) {
			continue
		}
		if pr.HeadRefOID != "" && review.Commit.OID != "" && review.Commit.OID == pr.HeadRefOID {
			headReviewers[login] = true
		} else if pr.HeadRefOID != "" && review.Commit.OID != "" {
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
// every merge without a verified head review at or above that many changed
// lines; 0 disables the flag. It returns the report text and whether any
// merge met the threshold.
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
		if err := validateChangedLineCounts(pr); err != nil {
			return "", false, err
		}
		finding := Classify(pr)
		counts[finding.Verdict]++
		line := fmt.Sprintf("#%d\t%s\t+%d/-%d", pr.Number, finding.Verdict, *pr.Additions, *pr.Deletions)
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

func validateChangedLineCounts(pr PullRequest) error {
	switch {
	case pr.Additions == nil && pr.Deletions == nil:
		return fmt.Errorf("PR #%d is missing additions and deletions; request both changed-line counts before applying the audit", pr.Number)
	case pr.Additions == nil:
		return fmt.Errorf("PR #%d is missing additions; request both changed-line counts before applying the audit", pr.Number)
	case pr.Deletions == nil:
		return fmt.Errorf("PR #%d is missing deletions; request both changed-line counts before applying the audit", pr.Number)
	case *pr.Additions < 0:
		return fmt.Errorf("PR #%d has negative additions %d; changed-line counts must be non-negative", pr.Number, *pr.Additions)
	case *pr.Deletions < 0:
		return fmt.Errorf("PR #%d has negative deletions %d; changed-line counts must be non-negative", pr.Number, *pr.Deletions)
	case *pr.Additions > math.MaxInt64-*pr.Deletions:
		return fmt.Errorf("PR #%d changed-line counts overflow int64; additions %d plus deletions %d exceeds the supported range", pr.Number, *pr.Additions, *pr.Deletions)
	default:
		return nil
	}
}
