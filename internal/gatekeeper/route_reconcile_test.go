package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		RepositoryIdentity:  func(string) string { return "ghe.example.test/acme/looper" },
	})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}
	initialReports, err := latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() error = %v", err)
	}
	crashPending := initialReports["acme/looper#42"]
	notEstablished := false
	crashPending.RouteEstablished = &notEstablished
	crashPending.SourceFingerprint = ""
	seedGateReport(t, fixture, crashPending)
	// The pull request merged through Mergify: it is gone from the open list
	// (outside the discovery page) and GitHub reports it closed with a merge
	// timestamp.
	fixture.github.openPullRequests = nil
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{
		Number: 42, State: "closed", MergedAt: "2026-07-30T10:05:00.000Z", MergedBy: "mergify[bot]",
	}
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("reconcile discovery() error = %v", err)
	}
	qualifiedTargetSeen := false
	for _, input := range fixture.github.mergeWatchInputs {
		if input.Repo == "ghe.example.test/acme/looper" {
			qualifiedTargetSeen = true
			break
		}
	}
	if !qualifiedTargetSeen {
		t.Fatalf("merge-watch inputs = %#v, want a provider-qualified repository target", fixture.github.mergeWatchInputs)
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

	// The merged route must be retired without a terminal human-review label,
	// and the route marked revoked so the pass is idempotent.
	if !slices.Equal(fixture.github.labelRemoves[len(fixture.github.labelRemoves)-1].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("last label removal = %#v, want stale auto-merge route retired", fixture.github.labelRemoves)
	}
	if len(fixture.github.labelAdds) != 1 || !slices.Equal(fixture.github.labelAdds[0].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label adds = %#v, want no terminal needs-human-review label", fixture.github.labelAdds)
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

func TestReconcileDoesNotRecordEvidenceForHumanMerge(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}

	fixture.github.openPullRequests = nil
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{
		Number: 42, State: "closed", MergedAt: "2026-07-30T10:05:00.000Z", MergedBy: "maintainer",
	}
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("human-merge reconcile discovery() error = %v", err)
	}

	events, err := fixture.repos.Events.List(context.Background(), 100)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	for _, event := range events {
		if event.EventType == MergeOutcomeEventType {
			t.Fatalf("human merge produced Looper merge evidence: %#v", event)
		}
	}
	if !slices.Equal(fixture.github.labelRemoves[len(fixture.github.labelRemoves)-1].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("last label removal = %#v, want human-merged auto route retired", fixture.github.labelRemoves)
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
	removedAuto := false
	for _, removal := range fixture.github.labelRemoves {
		if slices.Equal(removal.Labels, []string{labels.AutoMerge}) {
			removedAuto = true
			break
		}
	}
	if !removedAuto {
		t.Fatalf("label removals = %#v, want auto-merge route retired on demotion", fixture.github.labelRemoves)
	}
	if !slices.Equal(fixture.github.labelAdds[len(fixture.github.labelAdds)-1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("last label add = %#v, want durable needs-human-review veto on demotion", fixture.github.labelAdds)
	}
}

func TestReconcileReevaluatesOutOfPageRouteAfterHeadChange(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}

	fixture.github.openPullRequests = nil
	fixture.now = fixture.now.Add(outOfPageRouteReconcileInterval)
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-2"}
	fixture.github.detail.HeadSHA = "head-2"
	fixture.github.detail.BaseRefName = "main"
	fixture.policyPermits = true
	fixture.github.finalHeadSHA = "head-2"
	result, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("head-change reconcile discovery() error = %v", err)
	}
	_ = result // out-of-page reconciliation is reported through label/evidence state
	if len(fixture.github.labelAdds) < 2 || !slices.Equal(fixture.github.labelAdds[len(fixture.github.labelAdds)-1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label adds = %#v, want durable veto after head change", fixture.github.labelAdds)
	}
	removedAuto := false
	for _, removal := range fixture.github.labelRemoves {
		if slices.Equal(removal.Labels, []string{labels.AutoMerge}) {
			removedAuto = true
			break
		}
	}
	if !removedAuto {
		t.Fatalf("label removals = %#v, want stale auto-merge route removed", fixture.github.labelRemoves)
	}
}

func TestReconcileRetriesTemporarilyBlockedOutOfPageRoute(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}

	// The route is still open but a required check is temporarily pending. The
	// out-of-page pass must retire the live route without turning that transient
	// blocker into a permanent route_revoked tombstone.
	mergeable := true
	fixture.github.openPullRequests = nil
	fixture.now = fixture.now.Add(outOfPageRouteReconcileInterval)
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{
		Number: 42, State: "OPEN", HeadSHA: "head-1", BaseSHA: "base-1",
		Mergeable: &mergeable, MergeableState: "clean", AdditionsKnown: true, DeletionsKnown: true,
	}
	fixture.github.checks = githubinfra.PullRequestCheckRuns{
		TotalCount: 1,
		CheckRuns:  []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "in_progress", AppID: 15368}},
	}
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("temporary out-of-page block discovery() error = %v", err)
	}
	reports, err := latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() after temporary block: %v", err)
	}
	blocked := reports["acme/looper#42"]
	if !hasReason(blocked, ReasonRouteTemporarilyBlocked) {
		t.Fatalf("temporary block report = %#v, want recheck marker", blocked)
	}
	if hasReason(blocked, ReasonRouteRevoked) {
		t.Fatalf("temporary block report = %#v, must not be permanently revoked", blocked)
	}

	// Once the check recovers, the next cadence window must re-evaluate the
	// still-out-of-page PR and restore the route. A route_revoked marker would
	// skip this read forever.
	fixture.now = fixture.now.Add(outOfPageRouteReconcileInterval)
	fixture.github.checks = githubinfra.PullRequestCheckRuns{
		TotalCount: 1,
		CheckRuns:  []githubinfra.PullRequestCheckRun{{Name: "ci", Status: "completed", Conclusion: "success", AppID: 15368}},
	}
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{}
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("recovered out-of-page discovery() error = %v", err)
	}
	reports, err = latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() after recovery: %v", err)
	}
	recovered := reports["acme/looper#42"]
	if recovered.RouteEstablished == nil || !*recovered.RouteEstablished || hasReason(recovered, ReasonRouteTemporarilyBlocked) || hasReason(recovered, ReasonRouteRevoked) {
		t.Fatalf("recovered report = %#v, want an established route without lifecycle tombstones", recovered)
	}
	if len(fixture.github.labelAdds) == 0 || !slices.Equal(fixture.github.labelAdds[len(fixture.github.labelAdds)-1].Labels, []string{labels.AutoMerge}) {
		t.Fatalf("label adds = %#v, want auto-merge route restored after recovery", fixture.github.labelAdds)
	}
}

