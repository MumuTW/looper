package forge

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	ForgejoSummarySchemaVersion = 1

	ReviewerSummaryKind = "looper.forgejo.reviewer_summary"
	FixerSummaryKind    = "looper.forgejo.fixer_summary"

	ReviewerSummaryMarker = "looper:forgejo-reviewer-summary"
	FixerSummaryMarker    = "looper:forgejo-fixer-summary"
)

// ReviewItemID is a Reviewer-assigned stable identifier for one semantic review
// issue, unique within a single Pull Request's Reviewer Summary history.
//
// Identity is semantic, not positional. Reviewer reuses the same ReviewItemID
// for as long as the underlying issue is the same issue, across Review Rounds
// and across changes to the file, line, or wording it is reported against. A
// new ID is minted only when the issue is split, merged, or materially
// redefined; the prior IDs then appear in the new item's Supersedes.
//
// Consumers resolve items by ID and never by file, line number, or text
// similarity: a Fixer citing an ID is asserting "this is the same issue the
// Reviewer named", which is a claim only the ID can carry. Matching on
// location would silently re-open items whose code moved and silently merge
// items whose wording converged.
//
// The zero value means "no ID assigned yet" — the state of a finding an agent
// has just reported and Reviewer has not yet minted an ID for.
type ReviewItemID string

// Normalized returns the ID with surrounding whitespace removed. IDs arrive
// from agent JSON output and from summary comments parsed off the forge, both
// of which can carry padding.
//
// Consumers of a parsed summary do not need to call this: ParseReviewerSummary
// and ParseFixerSummary normalize before returning, so IDs reaching a consumer
// are already comparable and safe as map keys. It is exported for the agent
// output path, where a finding is normalized as it is validated.
func (id ReviewItemID) Normalized() ReviewItemID { return ReviewItemID(strings.TrimSpace(string(id))) }

// IsZero reports whether the ID is unassigned, treating whitespace-only as
// unassigned.
func (id ReviewItemID) IsZero() bool { return id.Normalized() == "" }

type ReviewItemStatus string

const (
	ReviewItemStatusOpen       ReviewItemStatus = "open"
	ReviewItemStatusResolved   ReviewItemStatus = "resolved"
	ReviewItemStatusSuperseded ReviewItemStatus = "superseded"
)

type FixerItemResult string

const (
	FixerItemResultFixed    FixerItemResult = "fixed"
	FixerItemResultDeclined FixerItemResult = "declined"
	FixerItemResultDeferred FixerItemResult = "deferred"
)

type EvidenceReachability string

const (
	EvidenceReachabilityUnknown     EvidenceReachability = ""
	EvidenceReachabilityVerified    EvidenceReachability = "verified"
	EvidenceReachabilityUnverified  EvidenceReachability = "unverified"
	EvidenceReachabilityUnreachable EvidenceReachability = "unreachable"
)

type ReviewerSummary struct {
	Kind               string       `json:"kind"`
	SchemaVersion      int          `json:"schema_version"`
	ReviewRoundID      int          `json:"review_round_id"`
	LatestFixerRoundID int          `json:"latest_fixer_round_id,omitempty"`
	Items              []ReviewItem `json:"items"`
}

type ReviewItem struct {
	ReviewItemID    ReviewItemID     `json:"review_item_id"`
	Status          ReviewItemStatus `json:"status"`
	Title           string           `json:"title"`
	Body            string           `json:"body"`
	Files           []string         `json:"files,omitempty"`
	Supersedes      []ReviewItemID   `json:"supersedes,omitempty"`
	SupersededBy    ReviewItemID     `json:"superseded_by,omitempty"`
	LastSeenRoundID int              `json:"last_seen_round_id"`
}

type FixerSummary struct {
	Kind                  string        `json:"kind"`
	SchemaVersion         int           `json:"schema_version"`
	FixRoundID            int           `json:"fix_round_id"`
	ConsumedReviewRoundID int           `json:"consumed_review_round_id"`
	ObservedHeadSHA       string        `json:"observed_head_sha,omitempty"`
	Evidence              FixerEvidence `json:"evidence,omitempty"`
	Results               []FixerResult `json:"results"`
}

