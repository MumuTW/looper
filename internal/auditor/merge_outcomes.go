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
		var projectID, repo, headSHA string
		var prNumber int64
		mergedAtText := event.CreatedAt
		switch event.EventType {
		case gatekeeper.MergeOutcomeEventType:
			var outcome gatekeeper.MergeOutcome
			if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
				return nil, fmt.Errorf("decode merge outcome event %s: %w", event.ID, err)
			}
			if !outcome.Merged {
				continue
			}
			projectID, repo, prNumber, headSHA = outcome.ProjectID, outcome.Repo, outcome.PRNumber, outcome.HeadSHA
		case eventlog.CoordinatorPullRequestMergedEventType:
			var outcome eventlog.CoordinatorPullRequestMerged
			if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
				return nil, fmt.Errorf("decode coordinator merge event %s: %w", event.ID, err)
			}
			projectID, repo, prNumber, headSHA, mergedAtText = outcome.ProjectID, outcome.Repo, outcome.PRNumber, outcome.HeadSHA, outcome.MergedAt
		default:
			continue
		}
		if strings.TrimSpace(projectID) == "" || strings.TrimSpace(repo) == "" || prNumber <= 0 || strings.TrimSpace(headSHA) == "" {
			return nil, fmt.Errorf("merge event %s is missing merged pull request identity", event.ID)
		}
		mergedAt, err := time.Parse(time.RFC3339Nano, mergedAtText)
		if err != nil {
			return nil, fmt.Errorf("parse merge event %s timestamp: %w", event.ID, err)
		}
		// Version 1 did not persist the availability bit. A legacy event that
		// already carries a non-empty file list can be safely enriched from its
		// own authoritative snapshot; an empty or absent list remains blocked
		// until the confirmation lane re-reads the PR files from GitHub.
		filesAvailable := outcome.TouchedFilesAvailable || (outcome.Version < 2 && len(outcome.TouchedFiles) > 0)
		candidates = append(candidates, MergeCandidate{ProjectID: outcome.ProjectID, Repo: outcome.Repo, PRNumber: outcome.PRNumber, HeadSHA: outcome.HeadSHA, MergeCommitSHA: outcome.MergeCommitSHA, SourceIssue: outcome.SourceIssue, MergedAt: mergedAt.UTC(), TouchedFiles: append([]string(nil), outcome.TouchedFiles...), TouchedFilesAvailable: filesAvailable})
	}
	return candidates, nil
}