func TestReconcileRetiresOutOfPageRouteAfterProviderMove(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	oldProvider := "github.com/acme/looper"
	newProvider := "ghe.example.test/acme/looper"
	oldRunner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		RepositoryIdentity:  func(string) string { return oldProvider },
	})
	if _, err := oldRunner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial provider discovery() error = %v", err)
	}
	initialReports, err := latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() error = %v", err)
	}
	crashPending := initialReports["acme/looper#42"]
	notEstablished := false
	crashPending.RouteEstablished = &notEstablished
	crashPending.SourceFingerprint = ""
	seedGateReport(t, fixture, crashPending)

	// Keep the same owner/slug while moving the project to another forge. The
	// durable route must be retired before the new provider can reuse the PR
	// number; comparing Repo alone would leave the old host authorized.
	fixture.github.openPullRequests = nil
	fixture.github.mergeWatch = githubinfra.PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1"}
	fixture.now = fixture.now.Add(outOfPageRouteReconcileInterval)
	newRunner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool { return fixture.policyPermits },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		RepositoryIdentity:  func(string) string { return newProvider },
	})
	if _, err := newRunner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("provider-move reconcile discovery() error = %v", err)
	}
	removedAuto := false
	for _, removal := range fixture.github.labelRemoves {
		if slices.Equal(removal.Labels, []string{labels.AutoMerge}) {
			removedAuto = true
			break
		}
	}
	if !removedAuto {
		t.Fatalf("label removals = %#v, want old-provider auto-merge route retired", fixture.github.labelRemoves)
	}
	reports, err := latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() error = %v", err)
	}
	if report := reports["acme/looper#42"]; !hasReason(report, ReasonRouteRevoked) {
		t.Fatalf("provider-move report = %#v, want ReasonRouteRevoked marker", report)
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

func TestRevokeProjectRoutesRetiresCrashPendingRoute(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	established := false
	report := Report{
		Version: reportVersion, Mode: string(config.GatekeeperTrustAuto), Eligible: true,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", RouteEstablished: &established,
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}
	seedGateReport(t, fixture, report)

	if err := runner.RevokeProjectRoutes(context.Background(), "project_1"); err != nil {
		t.Fatalf("RevokeProjectRoutes() error = %v", err)
	}
	foundAutoRemoval := false
	for _, removal := range fixture.github.labelRemoves {
		if slices.Equal(removal.Labels, []string{labels.AutoMerge}) {
			foundAutoRemoval = true
		}
	}
	if !foundAutoRemoval {
		t.Fatalf("label removals = %#v, want crash-pending auto-merge route retired", fixture.github.labelRemoves)
	}
	// Out-of-page retirement is label-only: the commit status is commit-wide,
	// so revoking it here would poison a still-eligible sibling sharing the
	// head. The durable needs-human-review veto blocks this PR on its own.
	if len(fixture.github.commitStatuses) != 0 {
		t.Fatalf("commit statuses = %#v, want label-only out-of-page retirement", fixture.github.commitStatuses)
	}
	reports, err := latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() error = %v", err)
	}
	if got := reports["acme/looper#42"]; !hasReason(got, ReasonRouteRevoked) {
		t.Fatalf("report after revocation = %#v, want ReasonRouteRevoked", got)
	}
}

