package gatekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/storage"
)

const (
	reviewerReviewPostedEventType   = "pr.review.posted"
	reviewerAgentStartedEventType   = "reviewer.agent.started"
	reviewerAgentCompletedEventType = "reviewer.agent.completed"
	reviewerAgentFailedEventType    = "reviewer.agent.failed"
	reviewerAgentTimedOutEventType  = "reviewer.agent.timed_out"
)

type reviewerReviewPostedPayload struct {
	Repo     string `json:"repo"`
	PRNumber int64  `json:"prNumber"`
	Event    string `json:"event"`
	HeadSHA  string `json:"headSha"`
	Outcome  string `json:"outcome"`
}

type reviewerAgentProgressPayload struct {
	Repo        string `json:"repo"`
	PRNumber    int64  `json:"prNumber"`
	Phase       string `json:"phase"`
	HeadSHA     string `json:"headSha"`
	ExecutionID string `json:"executionId"`
}

type activeReviewerExecution struct {
	HeadSHA     string
	ExecutionID string
	StartedAt   string
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
	events, err := repos.Events.ListByEntity(ctx, "pull_request", fmt.Sprintf("%s#%d", repo, prNumber))
	if err != nil {
		return evidence, fmt.Errorf("list Reviewer review events: %w", err)
	}
	activeExecutions := make(map[string]activeReviewerExecution)
	for _, event := range events {
		if !reviewEventBelongsToProject(event, projectID) || !reviewEventActorIsReviewer(event) {
			continue
		}
		if event.EventType == reviewerAgentStartedEventType {
			var payload reviewerAgentProgressPayload
			if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err == nil && reviewerAgentProgressMatches(payload, repo, prNumber, requiredHeadSHA) {
				if payload.ExecutionID != "" {
					activeExecutions[payload.ExecutionID] = activeReviewerExecution{HeadSHA: strings.TrimSpace(payload.HeadSHA), ExecutionID: strings.TrimSpace(payload.ExecutionID), StartedAt: event.CreatedAt}
				}
			}
			continue
		}
		if isReviewerAgentTerminalEvent(event.EventType) {
			var payload reviewerAgentProgressPayload
			if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err == nil && strings.EqualFold(strings.TrimSpace(payload.Phase), "review") {
				delete(activeExecutions, strings.TrimSpace(payload.ExecutionID))
			}
			continue
		}
		if event.EventType != reviewerReviewPostedEventType {
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
		reviewEvent := strings.ToUpper(strings.TrimSpace(payload.Event))
		switch reviewEvent {
		case "COMMENT", "APPROVE", "REQUEST_CHANGES":
		default:
			continue
		}
		evidence.ReviewedHeadSHA = reviewedHead
		evidence.Event = reviewEvent
		evidence.Outcome = normalizeCodexReviewOutcome(payload.Outcome)
		evidence.OutcomeKnown = isKnownCodexReviewOutcome(evidence.Outcome)
		evidence.RecordedAt = event.CreatedAt
	}
	evidence.CurrentHeadValid = evidence.ReviewedHeadSHA != "" && strings.EqualFold(evidence.ReviewedHeadSHA, evidence.RequiredHeadSHA)
	if !evidence.CurrentHeadValid {
		for _, execution := range activeExecutions {
			if !strings.EqualFold(execution.HeadSHA, evidence.RequiredHeadSHA) {
				continue
			}
			if !evidence.InProgress || execution.StartedAt >= evidence.StartedAt {
				evidence.InProgress = true
				evidence.StartedAt = execution.StartedAt
				evidence.ExecutionID = execution.ExecutionID
			}
		}
	}
	return evidence, nil
}

func reviewerAgentProgressMatches(payload reviewerAgentProgressPayload, repo string, prNumber int64, requiredHeadSHA string) bool {
	return strings.EqualFold(strings.TrimSpace(payload.Phase), "review") &&
		(strings.TrimSpace(payload.Repo) == "" || strings.EqualFold(strings.TrimSpace(payload.Repo), strings.TrimSpace(repo))) &&
		(payload.PRNumber == 0 || payload.PRNumber == prNumber) &&
		strings.EqualFold(strings.TrimSpace(payload.HeadSHA), strings.TrimSpace(requiredHeadSHA))
}

func isReviewerAgentTerminalEvent(eventType string) bool {
	switch eventType {
	case reviewerAgentCompletedEventType, reviewerAgentFailedEventType, reviewerAgentTimedOutEventType:
		return true
	default:
		return false
	}
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

func normalizeCodexReviewOutcome(outcome string) string {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "clean", "blocking", "non_blocking":
		return strings.ToLower(strings.TrimSpace(outcome))
	case "actionable":
		return "non_blocking"
	default:
		return strings.ToLower(strings.TrimSpace(outcome))
	}
}

func isKnownCodexReviewOutcome(outcome string) bool {
	switch outcome {
	case "clean", "blocking", "non_blocking":
		return true
	default:
		return false
	}
}

func codexReviewOutcomeReasonSubject(evidence CodexReviewEvidence) string {
	outcome := strings.TrimSpace(evidence.Outcome)
	if outcome == "" {
		outcome = "missing"
	}
	return "outcome=" + outcome + ";head=" + evidence.ReviewedHeadSHA
}

func codexReviewInProgressReasonSubject(evidence CodexReviewEvidence) string {
	parts := []string{"head=" + evidence.RequiredHeadSHA}
	if evidence.ExecutionID != "" {
		parts = append(parts, "executionId="+evidence.ExecutionID)
	}
	if evidence.StartedAt != "" {
		parts = append(parts, "startedAt="+evidence.StartedAt)
	}
	return strings.Join(parts, ";")
}
