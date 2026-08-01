package auditor

import (
	"testing"
	"time"
)

func TestAttributeUsesObservedSignalsAndRanksCandidates(t *testing.T) {
	now := time.Date(2026, time.July, 31, 10, 0, 0, 0, time.UTC)
	result := Attribute(FailureEvidence{ObservedAt: now, FailingPaths: []string{"internal/api/routes.go"}}, []MergeCandidate{
		{PRNumber: 10, MergedAt: now.Add(-2 * time.Minute), TouchedFiles: []string{"internal/api/routes.go"}},
		{PRNumber: 11, MergedAt: now.Add(-time.Minute), TouchedFiles: []string{"README.md"}},
	})
	if result.Confidence != ConfidenceHigh || result.Candidate == nil || result.Candidate.PRNumber != 10 {
		t.Fatalf("Attribute() = %#v, want high confidence PR 10", result)
	}
	if len(result.Ranked) != 2 || result.Ranked[0].Score != 2 || result.Ranked[1].Score != 0 {
		t.Fatalf("ranked = %#v, want overlap-first ranking", result.Ranked)
	}
}

func TestAttributeEscalatesTieAndPreexistingFailure(t *testing.T) {
	now := time.Date(2026, time.July, 31, 10, 0, 0, 0, time.UTC)
	tied := Attribute(FailureEvidence{ObservedAt: now, FailingPaths: []string{"a.go"}}, []MergeCandidate{
		{PRNumber: 10, MergedAt: now.Add(-2 * time.Minute), TouchedFiles: []string{"a.go"}},
		{PRNumber: 11, MergedAt: now.Add(-time.Minute), TouchedFiles: []string{"a.go"}},
	})
	if tied.Confidence != ConfidenceLow || tied.Reason != "multiple_candidates_have_equal_evidence" {
		t.Fatalf("tied attribution = %#v, want low-confidence escalation", tied)
	}
	preexisting := Attribute(FailureEvidence{ObservedAt: now, ExistedBeforeAuditWindow: true}, []MergeCandidate{{PRNumber: 12, MergedAt: now.Add(-time.Minute)}})
	if preexisting.Confidence != ConfidenceNone || preexisting.Candidate != nil || preexisting.Reason != "failure_precedes_audit_window" {
		t.Fatalf("preexisting attribution = %#v, want no attribution", preexisting)
	}
}