// The label retirement is the merge-blocking authority; a commit-status API
// outage must not keep a retired route alive. When the in-page plan asks for a
// status revocation that fails, the veto label is still added and the trigger
// removed, and the error is returned so the revocation is retried later.
func TestRoutingLabelRetirementSurvivesStatusRevocationFailure(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.statusErr = errors.New("status api unavailable")
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	established := true
	report := Report{
		Version: reportVersion, Mode: string(config.GatekeeperTrustAuto), Eligible: true,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", RouteEstablished: &established,
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}
	seedGateReport(t, fixture, report)

	err := runner.applyRoutingLabelPlan(context.Background(), report, routingLabelPlan{needsHumanReview: true, revokeGateStatus: true})
	if err == nil {
		t.Fatal("applyRoutingLabelPlan() succeeded with the status API failing, want the revocation error returned for retry")
	}
	if len(fixture.github.labelAdds) == 0 || !slices.Equal(fixture.github.labelAdds[len(fixture.github.labelAdds)-1].Labels, []string{labels.NeedsHumanReview}) {
		t.Fatalf("label adds = %#v, want the durable veto applied despite the status outage", fixture.github.labelAdds)
	}
	foundAutoRemoval := false
	for _, removal := range fixture.github.labelRemoves {
		if slices.Equal(removal.Labels, []string{labels.AutoMerge}) {
			foundAutoRemoval = true
		}
	}
	if !foundAutoRemoval {
		t.Fatalf("label removals = %#v, want the queue trigger removed despite the status outage", fixture.github.labelRemoves)
	}
}

// Two-pass invariant for a status revocation that outlives its first attempt:
// pass one installs the durable veto while the status API is down, and the
// retry marker must retain the established-route fact so pass two retries the
// revocation instead of deleting the veto pass one just installed.
func TestBlockedRouteRetriesStatusRevocationAcrossOutage(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)

	// Establish the route while everything works.
	fixture.github.openPullRequests = []githubinfra.PullRequestSummary{openPullRequestFixture()}
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("initial discovery() error = %v", err)
	}
	reports, err := latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() error = %v", err)
	}
	if r := reports["acme/looper#42"]; r.RouteEstablished == nil || !*r.RouteEstablished {
		t.Fatalf("initial report = %#v, want an established route", r)
	}
	// Pass zero legitimately clears a stale veto while establishing the route;
	// every assertion below watches only what happens after it.
	removalsAfterEstablishment := len(fixture.github.labelRemoves)

	// Pass one: the pull request gains a hold label while the status API is
	// down. The veto must still be installed; only the status write is lost.
	fixture.github.detail.Labels = []string{labels.HoldGlobal}
	fixture.github.statusErr = errors.New("status api unavailable")
	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"}); err == nil {
		t.Fatal("pass one hid the routing failure, want the status outage surfaced")
	}
	reports, err = latestGateReports(context.Background(), fixture.repos, "project_1")
	if err != nil {
		t.Fatalf("latestGateReports() after pass one error = %v", err)
	}
	if r := reports["acme/looper#42"]; r.RouteEstablished == nil || !*r.RouteEstablished {
		t.Fatalf("retry marker = %#v, want the established-route fact retained across the status outage", r)
	}
	vetoInstalled := false
	for _, add := range fixture.github.labelAdds {
		if slices.Equal(add.Labels, []string{labels.NeedsHumanReview}) {
			vetoInstalled = true
		}
	}
	if !vetoInstalled {
		t.Fatalf("label adds = %#v, want the durable veto installed despite the status outage", fixture.github.labelAdds)
	}

	// Pass two: the outage is over and the pull request is still blocked. The
	// projection must retry the revocation and keep the veto, never take the
	// removal-only branch that would strand an accepted queue entry.
	fixture.github.statusErr = nil
	second, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1"})
	if err != nil {
		t.Fatalf("pass two error = %v", err)
	}
	if second.Eligible {
		t.Fatalf("pass two report = %#v, want the hold label to keep blocking", second)
	}
	revoked := false
	for _, status := range fixture.github.commitStatuses {
		if status.State == "failure" && status.Context == RequiredStatusContext {
			revoked = true
		}
	}
	if !revoked {
		t.Fatalf("commit statuses = %#v, want the revocation retried after the outage", fixture.github.commitStatuses)
	}
	for _, removal := range fixture.github.labelRemoves[removalsAfterEstablishment:] {
		if slices.Equal(removal.Labels, []string{labels.NeedsHumanReview}) {
			t.Fatalf("label removals = %#v, want the durable veto never deleted across the outage", fixture.github.labelRemoves)
		}
	}
}

