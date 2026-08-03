// Package auditor holds the decision contracts for post-merge regression work.
package auditor

import (
	"path"
	"sort"
	"strings"
	"time"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

type Confidence string

const (
	ConfidenceNone Confidence = "none"
	ConfidenceLow  Confidence = "low"
	ConfidenceHigh Confidence = "high"
)

// MergeCandidate is one Looper-merged pull request in the configured audit
// window. Its touched files and reproduction evidence come from authoritative
// forge and reproduction records, not inference from an agent summary.
type MergeCandidate struct {
	ProjectID              string
	Repo                   string
	PRNumber               int64
	HeadSHA                string
	MergeCommitSHA         string
	SourceIssue            *githubinfra.IssueReference
	MergedAt               time.Time
	TouchedFiles           []string
	TouchedFilesAvailable  bool
	IntroducedReproduction bool
}

// FailureEvidence contains only observed failure facts collected before an
// auditor considers any action.
type FailureEvidence struct {
	ObservedAt                  time.Time
	FailingPaths                []string
	ExistedBeforeAuditWindow    bool
	BaselineKnown               bool
	FailingPathEvidenceComplete bool
}

type Attribution struct {
	Candidate  *MergeCandidate
	Confidence Confidence
	Reason     string
	Ranked     []RankedCandidate
}

type RankedCandidate struct {
	Candidate MergeCandidate
	Score     int
	Reasons   []string
}

// Attribute ranks all candidate merges using stated forge/reproduction signals.
// A pre-existing failure is never attributed. Ties and missing positive signals
// are deliberately low confidence, so callers must escalate rather than revert.
func Attribute(failure FailureEvidence, candidates []MergeCandidate) Attribution {
	if failure.ExistedBeforeAuditWindow {
		return Attribution{Confidence: ConfidenceNone, Reason: "failure_precedes_audit_window"}
	}
	if !failure.BaselineKnown {
		return Attribution{Confidence: ConfidenceNone, Reason: "clean_baseline_unavailable"}
	}
	if !failure.FailingPathEvidenceComplete {
		return Attribution{Confidence: ConfidenceNone, Reason: "failure_path_evidence_incomplete"}
	}
	for _, candidate := range candidates {
		if candidate.MergedAt.After(failure.ObservedAt) {
			continue
		}
		if !candidate.TouchedFilesAvailable {
			return Attribution{Confidence: ConfidenceNone, Reason: "merge_file_evidence_incomplete"}
		}
	}
	ranked := make([]RankedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.MergedAt.After(failure.ObservedAt) {
			continue
		}
		ranked = append(ranked, rankCandidate(failure, candidate))
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		if !ranked[i].Candidate.MergedAt.Equal(ranked[j].Candidate.MergedAt) {
			return ranked[i].Candidate.MergedAt.After(ranked[j].Candidate.MergedAt)
		}
		return ranked[i].Candidate.PRNumber < ranked[j].Candidate.PRNumber
	})
	if len(ranked) == 0 {
		return Attribution{Confidence: ConfidenceNone, Reason: "no_merge_candidate_before_failure"}
	}
	best := ranked[0]
	result := Attribution{Candidate: &best.Candidate, Confidence: ConfidenceLow, Reason: "insufficient_attribution_evidence", Ranked: ranked}
	if best.Score == 0 {
		return result
	}
	if len(ranked) > 1 && ranked[1].Score == best.Score {
		result.Reason = "multiple_candidates_have_equal_evidence"
		return result
	}
	result.Confidence = ConfidenceHigh
	result.Reason = "unique_candidate_has_observed_failure_evidence"
	return result
}

func rankCandidate(failure FailureEvidence, candidate MergeCandidate) RankedCandidate {
	ranked := RankedCandidate{Candidate: candidate}
	if candidate.IntroducedReproduction {
		ranked.Score += 3
		ranked.Reasons = append(ranked.Reasons, "introduced_reproduction")
	}
	if overlaps(failure.FailingPaths, candidate.TouchedFiles) {
		ranked.Score += 2
		ranked.Reasons = append(ranked.Reasons, "failure_path_overlaps_merge")
	}
	return ranked
}

func overlaps(failing, touched []string) bool {
	for _, failurePath := range failing {
		failurePath = strings.TrimSpace(failurePath)
		if failurePath == "" {
			continue
		}
		for _, touchedPath := range touched {
			if path.Clean(failurePath) == path.Clean(strings.TrimSpace(touchedPath)) {
				return true
			}
		}
	}
	return false
}