type FixerEvidence struct {
	HeadSHA   string               `json:"head_sha,omitempty"`
	Reachable EvidenceReachability `json:"reachable,omitempty"`
}

type FixerResult struct {
	ReviewItemID ReviewItemID    `json:"review_item_id"`
	Result       FixerItemResult `json:"result"`
	Explanation  string          `json:"explanation"`
}

// normalizeIDs canonicalizes every ReviewItemID the summary carries. Parsing
// runs this before validation so the invariant holds for every consumer at
// once. Leaving it to each consumer is what let a padded ID in a summary
// comment miss the map lookup in the reviewer's summary rebuild: validation
// compared normalized IDs and passed, then the rebuild indexed by raw ID and
// reported the item as unknown.
func (s *ReviewerSummary) normalizeIDs() {
	for i := range s.Items {
		s.Items[i].ReviewItemID = s.Items[i].ReviewItemID.Normalized()
		s.Items[i].SupersededBy = s.Items[i].SupersededBy.Normalized()
		for j := range s.Items[i].Supersedes {
			s.Items[i].Supersedes[j] = s.Items[i].Supersedes[j].Normalized()
		}
	}
}

func (s *FixerSummary) normalizeIDs() {
	for i := range s.Results {
		s.Results[i].ReviewItemID = s.Results[i].ReviewItemID.Normalized()
	}
}

func NewReviewerSummary(reviewRoundID int, items []ReviewItem) ReviewerSummary {
	return ReviewerSummary{Kind: ReviewerSummaryKind, SchemaVersion: ForgejoSummarySchemaVersion, ReviewRoundID: reviewRoundID, Items: items}
}

func NewFixerSummary(fixRoundID, consumedReviewRoundID int, results []FixerResult) FixerSummary {
	return FixerSummary{Kind: FixerSummaryKind, SchemaVersion: ForgejoSummarySchemaVersion, FixRoundID: fixRoundID, ConsumedReviewRoundID: consumedReviewRoundID, Results: results}
}

func RenderReviewerSummary(summary ReviewerSummary) (string, error) {
	if err := ValidateReviewerSummary(summary); err != nil {
		return "", err
	}
	return renderSummaryMarker(ReviewerSummaryMarker, summary)
}

func RenderFixerSummary(summary FixerSummary) (string, error) {
	if err := ValidateFixerSummary(summary); err != nil {
		return "", err
	}
	return renderSummaryMarker(FixerSummaryMarker, summary)
}

func ParseReviewerSummary(body string) (ReviewerSummary, error) {
	payload, err := extractSummaryPayload(body, ReviewerSummaryMarker)
	if err != nil {
		return ReviewerSummary{}, err
	}
	var summary ReviewerSummary
	if err := json.Unmarshal([]byte(payload), &summary); err != nil {
		return ReviewerSummary{}, fmt.Errorf("parse reviewer summary JSON: %w", err)
	}
	summary.normalizeIDs()
	if err := ValidateReviewerSummary(summary); err != nil {
		return ReviewerSummary{}, err
	}
	return summary, nil
}

func ParseFixerSummary(body string) (FixerSummary, error) {
	payload, err := extractSummaryPayload(body, FixerSummaryMarker)
	if err != nil {
		return FixerSummary{}, err
	}
	var summary FixerSummary
	if err := json.Unmarshal([]byte(payload), &summary); err != nil {
		return FixerSummary{}, fmt.Errorf("parse fixer summary JSON: %w", err)
	}
	summary.normalizeIDs()
	if err := ValidateFixerSummary(summary); err != nil {
		return FixerSummary{}, err
	}
	return summary, nil
}

func ParseUniqueReviewerSummaryComment(comments []Comment) (Comment, ReviewerSummary, error) {
	comment, err := findUniqueSummaryComment(comments, ReviewerSummaryMarker)
	if err != nil {
		return Comment{}, ReviewerSummary{}, err
	}
	summary, err := ParseReviewerSummary(comment.Body)
	return comment, summary, err
}

