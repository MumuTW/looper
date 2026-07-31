package reconcile

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/MumuTW/looper/internal/lifecycle"
)

// The State JSON keys are a persisted checkpoint contract: existing
// databases hold these exact keys, including the historical
// "committedByLooperd" for CommittedByLoop. A rename breaks resume.
func TestStatePersistedShapeIsStable(t *testing.T) {
	t.Parallel()

	state := State{
		BaseHeadSHA:      "base",
		FinalHeadSHA:     "final",
		NewCommitSHAs:    []string{"c1"},
		CommittedByAgent: true,
		CommittedByLoop:  true,
		WorkingTreeClean: true,
		ChangedFiles:     []string{"a.go"},
		CompletedAt:      "2026-07-30T12:00:00.000Z",
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"baseHeadSha":"base","finalHeadSha":"final","newCommitShas":["c1"],"committedByAgent":true,"committedByLooperd":true,"workingTreeClean":true,"changedFiles":["a.go"],"completedAt":"2026-07-30T12:00:00.000Z"}`
	if string(encoded) != want {
		t.Fatalf("Marshal() = %s, want %s", encoded, want)
	}

	var decoded State
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.BaseHeadSHA != state.BaseHeadSHA || !decoded.CommittedByLoop || !slices.Equal(decoded.NewCommitSHAs, state.NewCommitSHAs) {
		t.Fatalf("round-trip = %+v, want %+v", decoded, state)
	}
}

func TestBaseHeadSHAIsNilSafe(t *testing.T) {
	t.Parallel()

	if got := BaseHeadSHA(nil); got != "" {
		t.Fatalf("BaseHeadSHA(nil) = %q, want empty", got)
	}
	if got := BaseHeadSHA(&State{BaseHeadSHA: "base"}); got != "base" {
		t.Fatalf("BaseHeadSHA(state) = %q, want base", got)
	}
}

func TestIsComplete(t *testing.T) {
	t.Parallel()

	if IsComplete(nil) {
		t.Fatal("IsComplete(nil) = true, want false")
	}
	if IsComplete(&State{FinalHeadSHA: "final"}) {
		t.Fatal("IsComplete(no CompletedAt) = true, want false: only a completion timestamp marks a finished pass")
	}
	if !IsComplete(&State{CompletedAt: "2026-07-30T12:00:00.000Z"}) {
		t.Fatal("IsComplete(completed) = false, want true")
	}
}

func TestSelectBasePrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		state        *State
		worktreeBase string
		worktreeHead string
		want         string
	}{
		{name: "recorded base wins so re-reconciles keep the original diff base", state: &State{BaseHeadSHA: "recorded"}, worktreeBase: "prepared", worktreeHead: "head", want: "recorded"},
		{name: "prepared worktree base beats its head", state: nil, worktreeBase: "prepared", worktreeHead: "head", want: "prepared"},
		{name: "worktree head is the last resort", state: &State{}, worktreeBase: "", worktreeHead: "head", want: "head"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SelectBase(tc.state, tc.worktreeBase, tc.worktreeHead); got != tc.want {
				t.Fatalf("SelectBase() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompleteClassifiesThePass(t *testing.T) {
	t.Parallel()

	initial := Inspection{HeadSHA: "mid", NewCommitSHAs: []string{"agent1"}, HasUncommittedChanges: true}
	final := Inspection{HeadSHA: "final", NewCommitSHAs: []string{"agent1", "loop1"}, ChangedFiles: []string{"a.go"}, HasUncommittedChanges: false}
	state := Complete("base", initial, final, true, "2026-07-30T12:00:00.000Z")
	want := &State{
		BaseHeadSHA:      "base",
		FinalHeadSHA:     "final",
		NewCommitSHAs:    []string{"agent1", "loop1"},
		CommittedByAgent: true,
		CommittedByLoop:  true,
		WorkingTreeClean: true,
		ChangedFiles:     []string{"a.go"},
		CompletedAt:      "2026-07-30T12:00:00.000Z",
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("Complete() = %+v, want %+v", state, want)
	}

	// A pass where the agent committed nothing and the loop committed nothing:
	// no attribution flags, dirty tree recorded as not clean.
	idle := Complete("base", Inspection{}, Inspection{HeadSHA: "base", HasUncommittedChanges: true}, false, "t")
	if idle.CommittedByAgent || idle.CommittedByLoop || idle.WorkingTreeClean {
		t.Fatalf("Complete(idle) = %+v, want no attribution and an unclean tree", idle)
	}
}

func TestCompleteCopiesInspectionSlices(t *testing.T) {
	t.Parallel()

	shas := []string{"c1"}
	state := Complete("base", Inspection{}, Inspection{NewCommitSHAs: shas}, false, "t")
	shas[0] = "tampered"
	if state.NewCommitSHAs[0] != "c1" {
		t.Fatalf("State.NewCommitSHAs aliases the inspection slice: %v", state.NewCommitSHAs)
	}
}

func TestRefreshReclassifiesLaterInspectionAndPreservesPassAuthority(t *testing.T) {
	t.Parallel()

	previous := &State{
		BaseHeadSHA:      "base",
		FinalHeadSHA:     "repair",
		NewCommitSHAs:    []string{"repair"},
		CommittedByAgent: true,
		CommittedByLoop:  true,
		WorkingTreeClean: true,
		ChangedFiles:     []string{"repair.go"},
		CompletedAt:      "before",
	}
	shas := []string{"repair", "validation"}
	refreshed := Refresh(previous, Inspection{HeadSHA: "validation", NewCommitSHAs: shas, ChangedFiles: []string{"generated.go"}}, "after")
	if refreshed == previous {
		t.Fatal("Refresh() returned the prior State after the head changed")
	}
	if refreshed.BaseHeadSHA != "base" || refreshed.FinalHeadSHA != "validation" || !refreshed.CommittedByAgent || !refreshed.CommittedByLoop || !refreshed.WorkingTreeClean || refreshed.CompletedAt != "after" {
		t.Fatalf("Refresh() = %+v, want refreshed head-derived state with preserved base and loop attribution", refreshed)
	}
	if !slices.Equal(refreshed.NewCommitSHAs, shas) || !slices.Equal(refreshed.ChangedFiles, []string{"generated.go"}) {
		t.Fatalf("Refresh() = %+v, want refreshed commit and changed-file evidence", refreshed)
	}
	shas[1] = "tampered"
	if refreshed.NewCommitSHAs[1] != "validation" {
		t.Fatalf("Refresh() aliases the inspection slice: %v", refreshed.NewCommitSHAs)
	}
	if got := Refresh(refreshed, Inspection{HeadSHA: "validation"}, "later"); got != refreshed {
		t.Fatalf("Refresh(same head) = %#v, want the existing State %#v", got, refreshed)
	}
}

func TestCommitAttribution(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		hasNewCommits   bool
		committedByLoop bool
		current         string
		want            string
	}{
		{name: "no commits leave attribution untouched", hasNewCommits: false, committedByLoop: true, current: lifecycle.ActionSourceAgent, want: lifecycle.ActionSourceAgent},
		{name: "loop commit is always fallback", hasNewCommits: true, committedByLoop: true, current: lifecycle.ActionSourceAgent, want: lifecycle.ActionSourceFallback},
		{name: "agent commit claims an unclaimed action", hasNewCommits: true, committedByLoop: false, current: lifecycle.ActionSourceNone, want: lifecycle.ActionSourceAgent},
		{name: "agent commit never overwrites an existing claim", hasNewCommits: true, committedByLoop: false, current: lifecycle.ActionSourceFallback, want: lifecycle.ActionSourceFallback},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CommitAttribution(tc.hasNewCommits, tc.committedByLoop, tc.current); got != tc.want {
				t.Fatalf("CommitAttribution(%t, %t, %q) = %q, want %q", tc.hasNewCommits, tc.committedByLoop, tc.current, got, tc.want)
			}
		})
	}
}

func TestProducedNewCommits(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		state *State
		want  bool
	}{
		{name: "nil state produced nothing", state: nil, want: false},
		{name: "explicit new commits", state: &State{NewCommitSHAs: []string{"c1"}}, want: true},
		{name: "head moved off a known base", state: &State{BaseHeadSHA: "base", FinalHeadSHA: "final"}, want: true},
		{name: "head equal to base", state: &State{BaseHeadSHA: "same", FinalHeadSHA: "same"}, want: false},
		{name: "unknown base cannot prove movement", state: &State{FinalHeadSHA: "final"}, want: false},
		{name: "unknown final cannot prove movement", state: &State{BaseHeadSHA: "base"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ProducedNewCommits(tc.state); got != tc.want {
				t.Fatalf("ProducedNewCommits(%+v) = %t, want %t", tc.state, got, tc.want)
			}
		})
	}
}
