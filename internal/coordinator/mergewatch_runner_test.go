package coordinator

import (
	"context"
	"testing"

	"github.com/MumuTW/looper/internal/coordinator/mergewatch"
	"github.com/MumuTW/looper/internal/eventlog"
)

// TestLinkedPullRequestNumbersReadsProjectedCrossReference covers the
// gateway-to-coordinator lifecycle contract for issue #543's projection fix.
// ListIssueTimeline projects cross-referenced events down to the minimal
// identifying fields (number, html_url, url, and the nested pull_request
// marker) and stores those rows in rawTimeline. linkedPullRequestNumbers is
// what resolveWatchedPR and applyMarkReady use to discover the PR a
// cross-reference points at, so the projected shape must still yield the PR
// number — otherwise merge-watch and mark-ready are silently disabled for
// every issue whose linked PR is only reachable through a cross-referenced
// event. A cross-reference to a plain issue (no pull_request marker) must not
// be counted as a PR.
func TestLinkedPullRequestNumbersReadsProjectedCrossReference(t *testing.T) {
	t.Parallel()
	timeline := []map[string]any{
		// Projected cross-referenced event pointing at a linked PR: the source
		// issue carries the PR number, URL, and the pull_request marker that
		// distinguishes a PR from a plain issue.
		{
			"event":        "cross-referenced",
			"created_at":   "2026-05-16T10:00:00Z",
			"label":        nil,
			"source":       map[string]any{"issue": map[string]any{"number": int64(42), "html_url": "https://github.com/acme/looper/pull/42", "url": "https://api.github.com/repos/acme/looper/pulls/42", "pull_request": map[string]any{"number": int64(42), "html_url": "https://github.com/acme/looper/pull/42", "url": "https://api.github.com/repos/acme/looper/pulls/42"}}},
			"pull_request": nil,
			"issue":        nil,
		},
		// Projected cross-referenced event pointing at a plain issue: no
		// pull_request marker, and the html_url is an issue URL, so this must
		// not be reported as a linked PR.
		{
			"event":        "cross-referenced",
			"created_at":   "2026-05-17T10:00:00Z",
			"label":        nil,
			"source":       map[string]any{"issue": map[string]any{"number": int64(50), "html_url": "https://github.com/acme/looper/issues/50", "url": nil, "pull_request": nil}},
			"pull_request": nil,
			"issue":        nil,
		},
		// A referenced event carrying a top-level pull_request URL, the other
		// shape linkedPullRequestNumbers walks.
		{
			"event":        "referenced",
			"created_at":   "2026-05-18T10:00:00Z",
			"label":        nil,
			"source":       nil,
			"pull_request": map[string]any{"number": int64(7), "html_url": "https://github.com/acme/looper/pull/7", "url": "https://api.github.com/repos/acme/looper/pulls/7"},
			"issue":        nil,
		},
	}
	got := linkedPullRequestNumbers(timeline)
	want := []int64{42, 7}
	if len(got) != len(want) {
		t.Fatalf("linkedPullRequestNumbers() = %v, want %v (PR 42 from the cross-reference, PR 7 from the referenced event; the plain issue 50 must be excluded)", got, want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Fatalf("linkedPullRequestNumbers()[%d] = %d, want %d; full result = %v", i, got[i], n, got)
		}
	}
}

func TestRecordPostMergeEventPreservesForgeMergedAt(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	forgeMergedAt := "2026-05-14T11:58:07.000Z"
	if err := fixture.runner.recordPostMergeEvent(context.Background(), fixture.projectID, "acme/looper", 42, mergewatch.PRSnapshot{
		Repo: "acme/looper", PRNumber: 42, HeadSHA: "head-42", Merged: true, MergedAt: forgeMergedAt,
	}); err != nil {
		t.Fatalf("recordPostMergeEvent() error = %v", err)
	}
	events, err := fixture.runner.repos.Events.ListByEntity(context.Background(), "pull_request", "acme/looper#42")
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v, err=%v, want one merge event", events, err)
	}
	if events[0].EventType != eventlog.CoordinatorPullRequestMergedEventType || events[0].CreatedAt != "2026-05-14T12:00:00.000Z" {
		t.Fatalf("event = %#v, want durable observation timestamp", events[0])
	}
	if !containsAll(events[0].PayloadJSON, `"mergedAt":"`+forgeMergedAt+`"`, `"headSha":"head-42"`) {
		t.Fatalf("payload = %s, want forge mergedAt and head", events[0].PayloadJSON)
	}
}
