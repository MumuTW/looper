package github

import "testing"

func TestParseMergeabilityStateNormalizesAndClassifiesProviderValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		raw           string
		want          MergeabilityState
		wantKnown     bool
		wantClean     bool
		wantDirty     bool
		wantUnstable  bool
		wantAmbiguous bool
	}{
		{name: "clean", raw: " CLEAN ", want: MergeabilityStateClean, wantKnown: true, wantClean: true},
		{name: "conflict", raw: "DIRTY", want: MergeabilityStateDirty, wantKnown: true, wantDirty: true},
		{name: "unstable", raw: "unstable", want: MergeabilityStateUnstable, wantKnown: true, wantUnstable: true, wantAmbiguous: true},
		{name: "blocked", raw: "blocked", want: MergeabilityStateBlocked, wantKnown: true, wantAmbiguous: true},
		{name: "has hooks", raw: "has_hooks", want: MergeabilityStateHasHooks, wantKnown: true, wantAmbiguous: true},
		{name: "behind", raw: "behind", want: MergeabilityStateBehind, wantKnown: true},
		{name: "unblocked is not a GitHub enum value", raw: "unblocked", want: "unblocked"},
		{name: "explicit unknown", raw: "unknown", want: MergeabilityStateUnknown},
		{name: "empty", raw: "  ", want: ""},
		{name: "future provider value", raw: " future_state ", want: "future_state"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ParseMergeabilityState(tc.raw)
			if got != tc.want || got.Raw() != string(tc.want) {
				t.Fatalf("ParseMergeabilityState(%q) = %q, want %q", tc.raw, got.Raw(), tc.want)
			}
			if got.IsKnown() != tc.wantKnown || got.IsClean() != tc.wantClean || got.HasConflict() != tc.wantDirty || got.IsUnstable() != tc.wantUnstable || got.IsAmbiguousPolicyState() != tc.wantAmbiguous {
				t.Fatalf("state predicates for %q = known:%v clean:%v dirty:%v unstable:%v ambiguous:%v", got.Raw(), got.IsKnown(), got.IsClean(), got.HasConflict(), got.IsUnstable(), got.IsAmbiguousPolicyState())
			}
			if got.IsUnknown() == tc.wantKnown {
				t.Fatalf("IsUnknown() = %v for %q, want %v", got.IsUnknown(), got.Raw(), !tc.wantKnown)
			}
		})
	}
}

func TestPullRequestDetailParsesMergeabilityAtGatewayBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		row  map[string]any
		want MergeabilityState
	}{
		{name: "gh view field", row: map[string]any{"mergeStateStatus": "DIRTY"}, want: MergeabilityStateDirty},
		{name: "rest field", row: map[string]any{"mergeable_state": "unstable"}, want: MergeabilityStateUnstable},
		{name: "rest field wins", row: map[string]any{"mergeable_state": "clean", "mergeStateStatus": "DIRTY"}, want: MergeabilityStateClean},
		{name: "unknown preserved", row: map[string]any{"mergeStateStatus": "future_state"}, want: "future_state"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pullRequestDetailFromViewRow(tc.row, nil, nil).MergeableState
			if got != tc.want || got.Raw() != string(tc.want) {
				t.Fatalf("MergeableState = %q, want %q", got.Raw(), tc.want)
			}
		})
	}
}
