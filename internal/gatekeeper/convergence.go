package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/reviewer/convergence"
	"github.com/MumuTW/looper/internal/reviewitem"
	"github.com/MumuTW/looper/internal/storage"
)

// ReviewerConvergenceEvidence is the read-only convergence state that Gatekeeper
// used for this evaluation. The Reviewer loop metadata is the authority; this is
// an audit projection, not a second lifecycle record.
type ReviewerConvergenceEvidence struct {
	Policy                  convergence.Policy `json:"policy"`
	Status                  string             `json:"status,omitempty"`
	Action                  convergence.Action `json:"action,omitempty"`
	Reason                  convergence.Reason `json:"reason,omitempty"`
	TotalRounds             int                `json:"totalRounds"`
	ConsecutiveUnproductive int                `json:"consecutiveUnproductive"`
	OpenItemIDs             []string           `json:"openItemIds"`
}

type reviewerConvergenceMetadata struct {
	Policy    convergence.Policy `json:"policy"`
	State     convergence.State  `json:"state"`
	Action    convergence.Action `json:"action,omitempty"`
	Reason    convergence.Reason `json:"reason,omitempty"`
	Status    string             `json:"status,omitempty"`
	UpdatedAt string             `json:"updatedAt,omitempty"`
}

// latestReviewerConvergence reads only the newest Reviewer loop for this
// project/repository/pull request. A newer loop without valid convergence
// metadata deliberately hides older metadata rather than letting stale state
// block a new review. Missing or malformed legacy metadata is not inferred.
func latestReviewerConvergence(ctx context.Context, repos *storage.Repositories, projectID, repo string, prNumber int64) (ReviewerConvergenceEvidence, bool, error) {
	if repos == nil || repos.Loops == nil {
		return ReviewerConvergenceEvidence{}, false, nil
	}
	loops, err := repos.Loops.ListByRepoAndPR(ctx, repo, prNumber)
	if err != nil {
		return ReviewerConvergenceEvidence{}, false, fmt.Errorf("list reviewer convergence loops: %w", err)
	}
	for _, loop := range loops {
		if loop.ProjectID != projectID || loop.Type != string(domain.LoopTypeReviewer) {
			continue
		}
		evidence, ok := reviewerConvergenceFromLoop(loop)
		return evidence, ok, nil
	}
	return ReviewerConvergenceEvidence{}, false, nil
}

func reviewerConvergenceFromLoop(loop storage.LoopRecord) (ReviewerConvergenceEvidence, bool) {
	if loop.MetadataJSON == nil || strings.TrimSpace(*loop.MetadataJSON) == "" {
		return ReviewerConvergenceEvidence{}, false
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(*loop.MetadataJSON), &metadata); err != nil {
		return ReviewerConvergenceEvidence{}, false
	}
	raw, ok := metadata["convergence"]
	if !ok || raw == nil {
		return ReviewerConvergenceEvidence{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ReviewerConvergenceEvidence{}, false
	}
	var persisted reviewerConvergenceMetadata
	if err := json.Unmarshal(encoded, &persisted); err != nil || persisted.Policy.Validate() != nil || !validConvergenceStatus(persisted.Status) || !validConvergenceState(persisted.State) {
		return ReviewerConvergenceEvidence{}, false
	}

	openItemIDs := make([]string, 0)
	for id, item := range persisted.State.Items {
		if persisted.Policy.Includes(item.Severity) && item.Status == convergence.ItemStatusOpen {
			openItemIDs = append(openItemIDs, id)
		}
	}
	sort.Strings(openItemIDs)
	return ReviewerConvergenceEvidence{
		Policy:                  persisted.Policy,
		Status:                  persisted.Status,
		Action:                  persisted.Action,
		Reason:                  persisted.Reason,
		TotalRounds:             persisted.State.TotalRounds,
		ConsecutiveUnproductive: persisted.State.ConsecutiveUnproductive,
		OpenItemIDs:             openItemIDs,
	}, true
}

func validConvergenceStatus(status string) bool {
	switch status {
	case "", "active", "awaiting_human", "completed":
		return true
	default:
		return false
	}
}

func validConvergenceState(state convergence.State) bool {
	for id, item := range state.Items {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(item.ID) == "" || id != item.ID {
			return false
		}
		if _, err := reviewitem.ParseSeverity(string(item.Severity)); err != nil {
			return false
		}
		switch item.Status {
		case convergence.ItemStatusOpen, convergence.ItemStatusResolved, convergence.ItemStatusSuperseded, convergence.ItemStatusDeferred:
		default:
			return false
		}
	}
	return true
}

func reviewerConvergenceReasonSubject(evidence ReviewerConvergenceEvidence) string {
	parts := []string{"floor=" + string(evidence.Policy.SeverityFloor)}
	if evidence.Status != "" {
		parts = append(parts, "status="+evidence.Status)
	}
	if evidence.Reason != "" {
		parts = append(parts, "reason="+string(evidence.Reason))
	}
	if len(evidence.OpenItemIDs) > 0 {
		parts = append(parts, "open="+strings.Join(evidence.OpenItemIDs, ","))
	}
	return strings.Join(parts, ";")
}

func reviewerConvergenceBlocks(evidence ReviewerConvergenceEvidence) bool {
	// Awaiting human is itself a durable decision boundary. Normal evaluator
	// output always carries floor-qualified open items here, but retaining the
	// status guard keeps a partially written escalation fail-closed.
	return evidence.Status == "awaiting_human" || len(evidence.OpenItemIDs) > 0
}
