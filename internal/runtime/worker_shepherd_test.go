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
	// changes requested → fix
	cr := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", Checks: green}
	if !shepherdActionable(cr) || shepherdPhaseFor(cr) != "fixing" {
		t.Fatalf("CHANGES_REQUESTED should be actionable/fixing")
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
	// approved + green + clean → NOT actionable (bot waits for human merge), phase awaiting_merge
	ready := githubinfra.PullRequestDetail{State: "OPEN", ReviewDecision: "APPROVED", Checks: green}
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
		{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T00:00:00Z"},
		{"state": "CHANGES_REQUESTED", "submittedAt": "2026-07-08T09:00:00Z"}, // nettee re-reviewed at the same head
	}
	if foldShepherdSignal(base) == foldShepherdSignal(round2) {
		t.Fatal("a new review round at the same head/decision must change the signal (else re-review is missed)")
	}
	if !shepherdActionable(round2) {
		t.Fatal("CHANGES_REQUESTED must stay actionable")
	}
}
