package runtime

import (
	"testing"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

func check(status, conclusion string) map[string]any {
	return map[string]any{"status": status, "conclusion": conclusion}
}

func TestShepherdTerminalOutcome(t *testing.T) {
	cases := []struct {
		pr   githubinfra.PullRequestDetail
		want string
	}{
		{githubinfra.PullRequestDetail{State: "MERGED"}, "merged"},
		{githubinfra.PullRequestDetail{State: "OPEN", MergedAt: "2026-07-08T00:00:00Z"}, "merged"},
		{githubinfra.PullRequestDetail{State: "CLOSED"}, "abandoned"},
		{githubinfra.PullRequestDetail{State: "OPEN"}, ""},
	}
	for _, c := range cases {
		if got := shepherdTerminalOutcome(c.pr); got != c.want {
			t.Fatalf("shepherdTerminalOutcome(%+v) = %q, want %q", c.pr, got, c.want)
		}
	}
}

func TestShepherdCIPhase(t *testing.T) {
	if got := shepherdCIPhase(githubinfra.PullRequestDetail{Checks: []map[string]any{check("IN_PROGRESS", "")}}); got != "pending" {
		t.Fatalf("pending CI = %q", got)
	}
	if got := shepherdCIPhase(githubinfra.PullRequestDetail{Checks: []map[string]any{check("COMPLETED", "SUCCESS"), check("COMPLETED", "FAILURE")}}); got != "failed" {
		t.Fatalf("failed CI = %q", got)
	}
	if got := shepherdCIPhase(githubinfra.PullRequestDetail{Checks: []map[string]any{check("COMPLETED", "SUCCESS"), check("COMPLETED", "SKIPPED")}}); got != "passed" {
		t.Fatalf("passed CI = %q", got)
	}
}

func TestShepherdActionableAndPhase(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	// changes requested AT THE CURRENT HEAD → fix
	cr := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "h1", Checks: green,
		Reviews: []map[string]any{{"state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": "h1"}}}}
	if !shepherdActionable(cr) || shepherdPhaseFor(cr) != "fixing" {
		t.Fatalf("CHANGES_REQUESTED on current head should be actionable/fixing")
	}
	// changes requested on an OLD head (we already pushed a fix, awaiting re-review) → NOT actionable
	stale := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "h2", Checks: green,
		Reviews: []map[string]any{{"state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": "h1"}}, {"state": "COMMENTED", "commit": map[string]any{"oid": "h2"}}}}
	if shepherdActionable(stale) {
		t.Fatalf("CHANGES_REQUESTED on an OLD head must NOT be actionable — we already responded, awaiting re-review (stops the spin)")
	}
	// conflict → fix
	conflict := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HasConflicts: true, Checks: green}
	if !shepherdActionable(conflict) || shepherdPhaseFor(conflict) != "fixing" {
		t.Fatalf("conflict should be actionable/fixing")
	}
	// failing CI → fix
	failing := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", Checks: []map[string]any{check("COMPLETED", "FAILURE")}}
	if !shepherdActionable(failing) {
		t.Fatalf("failing CI should be actionable")
	}
	// approved AT THE CURRENT HEAD + green + clean → NOT actionable (bot waits for
	// a human to merge), phase awaiting_merge
	ready := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "hr", Checks: green,
		Reviews: []map[string]any{{"state": "APPROVED", "commit": map[string]any{"oid": "hr"}}}}
	if shepherdActionable(ready) {
		t.Fatalf("approved+green+clean must NOT be actionable — the bot never merges, it waits for a human")
	}
	if shepherdPhaseFor(ready) != "awaiting_merge" {
		t.Fatalf("approved+green+clean phase = %q, want awaiting_merge", shepherdPhaseFor(ready))
	}
}

// The wake key must NOT include updatedAt (or any field the agent's own
// comments/pushes bump gratuitously) beyond headSHA, else a fix would
// self-retrigger forever. Two PRs differing only in updatedAt fold to the same
// signal.
func TestFoldShepherdSignalIgnoresUpdatedAt(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	a := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "sha1", Checks: green, UpdatedAt: "2026-07-08T00:00:00Z"}
	b := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "sha1", Checks: green, UpdatedAt: "2026-07-08T09:99:99Z"}
	if foldShepherdSignal(a) != foldShepherdSignal(b) {
		t.Fatalf("signal must ignore updatedAt: %q vs %q", foldShepherdSignal(a), foldShepherdSignal(b))
	}
	// a new head IS a real change
	c := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", HeadSHA: "sha2", Checks: green}
	if foldShepherdSignal(a) == foldShepherdSignal(c) {
		t.Fatal("a new head must change the signal")
	}
}

// A new review round (reviewer submits again) must change the wake signal even at
// the same head with the same aggregate decision — else the shepherd never wakes
// to address the re-review (the live-e2e bug). The agent's own thread replies are
// NOT top-level reviews, so they don't move latestReviewMarker.
func TestSignalCatchesNewReviewRound(t *testing.T) {
	base := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "sha1",
		Checks:  []map[string]any{check("COMPLETED", "SUCCESS")},
		Reviews: []map[string]any{{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T00:00:00Z"}},
	}
	round2 := base
	round2.Reviews = []map[string]any{
		{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T00:00:00Z", "commit": map[string]any{"oid": "sha1"}},
		{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T09:00:00Z", "commit": map[string]any{"oid": "sha1"}}, // nettee re-reviewed at the SAME head
	}
	if foldShepherdSignal(base) == foldShepherdSignal(round2) {
		t.Fatal("a new review round at the same head/decision must change the signal (else re-review is missed)")
	}
	if !shepherdActionable(round2) {
		t.Fatal("CHANGES_REQUESTED at the current head must be actionable")
	}
}

// A stale CHANGES_REQUESTED on an OLD head must NOT block awaiting_merge once the
// CURRENT head is approved + green (the live-e2e case: two colleagues approved
// b8653748 while nettee's old CHANGES_REQUESTED kept the aggregate at
// CHANGES_REQUESTED). The bot then shows 待合并 and waits for a human to merge.
func TestAwaitingMergeIgnoresStaleAggregateDecision(t *testing.T) {
	green := []map[string]any{check("COMPLETED", "SUCCESS")}
	pr := githubinfra.PullRequestDetail{
		State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", HeadSHA: "cur", Checks: green,
		Reviews: []map[string]any{
			{"state": "CHANGES_REQUESTED", "commit": map[string]any{"oid": "old"}}, // stale, old head
			{"state": "APPROVED", "commit": map[string]any{"oid": "cur"}},          // fresh approval of current head
			{"state": "APPROVED", "commit": map[string]any{"oid": "cur"}},
		},
	}
	if shepherdActionable(pr) {
		t.Fatal("approved current head with no change-request-here must NOT be actionable")
	}
	if got := shepherdPhaseFor(pr); got != "awaiting_merge" {
		t.Fatalf("phase = %q, want awaiting_merge (stale aggregate CHANGES_REQUESTED must not wedge it)", got)
	}
}
