package hitl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AskSentinelRelPath is where an agent writes a mid-run question, relative to
// the worktree root.
const AskSentinelRelPath = ".looper/ask.json"

// DeliveredCommentStashRelPath stores the GitHub/Forgejo ask-comment id after a
// successful remote post when durable park correlation has not yet been written.
// Survives a correlation-attach retry so CreateIssueComment is not repeated.
const DeliveredCommentStashRelPath = ".looper/hitl-delivered-comment.json"

// AskPayload is the on-disk decision brief written by an agent before stopping.
type AskPayload struct {
	Question          string            `json:"question"`
	Options           []string          `json:"options"`
	Recommendation    string            `json:"recommendation,omitempty"`
	RecommendedOption string            `json:"recommendedOption,omitempty"`
	Consequences      map[string]string `json:"consequences,omitempty"`
	Confidence        string            `json:"confidence,omitempty"`
	// ExecutionID optionally identifies the agent execution that wrote this
	// sentinel. When present it is matched against the durable park's
	// ExecutionID so a same-text re-escalation is not treated as the parked file.
	ExecutionID string `json:"executionId,omitempty"`
}

// ReadAskSentinel reads the agent's ask sentinel without deleting it.
// Returns (nil, nil) when no sentinel exists.
// Returns an error when the file is present but unreadable, malformed, or
// missing a question / at least one non-empty option (fail closed).
func ReadAskSentinel(worktreePath string) (*AskPayload, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, nil
	}
	path := filepath.Join(worktreePath, AskSentinelRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read HITL ask sentinel: %w", err)
	}
	var ask AskPayload
	if err := json.Unmarshal(raw, &ask); err != nil {
		return nil, fmt.Errorf("malformed HITL ask sentinel: %w", err)
	}
	if strings.TrimSpace(ask.Question) == "" {
		return nil, fmt.Errorf("HITL ask sentinel missing question")
	}
	options := compactNonEmpty(ask.Options)
	if len(options) == 0 {
		return nil, fmt.Errorf("HITL ask sentinel requires at least one non-empty option")
	}
	ask.Options = options
	return &ask, nil
}

// RemoveAskSentinel deletes the ask sentinel if present. Best-effort after
// durable suspension has been recorded.
func RemoveAskSentinel(worktreePath string) {
	if strings.TrimSpace(worktreePath) == "" {
		return
	}
	_ = os.Remove(filepath.Join(worktreePath, AskSentinelRelPath))
}

// DeliveredCommentStash is the on-disk correlation recovery record written after
// CreateIssueComment succeeds and removed once AskCommentID is durably parked.
type DeliveredCommentStash struct {
	AskCommentID int64  `json:"askCommentId"`
	ExecutionID  string `json:"executionId,omitempty"`
	PRNumber     int64  `json:"prNumber,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Transport    string `json:"transport,omitempty"`
}

// WriteDeliveredCommentStash persists a delivered ask-comment id for correlation
// retry. Best-effort: returns an error when the worktree path is unusable.
func WriteDeliveredCommentStash(worktreePath string, stash DeliveredCommentStash) error {
	if strings.TrimSpace(worktreePath) == "" {
		return fmt.Errorf("write delivered comment stash: empty worktree path")
	}
	if stash.AskCommentID <= 0 {
		return fmt.Errorf("write delivered comment stash: missing askCommentId")
	}
	dir := filepath.Join(worktreePath, ".looper")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write delivered comment stash: mkdir: %w", err)
	}
	raw, err := json.Marshal(stash)
	if err != nil {
		return fmt.Errorf("write delivered comment stash: marshal: %w", err)
	}
	path := filepath.Join(worktreePath, DeliveredCommentStashRelPath)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write delivered comment stash: %w", err)
	}
	return nil
}

// ReadDeliveredCommentStash loads a correlation recovery record, if present.
// Returns (nil, nil) when missing.
func ReadDeliveredCommentStash(worktreePath string) (*DeliveredCommentStash, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, nil
	}
	path := filepath.Join(worktreePath, DeliveredCommentStashRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read delivered comment stash: %w", err)
	}
	var stash DeliveredCommentStash
	if err := json.Unmarshal(raw, &stash); err != nil {
		return nil, fmt.Errorf("malformed delivered comment stash: %w", err)
	}
	if stash.AskCommentID <= 0 {
		return nil, fmt.Errorf("delivered comment stash missing askCommentId")
	}
	return &stash, nil
}

// RemoveDeliveredCommentStash deletes the correlation recovery record if present.
func RemoveDeliveredCommentStash(worktreePath string) {
	if strings.TrimSpace(worktreePath) == "" {
		return
	}
	_ = os.Remove(filepath.Join(worktreePath, DeliveredCommentStashRelPath))
}

// ConsumeAskSentinel reads and removes the agent's ask sentinel from the
// worktree, if present. Returns (nil, nil) when no sentinel exists.
// Used by worker; Fixer prefers ReadAskSentinel + RemoveAskSentinel so the
// file is only deleted after durable suspension.
//
// Malformed sentinels are removed and ignored (worker historical behavior).
// Prefer ReadAskSentinel for fail-closed callers.
func ConsumeAskSentinel(worktreePath string) (*AskPayload, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, nil
	}
	path := filepath.Join(worktreePath, AskSentinelRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ask AskPayload
	if err := json.Unmarshal(raw, &ask); err != nil {
		_ = os.Remove(path)
		return nil, nil
	}
	_ = os.Remove(path)
	if strings.TrimSpace(ask.Question) == "" {
		return nil, nil
	}
	ask.Options = compactNonEmpty(ask.Options)
	return &ask, nil
}

func compactNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}
