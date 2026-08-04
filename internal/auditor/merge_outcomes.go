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
			projectID, repo, prNumber, headSHA, mergedAtText = outcome.ProjectID, outcome.Repo, outcome.PRNumber, outcome.HeadSHA, outcome.AttemptedAt
			if strings.TrimSpace(mergedAtText) == "" {
				mergedAtText = event.CreatedAt
			}
		case eventlog.CoordinatorPullRequestMergedEventType:
			var outcome eventlog.CoordinatorPullRequestMerged
			if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
				return nil, fmt.Errorf("decode coordinator merge event %s: %w", event.ID, err)
			}
			projectID, repo, prNumber, headSHA, mergedAtText = outcome.ProjectID, outcome.Repo, outcome.PRNumber, outcome.HeadSHA, outcome.MergedAt
			if strings.TrimSpace(projectID) == "" && event.ProjectID != nil {
				// Legacy coordinator payloads stored the project only on the
				// EventLog row. Preserve that exact key for project-scoped
				// attribution; only the emptiness check is trimmed.
				projectID = *event.ProjectID
			}
		default:
			continue
		}
		if strings.TrimSpace(projectID) == "" && event.ProjectID != nil {
			projectID = *event.ProjectID
		}
		if strings.TrimSpace(projectID) == "" || strings.TrimSpace(repo) == "" || prNumber <= 0 || strings.TrimSpace(headSHA) == "" {
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
		candidates = append(candidates, MergeCandidate{ProjectID: projectID, Repo: strings.TrimSpace(repo), PRNumber: prNumber, HeadSHA: strings.TrimSpace(headSHA), MergedAt: mergedAt.UTC()})
	}
	return candidates, nil
}
