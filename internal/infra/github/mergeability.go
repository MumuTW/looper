package github

import "strings"

// MergeabilityState is GitHub's mergeability state after it crosses the
// gateway boundary. Its string value is the normalized provider value, so an
// unrecognized state remains available as evidence while predicates classify
// it as unknown.
//
// The gateway is the authority for constructing this value. Consumers should
// use the semantic predicates below instead of comparing provider strings.
type MergeabilityState string

const (
	MergeabilityStateUnknown   MergeabilityState = "unknown"
	MergeabilityStateBehind    MergeabilityState = "behind"
	MergeabilityStateBlocked   MergeabilityState = "blocked"
	MergeabilityStateClean     MergeabilityState = "clean"
	MergeabilityStateDirty     MergeabilityState = "dirty"
	MergeabilityStateDraft     MergeabilityState = "draft"
	MergeabilityStateHasHooks  MergeabilityState = "has_hooks"
	MergeabilityStateUnblocked MergeabilityState = "unblocked"
	MergeabilityStateUnstable  MergeabilityState = "unstable"
)

// ParseMergeabilityState normalizes a raw GitHub value once at the gateway
// boundary. Unknown values are intentionally retained rather than collapsed to
// "unknown" so evidence can identify a newly observed provider state.
func ParseMergeabilityState(raw string) MergeabilityState {
	return MergeabilityState(strings.ToLower(strings.TrimSpace(raw)))
}

// Raw returns the normalized provider value for evidence or telemetry.
func (state MergeabilityState) Raw() string {
	return string(state)
}

// IsUnknown reports an empty, explicit unknown, or unrecognized provider state.
func (state MergeabilityState) IsUnknown() bool {
	switch state {
	case MergeabilityStateBehind,
		MergeabilityStateBlocked,
		MergeabilityStateClean,
		MergeabilityStateDirty,
		MergeabilityStateDraft,
		MergeabilityStateHasHooks,
		MergeabilityStateUnblocked,
		MergeabilityStateUnstable:
		return false
	default:
		return true
	}
}

// IsKnown reports whether the provider state is one of the recognized values.
func (state MergeabilityState) IsKnown() bool {
	return !state.IsUnknown()
}

// IsClean reports whether GitHub says the pull request is mergeable without a
// provider-policy blocker.
func (state MergeabilityState) IsClean() bool {
	return state == MergeabilityStateClean
}

// HasConflict reports the provider's explicit merge-conflict state.
func (state MergeabilityState) HasConflict() bool {
	return state == MergeabilityStateDirty
}

// IsUnstable reports the state in which GitHub exposes failing check details.
func (state MergeabilityState) IsUnstable() bool {
	return state == MergeabilityStateUnstable
}

// IsAmbiguousPolicyState reports states whose final meaning depends on
// provider policy or hooks and therefore cannot authorize a merge by itself.
func (state MergeabilityState) IsAmbiguousPolicyState() bool {
	switch state {
	case MergeabilityStateBlocked, MergeabilityStateHasHooks, MergeabilityStateUnstable:
		return true
	default:
		return false
	}
}
