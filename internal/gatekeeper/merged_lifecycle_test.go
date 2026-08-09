package gatekeeper

import (
	"context"
	"fmt"

	"testing"

	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

// merge makes the fake forge answer as it does after a merge: the pull request
// is gone from the open list, and a direct read reports MERGED with an
// ambiguous mergeability — which is what a merged pull request actually
// returns.
func merge(fixture *gatekeeperFixture, prNumber int64) {
	fixture.github.openPullRequests = nil
	fixture.github.detail = githubinfra.PullRequestDetail{
		Number: prNumber, State: "MERGED", HeadSHA: "head-1", BaseRefName: "main",
	}
	fixture.github.mergeable = githubinfra.PullRequestDetail{Number: prNumber, HeadSHA: "head-1"}
}

// TestPollingDiscoveryMakesTheMergedStateDurable is the defect the operator
// query hit: with webhooks off — the default — nothing ever reads a pull
// request again once it merges, because discovery lists only open ones. Its
// last durable report therefore says OPEN forever and `state=merged` answers
// empty on a default install.
//
// Nothing in this test involves a webhook. The only entry point is the polling
// discovery lane.
func TestPollingDiscoveryMakesTheMergedStateDurable(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	ctx := context.Background()
	noRequiredReviews(fixture)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{{
		Number: 42, State: "OPEN", HeadSHA: "head-1", UpdatedAt: "2026-07-30T09:00:00Z",
	}}
	// Nothing reviewed it, and CodeRabbit said so instead of reviewing: exactly
	// the pull request the endpoint exists to surface after it merges.
	fixture.github.comments = []githubinfra.CommentInfo{
		{ID: 5, Author: "coderabbitai[bot]", IsBot: true, Body: "Review limit reached"},
	}

	if _, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if merged, err := ListUnreviewed(ctx, fixture.repos, "MERGED"); err != nil || len(merged) != 0 {
		t.Fatalf("merged = %#v, err = %v; want nothing merged while the pull request is open", merged, err)
	}

	// The clock does not move between the two ticks, so the final report shares
	// a timestamp with the open one it replaces. "Latest" has to survive that:
	// event ids are random, so insertion order is the only thing that can order
	// them.
	merge(fixture, 42)
	result, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if result.Reconciled != 1 || result.Evaluated != 0 {
		t.Fatalf("result = %#v, want exactly the departed pull request reconciled", result)
	}

	merged, err := ListUnreviewed(ctx, fixture.repos, "MERGED")
	if err != nil {
		t.Fatalf("ListUnreviewed() error = %v", err)
	}
	if len(merged) != 1 || merged[0].PRNumber != 42 || merged[0].ReviewProvenance.Status != ReviewProvenanceRefused {
		t.Fatalf("merged unreviewed = %#v, want pull request 42 recorded as refused", merged)
	}

	// One-way and self-clearing: the report now says MERGED, so the pull request
	// is not a candidate again and a quiet tick costs nothing.
	quiet, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if quiet.Reconciled != 0 {
		t.Fatalf("second tick reconciled %d, want zero in steady state", quiet.Reconciled)
	}
}

// TestReconciliationLeavesEligibilityUnchanged holds the new lifecycle path to
// the same rule as the observation it exists to persist: it writes a report, it
// does not acquire the power to stop anything. The pull request has already
// merged by the time it runs, and the report it writes is observe-only.
func TestReconciliationLeavesEligibilityUnchanged(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	ctx := context.Background()
	noRequiredReviews(fixture)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{{
		Number: 42, State: "OPEN", HeadSHA: "head-1", UpdatedAt: "2026-07-30T09:00:00Z",
	}}
	open, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(open.Reports) != 1 || !open.Reports[0].Eligible {
		t.Fatalf("open report = %#v, want eligible", open.Reports)
	}

	merge(fixture, 42)
	result, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	if len(result.Reports) != 1 {
		t.Fatalf("reports = %#v, want the one reconciled report", result.Reports)
	}
	// The reconciled report records what it saw and blocks nothing that could
	// still be merged: the pull request it describes is already in the base
	// branch. Every open pull request's verdict is decided by the same
	// evaluation as before — reconciliation only reaches pull requests that
	// have already left the open set.
	final := result.Reports[0]
	if final.Evidence.PullRequestState != "MERGED" {
		t.Fatalf("report = %#v, want the merged state recorded", final)
	}
	if final.Evidence.ReviewProvenance.Status != ReviewProvenanceAbsent {
		t.Fatalf("provenance = %#v, want the final observation recorded", final.Evidence.ReviewProvenance)
	}
	if final.PRNumber != open.Reports[0].PRNumber {
		t.Fatalf("reconciled %d, want the same pull request the open tick evaluated", final.PRNumber)
	}
	reports, err := latestGateReports(ctx, fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() error = %v", err)
	}
	latest := reports["acme/looper#42"]
	if latest.Evidence.PullRequestState != "MERGED" || hasReason(latest, ReasonRouteRevoked) {
		t.Fatalf("latest report = %#v, want fresh terminal report without stale route-revoked overwrite", latest)
	}
}

// TestReconciliationIsBounded pins what stops this from becoming a poller over
// merged history. Absence from a full page is not evidence of departure, and a
// burst cannot make one tick unbounded.
func TestReconciliationIsBounded(t *testing.T) {
	t.Run("a truncated open list reconciles nothing", func(t *testing.T) {
		fixture := newGatekeeperFixture(t)
		ctx := context.Background()
		// One pull request was recorded open; discovery now returns a full page
		// that does not contain it, which is what a paged-off pull request looks
		// like and is indistinguishable from a merge.
		seedGateReportWithProvenance(t, fixture, "acme/looper", 7, "OPEN", ReviewProvenance{Status: ReviewProvenanceAbsent}, fixture.now)
		fixture.github.openPullRequests = openPullRequestPage(3)

		result, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper", Limit: 3})
		if err != nil {
			t.Fatalf("DiscoverPullRequests() error = %v", err)
		}
		if result.Reconciled != 0 {
			t.Fatalf("reconciled = %d, want nothing reconciled from a truncated page", result.Reconciled)
		}
	})

	t.Run("a burst is capped per tick", func(t *testing.T) {
		fixture := newGatekeeperFixture(t)
		ctx := context.Background()
		noRequiredReviews(fixture)
		for prNumber := int64(1); prNumber <= maxReconciledDepartures+5; prNumber++ {
			seedGateReportWithProvenance(t, fixture, "acme/looper", prNumber, "OPEN", ReviewProvenance{Status: ReviewProvenanceAbsent}, fixture.now)
		}
		merge(fixture, 1)

		result, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
		if err != nil {
			t.Fatalf("DiscoverPullRequests() error = %v", err)
		}
		if result.Reconciled != maxReconciledDepartures {
			t.Fatalf("reconciled = %d, want the per-tick cap of %d", result.Reconciled, maxReconciledDepartures)
		}
	})

	t.Run("a pull request Looper never saw open is never revisited", func(t *testing.T) {
		fixture := newGatekeeperFixture(t)
		ctx := context.Background()
		// Merged before this ever ran, and merged again is not a thing: there is
		// no backfill over history, only forward transitions out of the open set.
		seedGateReportWithProvenance(t, fixture, "acme/looper", 9, "MERGED", ReviewProvenance{Status: ReviewProvenanceAbsent}, fixture.now)

		result, err := fixture.runner().DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
		if err != nil {
			t.Fatalf("DiscoverPullRequests() error = %v", err)
		}
		if result.Reconciled != 0 {
			t.Fatalf("reconciled = %d, want no backfill of already-final reports", result.Reconciled)
		}
	})
}

func openPullRequestPage(size int) []githubinfra.PullRequestSummary {
	page := make([]githubinfra.PullRequestSummary, 0, size)
	for i := 0; i < size; i++ {
		page = append(page, githubinfra.PullRequestSummary{
			Number: int64(100 + i), State: "OPEN", HeadSHA: fmt.Sprintf("head-%d", i), UpdatedAt: "2026-07-30T09:00:00Z",
		})
	}
	return page
}
