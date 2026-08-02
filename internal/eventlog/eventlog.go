package eventlog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/storage"
)

// Post-merge digest events are intentionally ordinary EventLog records. The
// digest consumes these durable observations instead of querying GitHub at
// report time, so a daily report has one local source of truth.
const (
	CoordinatorPullRequestMergedEventType  = "coordinator.pull_request.merged"
	CoordinatorCloseAndRegenerateEventType = "coordinator.close_and_regenerate"
	FixerCloseAndRegenerateEventType       = "fixer.close_and_regenerate"
	PostMergeDigestSentEventType           = "coordinator.post_merge_digest.sent"
)

// CoordinatorPullRequestMerged is the durable payload for a merge-watch merge
// observation. It carries the identity needed by downstream audit consumers
// without requiring another forge read.
type CoordinatorPullRequestMerged struct {
	Version     int    `json:"version"`
	ProjectID   string `json:"projectId"`
	Repo        string `json:"repo"`
	PRNumber    int64  `json:"prNumber"`
	IssueNumber int64  `json:"issueNumber,omitempty"`
	HeadSHA     string `json:"headSha"`
	MergedAt    string `json:"mergedAt"`
}

type AppendInput struct {
	ID               string
	EventType        string
	ProjectID        *string
	LoopID           *string
	RunID            *string
	EntityType       *string
	EntityID         *string
	CorrelationID    *string
	CausationID      *string
	ActorType        *string
	ActorID          *string
	ActorDisplayName *string
	Payload          any
	PayloadJSON      *string
	CreatedAt        time.Time
}

func FormatJavaScriptISOString(value time.Time) string {
	value = value.UTC()
	return fmt.Sprintf("%s.%03dZ", value.Format("2006-01-02T15:04:05"), value.Nanosecond()/int(time.Millisecond))
}

// NextJavaScriptISOString returns a JavaScript-ISO timestamp strictly after
// previous. Loop writes use UpdatedAt as their durable revision, so a fixed or
// backward-moving clock must not reuse or regress that value.
func NextJavaScriptISOString(now time.Time, previous string) string {
	candidate := now.UTC().Truncate(time.Millisecond)
	if parsed, err := time.Parse(time.RFC3339Nano, previous); err == nil && !candidate.After(parsed) {
		candidate = parsed.UTC().Add(time.Millisecond)
	}
	return FormatJavaScriptISOString(candidate)
}

func NewEventID(prefix string) string {
	if prefix == "" {
		prefix = "event"
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UTC().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(raw)
}

// StableReviewEvidenceID returns the deterministic event ID shared by the
// Reviewer and Gatekeeper writers for one review-capacity observation. Keeping
// the identity algorithm at the event-log boundary makes retries and
// cross-writer reconciliation idempotent even when either writer is changed.
func StableReviewEvidenceID(eventType, repo string, prNumber int64, headSHA, reviewID, reason string) string {
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s", eventType, strings.TrimSpace(repo), prNumber, strings.TrimSpace(headSHA), strings.TrimSpace(reviewID), strings.TrimSpace(reason))
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s:%x", eventType, digest[:])
}

func Append(ctx context.Context, repositories *storage.Repositories, input AppendInput) error {
	if repositories == nil || repositories.Events == nil {
		return fmt.Errorf("events repository is not configured")
	}

	payloadJSON := "{}"
	if input.PayloadJSON != nil {
		payloadJSON = *input.PayloadJSON
	} else if input.Payload != nil {
		encoded, err := json.Marshal(input.Payload)
		if err != nil {
			return fmt.Errorf("marshal event payload: %w", err)
		}
		payloadJSON = string(encoded)
	}

	id := input.ID
	if id == "" {
		id = NewEventID("event")
	}

	createdAt := input.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	actorType := input.ActorType
	actorID := input.ActorID
	actorDisplayName := input.ActorDisplayName
	if actorType == nil {
		actorType = stringPointer("system")
	}
	if actorID == nil {
		actorID = stringPointer("looperd")
	}
	if actorDisplayName == nil {
		actorDisplayName = stringPointer("looperd")
	}

	return repositories.Events.Append(ctx, storage.EventLogRecord{
		ID:               id,
		EventType:        input.EventType,
		ProjectID:        input.ProjectID,
		LoopID:           input.LoopID,
		RunID:            input.RunID,
		EntityType:       input.EntityType,
		EntityID:         input.EntityID,
		CorrelationID:    input.CorrelationID,
		CausationID:      input.CausationID,
		ActorType:        actorType,
		ActorID:          actorID,
		ActorDisplayName: actorDisplayName,
		PayloadJSON:      payloadJSON,
		CreatedAt:        FormatJavaScriptISOString(createdAt),
	})
}

func stringPointer(value string) *string {
	return &value
}
