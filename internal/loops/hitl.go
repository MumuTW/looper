package loops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// hitlMetadataKey is where the mid-run HITL ask/answer state lives inside a
// loop's freeform metadata JSON. Using loop metadata avoids a schema migration.
const hitlMetadataKey = "hitl"

// HITLAsk is the persisted state of a mid-run human-in-the-loop question. It is
// written by the runner when the agent asks (Question/Options/SessionID), and
// the answer is filled in by POST /loops/{seq}/respond. On resume the runner
// reads Answer + SessionID to continue the same agent session.
type HITLAsk struct {
	Question    string   `json:"question,omitempty"`
	Options     []string `json:"options,omitempty"`
	SessionID   string   `json:"sessionId,omitempty"`
	ExecutionID string   `json:"executionId,omitempty"`
	Vendor      string   `json:"vendor,omitempty"`
	Answer      string   `json:"answer,omitempty"`
	Status      string   `json:"status,omitempty"` // "awaiting" | "answered" | "consumed"
	AskedAt     string   `json:"askedAt,omitempty"`
	AnsweredAt  string   `json:"answeredAt,omitempty"`
	// Transport records how the ask was delivered ("github" | "feishu"). GitHub
	// asks carry the PR + ask-comment id so the answer-poll lane can find the human
	// reply that came after the ask and resolve/re-request on that PR.
	Transport    string `json:"transport,omitempty"`
	PRNumber     int64  `json:"prNumber,omitempty"`
	AskCommentID int64  `json:"askCommentId,omitempty"`

	// The agent's decision brief — research + recommendation surfaced on the ask
	// card so a human can confirm in seconds instead of researching from scratch.
	Recommendation    string            `json:"recommendation,omitempty"`
	RecommendedOption string            `json:"recommendedOption,omitempty"`
	Consequences      map[string]string `json:"consequences,omitempty"`
	Confidence        string            `json:"confidence,omitempty"`

	// EvidenceHash identifies the malformed sentinel a synthetic ask was raised
	// for (sha256 of the bounded bytes read, or of the symlink target). It is the
	// persisted evidence identity that lets a resumed run recognize the SAME
	// sentinel the human already answered and consume it under human authority
	// instead of re-asking forever when quarantine could not remove the original.
	// Empty for a normal, decodable agent ask and for failures that could not
	// produce a stable fingerprint.
	EvidenceHash string `json:"evidenceHash,omitempty"`
}

// ReadHITLAsk extracts the HITL ask state from a loop's metadata JSON. The
// second return is false when no HITL state is present.
func ReadHITLAsk(metadataJSON *string) (HITLAsk, bool) {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[hitlMetadataKey]
	if !ok {
		return HITLAsk{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return HITLAsk{}, false
	}
	var ask HITLAsk
	if err := json.Unmarshal(encoded, &ask); err != nil {
		return HITLAsk{}, false
	}
	return ask, true
}

// WriteHITLAsk merges the HITL ask state into a loop's metadata JSON, preserving
// all other keys, and returns the updated JSON string.
func WriteHITLAsk(metadataJSON *string, ask HITLAsk) (string, error) {
	meta, err := DecodeMetadataObjectForWrite(metadataJSON)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(ask)
	if err != nil {
		return "", err
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		return "", err
	}
	meta[hitlMetadataKey] = asMap
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ClearHITLAsk removes the HITL ask state from a loop's metadata JSON.
func ClearHITLAsk(metadataJSON *string) (string, error) {
	meta, err := DecodeMetadataObjectForWrite(metadataJSON)
	if err != nil {
		return "", err
	}
	delete(meta, hitlMetadataKey)
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ErrMalformedLoopMetadata marks a stored loop metadata value that no longer
// decodes as a JSON object. Writers refuse to touch such a value: replacing it
// with a fresh map would silently discard worktree, routing, worker, and HITL
// state, so mutation is blocked and the stored value stays intact for
// diagnosis and recovery.
var ErrMalformedLoopMetadata = errors.New("loop metadata is not a valid JSON object")

// parseMetadataObject decodes metadata for read-only accessors. Reads stay
// lenient — malformed metadata simply reads as "no state present", which never
// destroys anything. Writers must use parseMetadataObjectForWrite.
func parseMetadataObject(metadataJSON *string) map[string]any {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return map[string]any{}
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil || meta == nil {
		return map[string]any{}
	}
	return meta
}

// DecodeMetadataObjectForWrite decodes the current metadata ahead of a
// mutation. Existing persisted metadata is part of the loop's durable workflow
// state: a writer must successfully decode the current version before it may
// replace it, so malformed metadata fails the mutation loudly instead of being
// overwritten with an empty object. Every loop-metadata writer — inside this
// package and out (e.g. the worker's PR/worktree merges) — must decode through
// this function.
func DecodeMetadataObjectForWrite(metadataJSON *string) (map[string]any, error) {
	if metadataJSON == nil || strings.TrimSpace(*metadataJSON) == "" {
		return map[string]any{}, nil
	}
	if !utf8.ValidString(*metadataJSON) {
		return nil, fmt.Errorf("%w: invalid UTF-8; stored value left untouched: %s", ErrMalformedLoopMetadata, truncateMetadataForDiagnosis(*metadataJSON))
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(*metadataJSON), &meta); err != nil {
		return nil, fmt.Errorf("%w: %v; stored value left untouched: %s", ErrMalformedLoopMetadata, err, truncateMetadataForDiagnosis(*metadataJSON))
	}
	if meta == nil {
		return nil, fmt.Errorf("%w: JSON null; stored value left untouched: %s", ErrMalformedLoopMetadata, truncateMetadataForDiagnosis(*metadataJSON))
	}
	return meta, nil
}

func truncateMetadataForDiagnosis(value string) string {
	const limit = 200
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
