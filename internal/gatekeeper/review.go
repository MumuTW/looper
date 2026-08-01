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
// for one pull request. Reviewer writes this event only after verifying the
// structured GitHub review marker, so Gatekeeper does not inspect prose or
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
		// A markerless event (clean COMMENT no-op) records that the Reviewer
		// processed the head without publishing a structured GitHub review, so
		// it must not satisfy the current-head gate. Only a marker-verified
		// event proves a structured review was published for this head.
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
	return event.ProjectID != nil && strings.TrimSpace(*event.ProjectID) == strings.TrimSpace(projectID)
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
