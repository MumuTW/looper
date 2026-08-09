package auditor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

// CandidatesFromMergeOutcomes projects durable successful merge events into
// Auditor candidates. Event records, rather than a fresh forge list or an agent
// narrative, are the authority for whether Looper observed a merged PR.
func CandidatesFromMergeOutcomes(events []storage.EventLogRecord) ([]MergeCandidate, error) {
	candidates := make([]MergeCandidate, 0, len(events))
	for _, event := range events {
		var candidate MergeCandidate
		var mergedAtText string
		switch event.EventType {
		case gatekeeper.MergeOutcomeEventType:
			var outcome gatekeeper.MergeOutcome
			if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
				return nil, fmt.Errorf("decode merge outcome event %s: %w", event.ID, err)
			}
			if !outcome.Merged {
				continue
			}
			// Version 1 did not persist the availability bit. A legacy event that
			// already carries a non-empty file list can be safely enriched from its
			// own authoritative snapshot; an empty or absent list remains blocked
			// until the confirmation lane re-reads the PR files from GitHub.
			filesAvailable := outcome.TouchedFilesAvailable || (outcome.Version < 2 && len(outcome.TouchedFiles) > 0)
			candidate = MergeCandidate{ProjectID: outcome.ProjectID, Repo: outcome.Repo, PRNumber: outcome.PRNumber, HeadSHA: outcome.HeadSHA, MergeCommitSHA: outcome.MergeCommitSHA, MergeStrategy: outcome.MergeStrategy, SourceIssue: outcome.SourceIssue, TouchedFiles: append([]string(nil), outcome.TouchedFiles...), TouchedFilesAvailable: filesAvailable}
			mergedAtText = outcome.AttemptedAt
			if strings.TrimSpace(mergedAtText) == "" {
				mergedAtText = event.CreatedAt
			}
		case eventlog.CoordinatorPullRequestMergedEventType:
			var outcome eventlog.CoordinatorPullRequestMerged
			if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
				return nil, fmt.Errorf("decode coordinator merge event %s: %w", event.ID, err)
			}
			candidate = MergeCandidate{ProjectID: outcome.ProjectID, Repo: outcome.Repo, PRNumber: outcome.PRNumber, HeadSHA: outcome.HeadSHA}
			mergedAtText = outcome.MergedAt
			if strings.TrimSpace(candidate.ProjectID) == "" && event.ProjectID != nil {
				// Legacy coordinator payloads stored the project only on the
				// EventLog row. Preserve that exact key for project-scoped
				// attribution; only the emptiness check is trimmed.
				candidate.ProjectID = *event.ProjectID
			}
		default:
			continue
		}
		if strings.TrimSpace(candidate.ProjectID) == "" && event.ProjectID != nil {
			candidate.ProjectID = *event.ProjectID
		}
		if strings.TrimSpace(candidate.ProjectID) == "" || strings.TrimSpace(candidate.Repo) == "" || candidate.PRNumber <= 0 || strings.TrimSpace(candidate.HeadSHA) == "" {
			return nil, fmt.Errorf("merge event %s is missing merged pull request identity", event.ID)
		}
		mergedAt, err := time.Parse(time.RFC3339Nano, mergedAtText)
		if err != nil {
			// Forge payloads are the preferred merge-time authority, but old or
			// malformed coordinator records must not poison the entire candidate
			// projection. The append timestamp is daemon-owned and therefore a
			// reliable lower-precision fallback for that one event.
			mergedAt, err = time.Parse(time.RFC3339Nano, event.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("parse merge event %s timestamp: %w", event.ID, err)
			}
		}
		candidate.MergedAt = mergedAt.UTC()
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}