// The revalidation retirement wraps its cleanup error beneath the original
// revalidation cause; the pending-revocation state must survive that wrapper
// or persist records RouteEstablished=false and the next blocked evaluation
// deletes the veto the cleanup just installed.
func TestRetireRoutingLabelsPreservesPendingRevocationThroughWrapper(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.statusErr = errors.New("status api unavailable")
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	established := true
	previous := Report{
		Version: reportVersion, Mode: string(config.GatekeeperTrustAuto), Eligible: true,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", RouteEstablished: &established,
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}
	report := previous
	report.Eligible = false

	err := runner.retireRoutingLabels(context.Background(), report, &previous, errors.New("revalidation failed"))
	if err == nil {
		t.Fatal("retireRoutingLabels() succeeded, want the wrapped failure")
	}
	var pending *gatekeeperStatusRevokePendingError
	if !errors.As(err, &pending) {
		t.Fatalf("retireRoutingLabels() error = %v, want the pending-revocation state preserved through the wrapper", err)
	}
	if !strings.Contains(err.Error(), "revalidation failed") {
		t.Fatalf("wrapped error = %v, want the revalidation cause retained", err)
	}
}

// When the status revocation fails and a label mutation fails after it, the
// returned error must still carry the pending-revocation state: the projection
// is unfinished either way, and persist must retain the established route so
// the whole projection is retried rather than deleted.
func TestApplyRoutingLabelPlanRetainsPendingWhenLabelsAlsoFail(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	fixture.github.statusErr = errors.New("status api unavailable")
	fixture.github.labelErr = errors.New("label api unavailable")
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	established := true
	report := Report{
		Version: reportVersion, Mode: string(config.GatekeeperTrustAuto), Eligible: true,
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1", RouteEstablished: &established,
		Evidence: Evidence{PullRequestState: "OPEN", FinalObservedHeadSHA: "head-1"},
	}

	err := runner.applyRoutingLabelPlan(context.Background(), report, routingLabelPlan{needsHumanReview: true, revokeGateStatus: true})
	if err == nil {
		t.Fatal("applyRoutingLabelPlan() succeeded with both APIs failing")
	}
	var pending *gatekeeperStatusRevokePendingError
	if !errors.As(err, &pending) {
		t.Fatalf("applyRoutingLabelPlan() error = %v, want pending-revocation state retained alongside the label failure", err)
	}
}

func TestRevokeProjectRoutesClearsTerminalProjectionRetry(t *testing.T) {
	fixture := newGatekeeperFixture(t)
	runner := trustRunner(fixture, config.GatekeeperTrustAuto)
	report := Report{
		Version: reportVersion, Mode: string(config.GatekeeperTrustAuto),
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		ObservedHeadSHA: "head-1",
		Reasons:         []Reason{{Code: ReasonPullRequestNotOpen}, {Code: ReasonRoutingProjectionFailed}},
		Evidence:        Evidence{PullRequestState: "CLOSED", FinalObservedHeadSHA: "head-1"},
	}
	seedGateReport(t, fixture, report)

	if err := runner.RevokeProjectRoutes(context.Background(), "project_1"); err != nil {
		t.Fatalf("RevokeProjectRoutes() error = %v", err)
	}
	if len(fixture.github.labelAdds) != 0 {
		t.Fatalf("label adds = %#v, want terminal retry removal-only cleanup", fixture.github.labelAdds)
	}
	if len(fixture.github.labelRemoves) != 2 {
		t.Fatalf("label removals = %#v, want both terminal routing labels cleared", fixture.github.labelRemoves)
	}
}