func ParseUniqueFixerSummaryComment(comments []Comment) (Comment, FixerSummary, error) {
	comment, err := findUniqueSummaryComment(comments, FixerSummaryMarker)
	if err != nil {
		return Comment{}, FixerSummary{}, err
	}
	summary, err := ParseFixerSummary(comment.Body)
	return comment, summary, err
}

func ValidateReviewerSummary(summary ReviewerSummary) error {
	if summary.Kind != ReviewerSummaryKind {
		return fmt.Errorf("reviewer summary kind = %q, want %q", summary.Kind, ReviewerSummaryKind)
	}
	if summary.SchemaVersion != ForgejoSummarySchemaVersion {
		return fmt.Errorf("reviewer summary schema_version = %d, want %d", summary.SchemaVersion, ForgejoSummarySchemaVersion)
	}
	if summary.ReviewRoundID <= 0 {
		return fmt.Errorf("reviewer summary review_round_id must be positive")
	}
	if summary.LatestFixerRoundID < 0 {
		return fmt.Errorf("reviewer summary latest_fixer_round_id must not be negative")
	}
	seen := map[ReviewItemID]struct{}{}
	for i, item := range summary.Items {
		id := item.ReviewItemID.Normalized()
		if id == "" {
			return fmt.Errorf("reviewer summary item %d review_item_id is required", i)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("reviewer summary duplicate review_item_id %q", id)
		}
		seen[id] = struct{}{}
		switch item.Status {
		case ReviewItemStatusOpen, ReviewItemStatusResolved, ReviewItemStatusSuperseded:
		default:
			return fmt.Errorf("reviewer summary item %q status = %q", id, item.Status)
		}
		if strings.TrimSpace(item.Title) == "" {
			return fmt.Errorf("reviewer summary item %q title is required", id)
		}
		if strings.TrimSpace(item.Body) == "" {
			return fmt.Errorf("reviewer summary item %q body is required", id)
		}
		if item.LastSeenRoundID <= 0 {
			return fmt.Errorf("reviewer summary item %q last_seen_round_id must be positive", id)
		}
		if item.LastSeenRoundID > summary.ReviewRoundID {
			return fmt.Errorf("reviewer summary item %q last_seen_round_id exceeds review_round_id", id)
		}
		if item.Status == ReviewItemStatusSuperseded && item.SupersededBy.IsZero() {
			return fmt.Errorf("reviewer summary item %q superseded_by is required for superseded status", id)
		}
		if item.Status != ReviewItemStatusSuperseded && !item.SupersededBy.IsZero() {
			return fmt.Errorf("reviewer summary item %q superseded_by requires superseded status", id)
		}
	}
	for _, item := range summary.Items {
		for _, supersededID := range item.Supersedes {
			if supersededID.IsZero() {
				return fmt.Errorf("reviewer summary item %q supersedes contains empty review_item_id", item.ReviewItemID)
			}
			if _, exists := seen[supersededID.Normalized()]; !exists {
				return fmt.Errorf("reviewer summary item %q supersedes unknown review_item_id %q", item.ReviewItemID, supersededID)
			}
		}
		if item.SupersededBy != "" {
			if _, exists := seen[item.SupersededBy.Normalized()]; !exists {
				return fmt.Errorf("reviewer summary item %q superseded_by unknown review_item_id %q", item.ReviewItemID, item.SupersededBy)
			}
		}
	}
	return nil
}

