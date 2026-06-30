package forge

import (
	"strings"
	"testing"
)

func TestReviewerSummaryRenderParseAndUniqueComment(t *testing.T) {
	t.Parallel()

	summary := NewReviewerSummary(3, []ReviewItem{
		{ReviewItemID: "R-001", Status: ReviewItemStatusOpen, Title: "Fix parsing", Body: "Reject invalid embedded JSON.", Files: []string{"internal/forge/summary_protocol.go"}, LastSeenRoundID: 3},
		{ReviewItemID: "R-002", Status: ReviewItemStatusSuperseded, Title: "Old split issue", Body: "Old wording.", SupersededBy: "R-001", LastSeenRoundID: 2},
	})
	summary.LatestFixerRoundID = 2

	body, err := RenderReviewerSummary(summary)
	if err != nil {
		t.Fatalf("RenderReviewerSummary() error = %v", err)
	}
	if !strings.HasPrefix(body, "<!-- looper:forgejo-reviewer-summary ") || !strings.HasSuffix(body, " -->") {
		t.Fatalf("RenderReviewerSummary() = %q, want looper marker comment", body)
	}
	parsed, err := ParseReviewerSummary("Human view\n\n" + body + "\n")
	if err != nil {
		t.Fatalf("ParseReviewerSummary() error = %v", err)
	}
	if parsed.Kind != ReviewerSummaryKind || parsed.SchemaVersion != 1 || parsed.ReviewRoundID != 3 || len(parsed.Items) != 2 || parsed.Items[0].ReviewItemID != "R-001" {
		t.Fatalf("ParseReviewerSummary() = %#v", parsed)
	}

	comment, parsed, err := ParseUniqueReviewerSummaryComment([]Comment{{ID: 10, Body: "other"}, {ID: 11, Body: body}})
	if err != nil {
		t.Fatalf("ParseUniqueReviewerSummaryComment() error = %v", err)
	}
	if comment.ID != 11 || parsed.Items[1].SupersededBy != "R-001" {
		t.Fatalf("ParseUniqueReviewerSummaryComment() = %#v, %#v", comment, parsed)
	}
}

