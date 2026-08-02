package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AgentSnapshot is the durable identity of the agent used for a run.
// It stores only vendor/model/profile/reasoning identity — never params or env.
//
// Authority: when runs.agent_snapshot_json is non-empty with a vendor, this
// snapshot is execution authority for spawn, prompts, HITL, disclosure, and
// interactive takeover on that run lineage. It is not the agent's structured
// output: vendor/model/reasoning effort is operator/config policy captured at
// run create, not something the agent emits.
//
// Trade-off: costs a persisted column, sticky copy across failed/interrupted
// retries, parse validation, and a legacy-null fallback path. Simpler options
// fail the sticky-identity contract — re-resolving live config mid-lineage can
// switch CLI vendor after hot reload (breaking native resume and params
// ownership), and agent output cannot authoritatively choose the executable.
type AgentSnapshot struct {
	Vendor          string           `json:"vendor"`
	Model           *string          `json:"model,omitempty"`
	ProfileID       string           `json:"profileId,omitempty"`
	ReasoningEffort *ReasoningEffort `json:"reasoningEffort,omitempty"`
}

// AgentSnapshotFromResolved builds a snapshot from a resolved coding-role agent.
func AgentSnapshotFromResolved(r ResolvedAgent) AgentSnapshot {
	return AgentSnapshot{
		Vendor:          string(r.Vendor),
		Model:           r.Model,
		ProfileID:       strings.TrimSpace(r.ProfileID),
		ReasoningEffort: r.ReasoningEffort,
	}
}

// AgentSnapshotFromIdentity builds a snapshot from frozen runner identity fields.
// model is a pointer so nil (unset) and non-nil empty (explicit suppress to the
// vendor default) stay distinct through freeze; collapsing empty to omitempty
// nil would make ParamsForRoleVendor preserve params --model/-m on thaw.
func AgentSnapshotFromIdentity(vendor string, model *string, profileID string, reasoningEffort *ReasoningEffort) AgentSnapshot {
	snapshot := AgentSnapshot{
		Vendor:    strings.TrimSpace(vendor),
		ProfileID: strings.TrimSpace(profileID),
	}
	if model != nil {
		trimmed := strings.TrimSpace(*model)
		snapshot.Model = &trimmed
	}
	snapshot.ReasoningEffort = reasoningEffort
	return snapshot
}

// MarshalAgentSnapshot encodes a snapshot as JSON text for runs.agent_snapshot_json.
func MarshalAgentSnapshot(s AgentSnapshot) (string, error) {
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal agent snapshot: %w", err)
	}
	return string(encoded), nil
}

// ParseAgentSnapshot decodes runs.agent_snapshot_json.
func ParseAgentSnapshot(raw string) (AgentSnapshot, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return AgentSnapshot{}, fmt.Errorf("agent snapshot is empty")
	}
	var snapshot AgentSnapshot
	if err := json.Unmarshal([]byte(trimmed), &snapshot); err != nil {
		return AgentSnapshot{}, fmt.Errorf("parse agent snapshot: %w", err)
	}
	return snapshot, nil
}

// ResolveRunAgentSnapshotJSON picks the durable agent snapshot for a new run.
// sticky is true when continuing any failed/interrupted predecessor run (any
// step, including first-step retries). A non-empty predecessor snapshot is
// copied so identity stays sticky across the retry lineage, but only after
// parse + non-empty vendor validation (invalid predecessor fails loudly).
// Otherwise the snapshot is built from the runner's frozen vendor/model/profile.
// model is a pointer so explicit empty suppress survives freeze.
// legacyResume is true when continuing a predecessor that had no snapshot (pre-migration).
// Marshal failures return an error so callers can fail run creation loudly.
func ResolveRunAgentSnapshotJSON(predecessorSnapshot *string, sticky bool, vendor string, model *string, profileID string, reasoningEffort *ReasoningEffort) (snapshotJSON *string, legacyResume bool, err error) {
	if sticky {
		if predecessorSnapshot != nil {
			if trimmed := strings.TrimSpace(*predecessorSnapshot); trimmed != "" {
				snapshot, parseErr := ParseAgentSnapshot(trimmed)
				if parseErr != nil {
					return nil, false, parseErr
				}
				if strings.TrimSpace(snapshot.Vendor) == "" {
					return nil, false, fmt.Errorf("agent snapshot missing vendor")
				}
				copied := trimmed
				return &copied, false, nil
			}
		}
		legacyResume = true
	}
	if strings.TrimSpace(vendor) == "" {
		// No durable identity to freeze; leave null so IdentityFromRunSnapshot
		// can fall back to the runner's live fields (tests / unconfigured agent).
		return nil, legacyResume, nil
	}
	encoded, marshalErr := MarshalAgentSnapshot(AgentSnapshotFromIdentity(vendor, model, profileID, reasoningEffort))
	if marshalErr != nil {
		return nil, legacyResume, marshalErr
	}
	return &encoded, legacyResume, nil
}

