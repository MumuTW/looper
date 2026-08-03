package loops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/storage"
)

const takeoverResumeMetadataKey = "takeoverResume"

// TakeoverResume records that a loop was handed back after an interactive human
// takeover, carrying the native session id the human drove so the daemon's next
// worker run resumes THAT session (seeing the human's turns) rather than starting
// a fresh one. Consumed after one resume.
type TakeoverResume struct {
	SessionID string `json:"sessionId,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
}

type HandbackPreparationInput struct {
	LoopID string
	NowISO string
}

type HandbackPreparationResult struct {
	Loop                storage.LoopRecord
	CancelledQueueItems int64
}

// PrepareHandback captures the human-driven native session and cancels queue
// work that survived the takeover race. It is transaction-local so a caller can
// re-arm the loop only after both durable changes commit.
func PrepareHandback(ctx context.Context, repos *storage.Repositories, input HandbackPreparationInput) (HandbackPreparationResult, error) {
	if repos == nil || repos.Loops == nil || repos.Queue == nil || repos.AgentExecutions == nil {
		return HandbackPreparationResult{}, fmt.Errorf("handback preparation is not configured")
	}
	if strings.TrimSpace(input.LoopID) == "" || strings.TrimSpace(input.NowISO) == "" {
		return HandbackPreparationResult{}, fmt.Errorf("handback preparation requires loop id and time")
	}
	loop, err := repos.Loops.GetByID(ctx, input.LoopID)
	if err != nil {
		return HandbackPreparationResult{}, err
	}
	if loop == nil {
		return HandbackPreparationResult{}, fmt.Errorf("%w: %s", ErrLoopNotFound, input.LoopID)
	}
	updated := *loop
	execution, err := repos.AgentExecutions.GetLatestByLoopID(ctx, input.LoopID)
	if err != nil {
		return HandbackPreparationResult{}, err
	}
	if execution != nil && execution.NativeSessionID != nil && strings.TrimSpace(*execution.NativeSessionID) != "" {
		metadata, err := WriteTakeoverResume(updated.MetadataJSON, TakeoverResume{SessionID: strings.TrimSpace(*execution.NativeSessionID)})
		if err != nil {
			return HandbackPreparationResult{}, fmt.Errorf("persist takeover resume marker: %w", err)
		}
		updated.MetadataJSON = &metadata
		updated.UpdatedAt = input.NowISO
		if err := repos.Loops.UpsertChangingHumanHold(ctx, updated); err != nil {
			return HandbackPreparationResult{}, err
		}
	}
	reason := "Cleared for takeover handback"
	cancelled, err := repos.Queue.CancelByLoop(ctx, input.LoopID, input.NowISO, &reason)
	if err != nil {
		return HandbackPreparationResult{}, err
	}
	return HandbackPreparationResult{Loop: updated, CancelledQueueItems: cancelled}, nil
}

// ReadTakeoverResume returns the pending takeover-resume marker, if any.
func ReadTakeoverResume(metadataJSON *string) (TakeoverResume, bool) {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[takeoverResumeMetadataKey]
	if !ok {
		return TakeoverResume{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return TakeoverResume{}, false
	}
	var tr TakeoverResume
	if err := json.Unmarshal(encoded, &tr); err != nil {
		return TakeoverResume{}, false
	}
	return tr, true
}

// WriteTakeoverResume merges the takeover-resume marker into a loop's metadata,
// preserving all other keys.
func WriteTakeoverResume(metadataJSON *string, tr TakeoverResume) (string, error) {
	meta, err := DecodeMetadataObjectForWrite(metadataJSON)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(tr)
	if err != nil {
		return "", err
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		return "", err
	}
	meta[takeoverResumeMetadataKey] = asMap
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ClearTakeoverResume removes the takeover-resume marker from a loop's metadata.
func ClearTakeoverResume(metadataJSON *string) (string, error) {
	meta, err := DecodeMetadataObjectForWrite(metadataJSON)
	if err != nil {
		return "", err
	}
	delete(meta, takeoverResumeMetadataKey)
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
