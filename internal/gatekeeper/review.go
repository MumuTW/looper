package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/storage"
)

const reviewerReviewPostedEventType = "pr.review.posted"

type reviewerReviewPostedPayload struct {
	Repo           string `json:"repo"`
	PRNumber       int64  `json:"prNumber"`
	Event          string `json:"event"`
	HeadSHA        string `json:"headSha"`
	MarkerVerified bool   `json:"markerVerified"`
}

// latestCodexReviewForHead projects the latest durable Reviewer review event
// for one pull request. Reviewer writes this event only after verifying a
// clean signal — either a structured GitHub review marker or the configured
// COMMENT clean-noop +1 reaction — so Gatekeeper does not inspect prose or
// infer completion from a generic review decision.
func latestCodexReviewForHead(ctx context.Context, repos *storage.Repositories, projectID, repo string, prNumber int64, requiredHeadSHA string) (CodexReviewEvidence, error) {
	evidence := CodexReviewEvidence{RequiredHeadSHA: strings.TrimSpace(requiredHeadSHA)}
	if repos == nil || repos.Events == nil {
		return evidence, fmt.Errorf("events repository is not configured")
	}
	events, err := repos.Events.ListByEntityAndEventTypes(ctx, "pull_request", fmt.Sprintf("%s#%d", repo, prNumber), []string{reviewerReviewPostedEventType})
	if err != nil {
		return evidence, fmt.Errorf("list Reviewer review events: %w", err)
	}
	for _, event := range events {
		if event.EventType != reviewerReviewPostedEventType || !reviewEventBelongsToProject(event, projectID) || !reviewEventActorIsReviewer(event) {
			continue
		}
		var payload reviewerReviewPostedPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			continue
		}
		if strings.TrimSpace(payload.Repo) != "" && !strings.EqualFold(strings.TrimSpace(payload.Repo), strings.TrimSpace(repo)) {
			continue
		}
		if payload.PRNumber != 0 && payload.PRNumber != prNumber {
			continue
		}
		reviewedHead := strings.TrimSpace(payload.HeadSHA)
		if reviewedHead == "" {
			continue
		}
		// A markerless event (markerVerified=false) records that the Reviewer
		// processed the head without publishing a verified clean signal, so it
		// must not satisfy the current-head gate. The COMMENT clean-noop path
		// records markerVerified=true because the runner validated the
		// configured clean signal (+1 reaction) for that policy; a
		// marker-verified event proves either a structured GitHub review or the
		// configured COMMENT clean-noop signal was published for this head.
		if !payload.MarkerVerified {
			continue
		}
		reviewEvent := strings.ToUpper(strings.TrimSpace(payload.Event))
		switch reviewEvent {
		case "COMMENT", "APPROVE", "REQUEST_CHANGES":
		default:
			continue
		}
		evidence.ReviewedHeadSHA = reviewedHead
		evidence.Event = reviewEvent
		evidence.RecordedAt = event.CreatedAt
	}
	evidence.CurrentHeadValid = evidence.ReviewedHeadSHA != "" && strings.EqualFold(evidence.ReviewedHeadSHA, evidence.RequiredHeadSHA)
	return evidence, nil
}

func reviewEventBelongsToProject(event storage.EventLogRecord, projectID string) bool {
	// Compare project IDs exactly, without trimming. EvaluatePullRequest
	// deliberately preserves the caller's original project ID (only an
	// emptiness check is trimmed) and ListByProjectAndEntityType matches it
	// with an exact SQL equality. Trimming here would let a renamed project
	// whose new ID trims to the old value (e.g. " foo " → "foo") authorize
	// Gatekeeper under the new project using a review written under the old
	// one, violating the same-project authority contract.
	return event.ProjectID != nil && *event.ProjectID == projectID
}

func reviewEventActorIsReviewer(event storage.EventLogRecord) bool {
	if event.ActorID == nil || strings.TrimSpace(*event.ActorID) == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(*event.ActorID), "reviewer-loop")
}

func codexReviewReasonSubject(evidence CodexReviewEvidence) string {
	if evidence.ReviewedHeadSHA == "" {
		return "reviewed=none;required=" + evidence.RequiredHeadSHA
	}
	return "reviewed=" + evidence.ReviewedHeadSHA + ";required=" + evidence.RequiredHeadSHA
}