func ValidateFixerSummary(summary FixerSummary) error {
	if summary.Kind != FixerSummaryKind {
		return fmt.Errorf("fixer summary kind = %q, want %q", summary.Kind, FixerSummaryKind)
	}
	if summary.SchemaVersion != ForgejoSummarySchemaVersion {
		return fmt.Errorf("fixer summary schema_version = %d, want %d", summary.SchemaVersion, ForgejoSummarySchemaVersion)
	}
	if summary.FixRoundID <= 0 {
		return fmt.Errorf("fixer summary fix_round_id must be positive")
	}
	if summary.ConsumedReviewRoundID <= 0 {
		return fmt.Errorf("fixer summary consumed_review_round_id must be positive")
	}
	if summary.Evidence.Reachable != EvidenceReachabilityUnknown && summary.Evidence.Reachable != EvidenceReachabilityVerified && summary.Evidence.Reachable != EvidenceReachabilityUnverified && summary.Evidence.Reachable != EvidenceReachabilityUnreachable {
		return fmt.Errorf("fixer summary evidence reachable = %q", summary.Evidence.Reachable)
	}
	seen := map[ReviewItemID]struct{}{}
	for i, result := range summary.Results {
		id := result.ReviewItemID.Normalized()
		if id == "" {
			return fmt.Errorf("fixer summary result %d review_item_id is required", i)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("fixer summary duplicate review_item_id %q", id)
		}
		seen[id] = struct{}{}
		switch result.Result {
		case FixerItemResultFixed, FixerItemResultDeclined, FixerItemResultDeferred:
		default:
			return fmt.Errorf("fixer summary result %q result = %q", id, result.Result)
		}
		if strings.TrimSpace(result.Explanation) == "" {
			return fmt.Errorf("fixer summary result %q explanation is required", id)
		}
	}
	return nil
}

func ValidateFixerResultsForReviewerSummary(reviewer ReviewerSummary, fixer FixerSummary) error {
	if err := ValidateReviewerSummary(reviewer); err != nil {
		return err
	}
	if err := ValidateFixerSummary(fixer); err != nil {
		return err
	}
	if fixer.ConsumedReviewRoundID != reviewer.ReviewRoundID {
		return fmt.Errorf("fixer summary consumed_review_round_id = %d, want reviewer review_round_id %d", fixer.ConsumedReviewRoundID, reviewer.ReviewRoundID)
	}
	open := map[ReviewItemID]struct{}{}
	for _, item := range reviewer.Items {
		if item.Status == ReviewItemStatusOpen {
			open[item.ReviewItemID] = struct{}{}
		}
	}
	seen := map[ReviewItemID]struct{}{}
	for _, result := range fixer.Results {
		if _, exists := open[result.ReviewItemID]; !exists {
			return fmt.Errorf("fixer summary result for unknown or non-open review_item_id %q", result.ReviewItemID)
		}
		seen[result.ReviewItemID] = struct{}{}
	}
	for id := range open {
		if _, exists := seen[id]; !exists {
			return fmt.Errorf("fixer summary missing result for open review_item_id %q", id)
		}
	}
	return nil
}

func renderSummaryMarker(marker string, summary any) (string, error) {
	payload, err := json.Marshal(summary)
	if err != nil {
		return "", fmt.Errorf("render %s JSON: %w", marker, err)
	}
	return fmt.Sprintf("<!-- %s %s -->", marker, payload), nil
}

func extractSummaryPayload(body, marker string) (string, error) {
	prefix := "<!-- " + marker
	if strings.Count(body, prefix) == 0 {
		return "", fmt.Errorf("%s marker is missing", marker)
	}
	if strings.Count(body, prefix) > 1 {
		return "", fmt.Errorf("%s marker is duplicated", marker)
	}
	start := strings.Index(body, prefix)
	if start < 0 {
		return "", fmt.Errorf("%s marker is missing", marker)
	}
	payloadStart := start + len(prefix)
	end := strings.Index(body[payloadStart:], "-->")
	if end < 0 {
		return "", fmt.Errorf("%s marker is unterminated", marker)
	}
	payload := strings.TrimSpace(body[payloadStart : payloadStart+end])
	if payload == "" {
		return "", fmt.Errorf("%s marker JSON is missing", marker)
	}
	return payload, nil
}

func findUniqueSummaryComment(comments []Comment, marker string) (Comment, error) {
	prefix := "<!-- " + marker
	found := Comment{}
	count := 0
	for _, comment := range comments {
		markerCount := strings.Count(comment.Body, prefix)
		if markerCount == 0 {
			continue
		}
		count += markerCount
		if count == markerCount {
			found = comment
		}
	}
	if count == 0 {
		return Comment{}, fmt.Errorf("%s comment is missing", marker)
	}
	if count > 1 {
		return Comment{}, fmt.Errorf("%s comment is duplicated", marker)
	}
	return found, nil
}
