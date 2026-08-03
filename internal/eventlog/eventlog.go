package eventlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MumuTW/looper/internal/storage"
)

// CoordinatorPullRequestMergedEventType records a merge-watch observation of
// a pull request merged by the forge (including Mergify). The forge's
// MergedAt value in the payload is the merge-time authority; CreatedAt is only
// when the daemon observed it.
const CoordinatorPullRequestMergedEventType = "coordinator.pull_request.merged"

// Post-merge digest events are ordinary EventLog records so downstream
// consumers have one durable local source of truth.
const (
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

// CoordinatorRoutedMergeWatchEventType records that a routed pull request (one
// carrying the Mergify auto-merge route label) is under merge-watch
// independent of issue discovery. The open-issue loop loses a routed merge the
// moment its body closes the tracked issue, so this registry keeps the PR
// number durable until the merge (or a non-merge close) is observed.
const CoordinatorRoutedMergeWatchEventType = "coordinator.routed_merge_watch"

// CoordinatorRoutedMergeWatch is the durable payload for one routed-PR
// merge-watch registration. Settled marks a terminal observation (merged and
// recorded, closed without merge, or handed to a competing human-owned native
// auto-merge route); the reader keeps the newest record per entity.
type CoordinatorRoutedMergeWatch struct {
	Version   int    `json:"version"`
	Revision  int64  `json:"revision,omitempty"`
	ProjectID string `json:"projectId"`
	Repo      string `json:"repo"`
	PRNumber  int64  `json:"prNumber"`
	HeadSHA   string `json:"headSha"`
	Settled   bool   `json:"settled,omitempty"`
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
