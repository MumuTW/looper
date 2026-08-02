package gatekeeper

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
)

// trustRunner builds a runner for the given trust level, mirroring the inline
// runner in the discovery tests.
func trustRunner(fixture *gatekeeperFixture, trust config.GatekeeperTrustLevel) *Runner {
	return New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return trust },
	})
}

func TestReconcileRecordsMergeEvidenceForOutOfPageMergedRoute(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}
	// The pull request merged through Mergify: it is gone from the open list
	// (outside the discovery page) and GitHub reports it closed with a merge
	// timestamp.
	fixture.github.openPullRequests = nil
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{
		Number: 42, State: "closed", MergedAt: "2026-07-30T10:05:00.000Z",
	}
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("reconcile discovery() error = %v", err)
	}

	mergeOutcomes := func() []MergeOutcome {
		events, err := fixture.repos.Events.List(context.Background(), 100)
		if err != nil {
			t.Fatalf("Events.List() error = %v", err)
		}
		var outcomes []MergeOutcome
		for _, event := range events {
			if event.EventType != MergeOutcomeEventType {
				continue
			}
			var outcome MergeOutcome
			if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
				t.Fatalf("decode merge outcome: %v", err)
			}
			outcomes = append(outcomes, outcome)
		}
		return outcomes
	}
	outcomes := mergeOutcomes()
	if len(outcomes) != 1 {
		t.Fatalf("merge outcome events = %d, want exactly one for the Mergify merge", len(outcomes))
	}
	if !outcomes[0].Merged || outcomes[0].HeadSHA != "head-1" || outcomes[0].PRNumber != 42 {
		t.Fatalf("merge outcome = %#v, want merged head-1 for PR 42", outcomes[0])
	}

	// The merged route must be retired: auto-merge removed, durable queue veto
	// applied, and the route marked revoked so the pass is idempotent.
	if !slices.Equal(fixture.github.labelRemoves[len(fixture.github.labelRemoves)-1].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("last label removal = %#v, want stale auto-merge route retired", fixture.github.labelRemoves)
	}
	if !slices.Equal(fixture.github.labelAdds[len(fixture.github.labelAdds)-1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("last label add = %#v, want durable needs-human-review veto", fixture.github.labelAdds)
	}

	// A second tick must not re-read the merged route (idempotency): no label
	// churn and no duplicate merge evidence.
	before := len(fixture.github.labelRemoves)
	fixture.github.openPullRequests = nil
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("second reconcile discovery() error = %v", err)
	}
	if len(fixture.github.labelRemoves) != before {
		t.Fatalf("label removals grew from %d to %d on second tick, want idempotent reconcile", before, len(fixture.github.labelRemoves))
	}
	if refreshed := mergeOutcomes(); len(refreshed) != 1 {
		t.Fatalf("merge outcome events = %d after second tick, want still exactly one (no duplicate evidence)", len(refreshed))
	}
}

func TestReconcileRetiresOutOfPageRouteOnDemotion(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}

	// The operator demotes the project to observe. The routed pull request is
	// still open but now beyond the discovery page, so only the reconcile pass
	// can retire its route; the inputs no longer permit the route.
	fixture.github.openPullRequests = nil
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1"}
	demoted := trustRunner(fixture, config.GatekeeperTrustObserve)
	if _, err := demoted.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("demoted reconcile discovery() error = %v", err)
	}
	if !slices.Equal(fixture.github.labelRemoves[len(fixture.github.labelRemoves)-1].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("last label removal = %#v, want auto-merge route retired on demotion", fixture.github.labelRemoves)
	}
	if !slices.Equal(fixture.github.labelAdds[len(fixture.github.labelAdds)-1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("last label add = %#v, want durable needs-human-review veto on demotion", fixture.github.labelAdds)
	}
}

func TestRevokeProjectRoutesRetiresPublishedRoutes(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}
	if err := runner.RevokeProjectRoutes(context.Background(), "project_1"); err != nil {
		t.Fatalf("RevokeProjectRoutes() error = %v", err)
	}

	// The published auto-merge route must be retired with the durable veto.
	if !slices.Equal(fixture.github.labelRemoves[len(fixture.github.labelRemoves)-1].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("last label removal = %#v, want auto-merge route retired on project revocation", fixture.github.labelRemoves)
	}
	if !slices.Equal(fixture.github.labelAdds[len(fixture.github.labelAdds)-1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("last label add = %#v, want durable needs-human-review veto on project revocation", fixture.github.labelAdds)
	}

	// The route must be marked revoked so a later re-entry re-evaluates rather
	// than reusing the stale published report.
	reports, err := latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() error = %v", err)
	}
	report, ok := reports["acme/looper#42"]
	if !ok {
		t.Fatalf("no gate report for acme/looper#42 after revocation")
	}
	if !hasReason(report, ReasonRouteRevoked) {
		t.Fatalf("report after revocation = %#v, want ReasonRouteRevoked marker", report)
	}
}
