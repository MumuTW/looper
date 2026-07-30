package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PullRequest is the subset of `gh pr list --json` this audit consumes.
type PullRequest struct {
	Number    int64     `json:"number"`
	Title     string    `json:"title"`
	Additions int64     `json:"additions"`
	Deletions int64     `json:"deletions"`
	MergedAt  string    `json:"mergedAt"`
	Reviews   []Review  `json:"reviews"`
	Comments  []Comment `json:"comments"`
}

type Review struct {
	Author Author `json:"author"`
	State  string `json:"state"`
}

type Comment struct {
	Author Author `json:"author"`
	Body   string `json:"body"`
}

type Author struct {
	Login string `json:"login"`
}

// Verdict classifies one merged PR's review coverage.
type Verdict string

const (
	// VerdictReviewed: at least one review was actually submitted.
	VerdictReviewed Verdict = "reviewed"
	// VerdictRefused: no review was submitted, and a reviewer's rate-limit
	// notice is present — the gate refused and left a comment that reads
	// like participation.
	VerdictRefused Verdict = "rate-limit-refused"
	// VerdictUnreviewed: no review and no recorded refusal at all.
	VerdictUnreviewed Verdict = "unreviewed"
)

// refusalMarkers identify reviewer refusals posted as ordinary comments.
// CodeRabbit's rate-limit notice is the shape observed in #367.
var refusalMarkers = []string{
	"Review limit reached",
	"rate limited by coderabbit",
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

// Classify audits one merged PR.
func Classify(pr PullRequest) Finding {
	reviewers := map[string]bool{}
	for _, review := range pr.Reviews {
		login := strings.TrimSpace(review.Author.Login)
		if login == "" {
			login = "(unknown)"
		}
		reviewers[login] = true
	}
	if len(reviewers) > 0 {
		names := make([]string, 0, len(reviewers))
		for name := range reviewers {
			names = append(names, name)
		}
		sort.Strings(names)
		return Finding{PR: pr, Verdict: VerdictReviewed, Reviewers: names}
	}
	for _, comment := range pr.Comments {
		for _, marker := range refusalMarkers {
			if strings.Contains(strings.ToLower(comment.Body), strings.ToLower(marker)) {
				return Finding{PR: pr, Verdict: VerdictRefused}
			}
		}
	}
	return Finding{PR: pr, Verdict: VerdictUnreviewed}
}

// Audit classifies every PR and renders the report. largeThreshold flags
// unreviewed or refused merges at or above that many changed lines; 0
// disables the flag. It returns the report text and whether any
// unreviewed-or-refused merge met the threshold.
func Audit(input []byte, largeThreshold int64) (string, bool, error) {
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
	b.WriteString(fmt.Sprintf("\ntotal=%d reviewed=%d rate-limit-refused=%d unreviewed=%d\n",
		len(prs), counts[VerdictReviewed], counts[VerdictRefused], counts[VerdictUnreviewed]))
	return b.String(), flagged, nil
}
