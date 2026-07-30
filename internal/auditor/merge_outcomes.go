package auditor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

// CandidatesFromMergeOutcomes projects Gatekeeper's durable successful merge
// events into Auditor candidates. Event records, rather than a fresh forge list
// or an agent narrative, are the authority for whether Looper merged a PR.
func CandidatesFromMergeOutcomes(events []storage.EventLogRecord) ([]MergeCandidate, error) {
	candidates := make([]MergeCandidate, 0, len(events))
	for _, event := range events {
		if event.EventType != gatekeeper.MergeOutcomeEventType {
			continue
		}
		var outcome gatekeeper.MergeOutcome
		if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
			return nil, fmt.Errorf("decode merge outcome event %s: %w", event.ID, err)
		}
		if !outcome.Merged {
			continue
		}
		if strings.TrimSpace(outcome.ProjectID) == "" || strings.TrimSpace(outcome.Repo) == "" || outcome.PRNumber <= 0 || strings.TrimSpace(outcome.HeadSHA) == "" {
			return nil, fmt.Errorf("merge outcome event %s is missing merged pull request identity", event.ID)
		}
		mergedAt, err := time.Parse(time.RFC3339Nano, event.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse merge outcome event %s timestamp: %w", event.ID, err)
		}
		candidates = append(candidates, MergeCandidate{ProjectID: outcome.ProjectID, Repo: outcome.Repo, PRNumber: outcome.PRNumber, HeadSHA: outcome.HeadSHA, MergedAt: mergedAt.UTC()})
	}
	return candidates, nil
}
