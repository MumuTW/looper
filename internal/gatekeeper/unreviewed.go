package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/storage"
)

// UnreviewedPullRequest is one pull request whose most recent Gate report
// recorded no completed review.
type UnreviewedPullRequest struct {
	ProjectID        string           `json:"projectId"`
	Repo             string           `json:"repo"`
	PRNumber         int64            `json:"prNumber"`
	PullRequestState string           `json:"pullRequestState"`
	ObservedHeadSHA  string           `json:"observedHeadSha"`
	EvaluatedAt      string           `json:"evaluatedAt"`
	ReviewProvenance ReviewProvenance `json:"reviewProvenance"`
}

// ListUnreviewed enumerates the pull requests whose latest Gate report says no
// reviewer completed a review — the ones that merged with a refusal in place of
// a review, and the ones nothing looked at.
//
// state filters on the pull request state the report observed; "merged" answers
// the question this exists for. Empty means no filter.
//
// A report that never made the observation, or that made it and could not get an
// answer, is left out. This list is the set of pull requests Looper can say were
// not reviewed, not the set it cannot say were reviewed.
//
// The Gate reports are already the durable record and the event log is already
// their store, so this reads them and derives the answer. It adds no second
// materialized view to keep in sync with the first.
func ListUnreviewed(ctx context.Context, repos *storage.Repositories, state string) ([]UnreviewedPullRequest, error) {
	if repos == nil || repos.Events == nil {
		return nil, fmt.Errorf("gatekeeper event repository is not configured")
	}
	records, err := repos.Events.ListByEntityTypeAndEventTypes(ctx, "pull_request", []string{GateReportEventType})
	if err != nil {
		return nil, fmt.Errorf("list gate reports: %w", err)
	}
	// Records arrive oldest first, so a later report for the same pull request
	// overwrites an earlier one and the map ends up holding the latest verdict on
	// each — which is the only one that describes how the pull request stands now.
	latest := make(map[string]Report, len(records))
	for _, record := range records {
		if record.EntityID == nil {
			continue
		}
		var report Report
		if err := json.Unmarshal([]byte(record.PayloadJSON), &report); err != nil {
			continue
		}
		latest[*record.EntityID] = report
	}
	wanted := strings.ToUpper(strings.TrimSpace(state))
	items := make([]UnreviewedPullRequest, 0)
	for _, report := range latest {
		provenance := report.Evidence.ReviewProvenance
		if provenance.Status != ReviewProvenanceAbsent && provenance.Status != ReviewProvenanceRefused {
			continue
		}
		if wanted != "" && !strings.EqualFold(strings.TrimSpace(report.Evidence.PullRequestState), wanted) {
			continue
		}
		items = append(items, UnreviewedPullRequest{
			ProjectID:        report.ProjectID,
			Repo:             report.Repo,
			PRNumber:         report.PRNumber,
			PullRequestState: report.Evidence.PullRequestState,
			ObservedHeadSHA:  report.ObservedHeadSHA,
			EvaluatedAt:      report.EvaluatedAt,
			ReviewProvenance: provenance,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Repo != items[j].Repo {
			return items[i].Repo < items[j].Repo
		}
		return items[i].PRNumber < items[j].PRNumber
	})
	return items, nil
}
