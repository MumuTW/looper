package loops

import (
	"encoding/json"
	"strings"
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
	// ResumeExecutionID is set when a resume turn starts injecting this answer.
	// Distinguishes the parked ask generation from a later same-text re-escalation
	// written by the agent after the human already answered (crash-recovery).
	ResumeExecutionID string `json:"resumeExecutionId,omitempty"`
	// Transport records how the ask was delivered ("github" | "feishu"). GitHub
	// asks carry the PR + ask-comment id so the answer-poll lane can find the human
	// reply that came after the ask and resolve/re-request on that PR.
	// Transport is the answer channel, not the forge host: "github" means
	// PR-comment answers (on GitHub.com or Forgejo).
	Transport string `json:"transport,omitempty"`
	// Provider is the forge host that received the PR-comment ask
	// ("github" | "forgejo"). Distinct from Transport so the resume poll can
	// call the matching client instead of always using githubinfra.Gateway.
	// Empty means unknown/legacy; poll resolves from project binding.
	Provider     string `json:"provider,omitempty"`
	PRNumber     int64  `json:"prNumber,omitempty"`
	AskCommentID int64  `json:"askCommentId,omitempty"`
	// DeliveryPending is true after a GitHub-transport park commits but before
	// AskCommentID correlation is durable. Startup recovery retries these parks
	// (daemon crash between parkHITLLoop and persistParkedHITLAsk); normal
	// awaiting parks with a correlated comment leave this false so recovery
	// does not requeue answerable HITL loops.
	DeliveryPending bool `json:"deliveryPending,omitempty"`

	// The agent's decision brief — research + recommendation surfaced on the ask
	// card so a human can confirm in seconds instead of researching from scratch.
	Recommendation    string            `json:"recommendation,omitempty"`
	RecommendedOption string            `json:"recommendedOption,omitempty"`
	Consequences      map[string]string `json:"consequences,omitempty"`
	Confidence        string            `json:"confidence,omitempty"`

	// Drift-detection fingerprints (infra signals only — authority remains the
	// human answer / agent structured output). Populated by Fixer HITL asks;
	// worker asks leave them empty.
	//
	// Trade-off (AGENTS.md "new concept"):
	//   Failure prevented: a parked human answer must not authorize action after
	//   the PR head, review thread (incl. non-root replies), or PR title/body
	//   intent drifted while awaiting_human.
	//   Cost: new persisted fields; resume must live-refresh provider content;
	//   not-found vs transient errors must be distinguished; forgejo summary and
	//   GitHub thread shapes need matching ask/resume field layouts; a refresh
	//   failure fails closed (retry) instead of injecting the answer.
	//   Why not simpler: fail-loud without fingerprints still injects a stale
	//   answer on silent drift; trusting agent structured output alone cannot
	//   see mid-park external PR/thread mutations the agent never re-observed.
	HeadSHA                  string `json:"headSha,omitempty"`
	ReviewThreadID           string `json:"reviewThreadId,omitempty"`
	ReviewCommentID          string `json:"reviewCommentId,omitempty"`
	ReviewContentFingerprint string `json:"reviewContentFingerprint,omitempty"`
	PRIntentFingerprint      string `json:"prIntentFingerprint,omitempty"`
	// Role is optional debug metadata ("fixer" | "worker").
	Role string `json:"role,omitempty"`
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
	meta := parseMetadataObject(metadataJSON)
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
	meta := parseMetadataObject(metadataJSON)
	delete(meta, hitlMetadataKey)
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// GitHubAskDeliveryPending reports whether a durable park is waiting on GitHub
// ask delivery / AskCommentID correlation. Used by startup recovery to requeue
// incomplete parks that would otherwise stay awaiting_human forever (poll skips
// AskCommentID==0 and recovery deliberately does not requeue full parks).
func GitHubAskDeliveryPending(ask HITLAsk) bool {
	if !ask.DeliveryPending {
		return false
	}
	if ask.AskCommentID > 0 {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(ask.Transport), "github") {
		return false
	}
	status := strings.TrimSpace(ask.Status)
	return status == "" || status == "awaiting"
}

// AskGenerationMatches reports whether a client payload targets the currently
// parked ask generation.
//
// Rules:
//   - Legacy parks with no executionId/askedAt accept any payload.
//   - When the client omits both tokens, accept the current park (answer-only
//     POST /respond contract — there is only one awaiting ask per loop).
//   - When the client supplies tokens, every non-empty supplied token must match
//     the park and at least one token must match, so a stale card cannot apply
//     its option to a later re-escalation.
func AskGenerationMatches(ask HITLAsk, executionID, askedAt string) bool {
	parkExec := strings.TrimSpace(ask.ExecutionID)
	parkAsked := strings.TrimSpace(ask.AskedAt)
	cardExec := strings.TrimSpace(executionID)
	cardAsked := strings.TrimSpace(askedAt)
	// Legacy parks with no generation identity accept any card (pre-binding).
	if parkExec == "" && parkAsked == "" {
		return true
	}
	// Answer-only clients (curl / scripts for answerTransport=respond) omit
	// generation tokens: authorize against the currently parked ask.
	if cardExec == "" && cardAsked == "" {
		return true
	}
	// Park has a generation and the client supplied tokens: require match.
	matched := false
	if parkExec != "" {
		if cardExec == "" {
			// Card missing execution id while park has one — not a match unless
			// askedAt matches below.
		} else if cardExec != parkExec {
			return false
		} else {
			matched = true
		}
	}
	if parkAsked != "" {
		if cardAsked == "" {
			// missing askedAt
		} else if cardAsked != parkAsked {
			return false
		} else {
			matched = true
		}
	}
	return matched
}

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