func TestReviewerSummaryValidationFailures(t *testing.T) {
	t.Parallel()

	valid := NewReviewerSummary(2, []ReviewItem{{ReviewItemID: "R-001", Status: ReviewItemStatusOpen, Title: "Title", Body: "Body", LastSeenRoundID: 2}})

	tests := []struct {
		name   string
		mutate func(*ReviewerSummary)
		want   string
	}{
		{name: "kind", mutate: func(s *ReviewerSummary) { s.Kind = "wrong" }, want: "kind"},
		{name: "schema", mutate: func(s *ReviewerSummary) { s.SchemaVersion = 2 }, want: "schema_version"},
		{name: "round", mutate: func(s *ReviewerSummary) { s.ReviewRoundID = 0 }, want: "review_round_id"},
		{name: "duplicate id", mutate: func(s *ReviewerSummary) { s.Items = append(s.Items, s.Items[0]) }, want: "duplicate review_item_id"},
		{name: "status", mutate: func(s *ReviewerSummary) { s.Items[0].Status = "ignored" }, want: "status"},
		{name: "last seen", mutate: func(s *ReviewerSummary) { s.Items[0].LastSeenRoundID = 3 }, want: "last_seen_round_id exceeds"},
		{name: "superseded link required", mutate: func(s *ReviewerSummary) { s.Items[0].Status = ReviewItemStatusSuperseded }, want: "superseded_by is required"},
		{name: "unknown superseded link", mutate: func(s *ReviewerSummary) {
			s.Items[0].Status = ReviewItemStatusSuperseded
			s.Items[0].SupersededBy = "R-404"
		}, want: "unknown review_item_id"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			summary := valid
			summary.Items = append([]ReviewItem(nil), valid.Items...)
			test.mutate(&summary)
			err := ValidateReviewerSummary(summary)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateReviewerSummary() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFixerSummaryRenderParseAndValidateAgainstReviewer(t *testing.T) {
	t.Parallel()

	reviewer := NewReviewerSummary(4, []ReviewItem{
		{ReviewItemID: "R-001", Status: ReviewItemStatusOpen, Title: "Open", Body: "Fix it.", LastSeenRoundID: 4},
		{ReviewItemID: "R-002", Status: ReviewItemStatusResolved, Title: "Done", Body: "Already fixed.", LastSeenRoundID: 3},
	})
	fixer := NewFixerSummary(2, 4, []FixerResult{{ReviewItemID: "R-001", Result: FixerItemResultFixed, Explanation: "Added strict parsing."}})
	fixer.ObservedHeadSHA = "abc123"
	fixer.Evidence = FixerEvidence{HeadSHA: "def456", Reachable: EvidenceReachabilityVerified}

	body, err := RenderFixerSummary(fixer)
	if err != nil {
		t.Fatalf("RenderFixerSummary() error = %v", err)
	}
	parsed, err := ParseFixerSummary("Visible audit\n" + body)
	if err != nil {
		t.Fatalf("ParseFixerSummary() error = %v", err)
	}
	if err := ValidateFixerResultsForReviewerSummary(reviewer, parsed); err != nil {
		t.Fatalf("ValidateFixerResultsForReviewerSummary() error = %v", err)
	}
}

func TestFixerSummaryValidationFailures(t *testing.T) {
	t.Parallel()

	valid := NewFixerSummary(1, 2, []FixerResult{{ReviewItemID: "R-001", Result: FixerItemResultDeferred, Explanation: "Needs human input."}})

	tests := []struct {
		name   string
		mutate func(*FixerSummary)
		want   string
	}{
		{name: "kind", mutate: func(s *FixerSummary) { s.Kind = "wrong" }, want: "kind"},
		{name: "schema", mutate: func(s *FixerSummary) { s.SchemaVersion = 2 }, want: "schema_version"},
		{name: "fix round", mutate: func(s *FixerSummary) { s.FixRoundID = 0 }, want: "fix_round_id"},
		{name: "review round", mutate: func(s *FixerSummary) { s.ConsumedReviewRoundID = 0 }, want: "consumed_review_round_id"},
		{name: "duplicate result", mutate: func(s *FixerSummary) { s.Results = append(s.Results, s.Results[0]) }, want: "duplicate review_item_id"},
		{name: "result enum", mutate: func(s *FixerSummary) { s.Results[0].Result = "maybe" }, want: "result"},
		{name: "evidence enum", mutate: func(s *FixerSummary) { s.Evidence.Reachable = "maybe" }, want: "reachable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			summary := valid
			summary.Results = append([]FixerResult(nil), valid.Results...)
			test.mutate(&summary)
			err := ValidateFixerSummary(summary)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateFixerSummary() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSummaryMarkerParsingFailures(t *testing.T) {
	t.Parallel()

	summary := NewReviewerSummary(1, []ReviewItem{{ReviewItemID: "R-001", Status: ReviewItemStatusOpen, Title: "Title", Body: "Body", LastSeenRoundID: 1}})
	body, err := RenderReviewerSummary(summary)
	if err != nil {
		t.Fatalf("RenderReviewerSummary() error = %v", err)
	}

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "missing", body: "no marker", want: "missing"},
		{name: "duplicated", body: body + "\n" + body, want: "duplicated"},
		{name: "missing json", body: "<!-- looper:forgejo-reviewer-summary -->", want: "JSON is missing"},
		{name: "invalid json", body: "<!-- looper:forgejo-reviewer-summary {nope} -->", want: "parse reviewer summary JSON"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseReviewerSummary(test.body)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseReviewerSummary() error = %v, want %q", err, test.want)
			}
		})
	}

	_, _, err = ParseUniqueReviewerSummaryComment([]Comment{{ID: 1, Body: body}, {ID: 2, Body: body}})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("ParseUniqueReviewerSummaryComment duplicate error = %v", err)
	}
}

func TestValidateFixerResultsForReviewerSummaryFailures(t *testing.T) {
	t.Parallel()

	reviewer := NewReviewerSummary(7, []ReviewItem{
		{ReviewItemID: "R-001", Status: ReviewItemStatusOpen, Title: "One", Body: "Body", LastSeenRoundID: 7},
		{ReviewItemID: "R-002", Status: ReviewItemStatusOpen, Title: "Two", Body: "Body", LastSeenRoundID: 7},
		{ReviewItemID: "R-003", Status: ReviewItemStatusResolved, Title: "Three", Body: "Body", LastSeenRoundID: 6},
	})

	tests := []struct {
		name  string
		fixer FixerSummary
		want  string
	}{
		{name: "wrong round", fixer: NewFixerSummary(1, 6, []FixerResult{{ReviewItemID: "R-001", Result: FixerItemResultFixed, Explanation: "done"}, {ReviewItemID: "R-002", Result: FixerItemResultDeclined, Explanation: "no"}}), want: "consumed_review_round_id"},
		{name: "unknown", fixer: NewFixerSummary(1, 7, []FixerResult{{ReviewItemID: "R-001", Result: FixerItemResultFixed, Explanation: "done"}, {ReviewItemID: "R-404", Result: FixerItemResultDeclined, Explanation: "no"}}), want: "unknown or non-open"},
		{name: "non open", fixer: NewFixerSummary(1, 7, []FixerResult{{ReviewItemID: "R-001", Result: FixerItemResultFixed, Explanation: "done"}, {ReviewItemID: "R-003", Result: FixerItemResultDeclined, Explanation: "no"}}), want: "unknown or non-open"},
		{name: "missing", fixer: NewFixerSummary(1, 7, []FixerResult{{ReviewItemID: "R-001", Result: FixerItemResultFixed, Explanation: "done"}}), want: "missing result"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateFixerResultsForReviewerSummary(reviewer, test.fixer)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateFixerResultsForReviewerSummary() error = %v, want %q", err, test.want)
			}
		})
	}
}