// ResolveRunAgentSnapshotJSONForValidationGate picks the durable agent snapshot
// for a new run, refreshing a stale predecessor snapshot that the validation
// gate cannot serve.
//
// When sticky and requireToolNetworkDenial is true, a predecessor snapshot whose
// vendor cannot deny tool network access is abandoned in favor of a fresh
// snapshot built from the current role identity — but only when the current
// role vendor CAN serve the gate AND the resume will actually replay an agent
// step (replaysAgentStep). Without this refresh, switching the role vendor to a
// supported one and retrying would keep spawning the unsupported snapshotted
// vendor (the snapshot is execution authority) and repeat the same permanent
// hold; the sticky-identity contract would defeat the recovery action the
// spawn diagnostic recommends, leaving the operator to disable the gate or
// abandon the lineage.
//
// replaysAgentStep gates the refresh because the snapshot is also the disclosure
// authority: PR/issue stamps derive from it via disclosureIdentity. When a
// resume advances past the agent step (e.g. a worker predecessor whose
// LastCompletedStep is "execute" resumes at "validate"), no agent process runs
// under the gate, so refreshing the snapshot would only rewrite disclosure
// identity — publishing the predecessor's authored changes stamped as the
// current vendor's. The predecessor snapshot is preserved in that case so
// attribution stays with the author that actually ran. The caller is the
// authority for replaysAgentStep: it knows the workflow's agent step and
// whether the resume start step reaches it.
//
// Authority: the current role vendor (operator/config policy captured at run
// create) is the authority for the refresh, not the agent's structured output.
// The predecessor snapshot is abandoned only when it is structurally
// incompatible with the configured gate AND an agent will replay; otherwise
// stickiness is preserved.
//
// Trade-off: refreshing abandons the predecessor's native-resume session, but
// the predecessor vendor cannot spawn under the gate anyway, so native resume
// with that vendor was already impossible. The spawn's native-resume vendor
// check (latest.Vendor != cfg.Vendor) keeps the refreshed run from adopting the
// abandoned session, so it starts a fresh checkpoint instead. When the current
// role vendor is also unsupported, no refresh happens and the spawn refuses with
// a manual-intervention diagnostic — the operator must switch to a supported
// vendor or set validation.optOut=true.
//
// What it still does not catch: a predecessor snapshot whose vendor supports the
// gate but whose CLI session is stale for unrelated reasons — that stays a
// native-resume fallback problem, not a snapshot-refresh problem.
//
// refreshed is true when the predecessor snapshot was replaced; legacyResume is
// true when continuing a predecessor that had no snapshot (pre-migration).
func ResolveRunAgentSnapshotJSONForValidationGate(predecessorSnapshot *string, sticky, requireToolNetworkDenial, replaysAgentStep bool, vendor string, model *string, profileID string, reasoningEffort ...*ReasoningEffort) (snapshotJSON *string, refreshed, legacyResume bool, err error) {
	if sticky && requireToolNetworkDenial && replaysAgentStep && predecessorSnapshot != nil {
		if trimmed := strings.TrimSpace(*predecessorSnapshot); trimmed != "" {
			predecessor, parseErr := ParseAgentSnapshot(trimmed)
			if parseErr != nil {
				return nil, false, false, parseErr
			}
			predecessorVendor := AgentVendor(strings.TrimSpace(predecessor.Vendor))
			currentVendor := AgentVendor(strings.TrimSpace(vendor))
			if predecessorVendor != "" && !VendorSupportsToolNetworkDenial(predecessorVendor) && VendorSupportsToolNetworkDenial(currentVendor) {
				var effort *ReasoningEffort
				if len(reasoningEffort) > 0 {
					effort = reasoningEffort[0]
				}
				encoded, marshalErr := MarshalAgentSnapshot(AgentSnapshotFromIdentity(vendor, model, profileID, effort))
				if marshalErr != nil {
					return nil, false, false, marshalErr
				}
				return &encoded, true, false, nil
			}
		}
	}
	base, legacy, err := ResolveRunAgentSnapshotJSON(predecessorSnapshot, sticky, vendor, model, profileID, nil)
	if err != nil {
		return nil, false, false, err
	}
	return base, false, legacy, nil
}

// IdentityFromRunSnapshot returns the vendor/model/profile/reasoning effort that
// should drive a run.
// When snapshotJSON is non-empty and has a non-empty vendor it is the authority
// (fromSnapshot=true). Malformed non-empty snapshots, or snapshots with an empty
// vendor, return an error (do not fall back to live identity).
// Only empty/null snapshots fall back to the runner's frozen identity
// (fromSnapshot=false) for pre-migration legacy runs.
// model is a pointer so nil (unset) and non-nil empty (suppress) stay distinct.
func IdentityFromRunSnapshot(snapshotJSON *string, fallbackVendor string, fallbackModel *string, fallbackProfile string, fallbackReasoningEffort *ReasoningEffort) (vendor string, model *string, profile string, reasoningEffort *ReasoningEffort, fromSnapshot bool, err error) {
	fallbackVendor = strings.TrimSpace(fallbackVendor)
	fallbackProfile = strings.TrimSpace(fallbackProfile)
	if snapshotJSON == nil || strings.TrimSpace(*snapshotJSON) == "" {
		return fallbackVendor, fallbackModel, fallbackProfile, fallbackReasoningEffort, false, nil
	}
	snapshot, parseErr := ParseAgentSnapshot(*snapshotJSON)
	if parseErr != nil {
		return "", nil, "", nil, false, parseErr
	}
	vendor = strings.TrimSpace(snapshot.Vendor)
	if vendor == "" {
		return "", nil, "", nil, false, fmt.Errorf("agent snapshot missing vendor")
	}
	if snapshot.Model != nil {
		m := strings.TrimSpace(*snapshot.Model)
		model = &m
	}
	profile = strings.TrimSpace(snapshot.ProfileID)
	return vendor, model, profile, snapshot.ReasoningEffort, true, nil
}
