package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/storage"
)

func autoRunner(t *testing.T, fixture *gatekeeperFixture) *Runner {
	t.Helper()
	return New(Options{
		Repos:  fixture.repos,
		GitHub: fixture.github,
		Now:    func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool {
			return fixture.policyPermits
		},
		TrustForProject: func(string) config.GatekeeperTrustLevel {
			return config.GatekeeperTrustAuto
		},
	})
}

func mergeOutcomes(t *testing.T, repos *storage.Repositories) []MergeOutcome {
	t.Helper()
	events, err := repos.Events.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	outcomes := []MergeOutcome{}
	for _, event := range events {
		if event.EventType != MergeOutcomeEventType {
			continue
		}
		var outcome MergeOutcome
		if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
			t.Fatalf("decode merge outcome: %v", err)
		}
		if !outcome.Pending {
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes
}

func pendingMergeOutcomes(t *testing.T, repos *storage.Repositories) []MergeOutcome {
	t.Helper()
	events, err := repos.Events.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	outcomes := []MergeOutcome{}
	for _, event := range events {
		if event.EventType != MergeOutcomeEventType {
			continue
		}
		var outcome MergeOutcome
		if err := json.Unmarshal([]byte(event.PayloadJSON), &outcome); err != nil {
			t.Fatalf("decode merge outcome: %v", err)
		}
		if outcome.Pending {
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes
}

func TestAutoMergesAnEligiblePullRequest(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if !report.Eligible {
		t.Fatalf("report = %+v, want eligible", report)
	}
	if len(fixture.github.merges) != 1 {
		t.Fatalf("merges = %v, want one", fixture.github.merges)
	}
	// The forge is asked to refuse if the head moved, so the decision cannot be
	// applied to a commit it was not made about.
	if fixture.github.merges[0].HeadSHA != "head-1" {
		t.Fatalf("merge head = %q", fixture.github.merges[0].HeadSHA)
	}
	if fixture.github.merges[0].BaseBranch != "main" {
		t.Fatalf("merge base branch = %q, want main", fixture.github.merges[0].BaseBranch)
	}
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || !outcomes[0].Merged {
		t.Fatalf("outcomes = %+v", outcomes)
	}
}

func TestAutoPublishesConfirmedStatusBeforeMergeWhenDiscoveryDefers(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	_, err := fixture.autoRunner().EvaluatePullRequest(withDeferCommitStatus(context.Background()), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if len(fixture.github.callOrder) < 2 || fixture.github.callOrder[len(fixture.github.callOrder)-2] != "status" || fixture.github.callOrder[len(fixture.github.callOrder)-1] != "merge" {
		t.Fatalf("call order = %v, want confirming status immediately before merge", fixture.github.callOrder)
	}
}

// A Gate report is a statement about the moment it was made. Holds, reviews,
// threads, and policy can all change without moving the head, so acting on one
// requires making it true again first.
func TestAutoRefusesWhenTheConfirmingEvaluationBlocks(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)

	// The first evaluation passes; a hold appears before the confirming one.
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views > 1 {
			github.detail.Labels = []string{labels.HoldGlobal}
		}
	}

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	_ = report

	if len(fixture.github.merges) != 0 {
		t.Fatalf("merged despite a hold appearing between evaluations: %v", fixture.github.merges)
	}
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || outcomes[0].Merged {
		t.Fatalf("outcomes = %+v, want a recorded refusal", outcomes)
	}
	if outcomes[0].Reason != refusalNoLongerClean {
		t.Fatalf("reason = %q, want %q", outcomes[0].Reason, refusalNoLongerClean)
	}
	if len(outcomes[0].ConfirmingReasons) == 0 {
		t.Fatal("the refusal records no reason from the confirming pass")
	}
}

func TestAutoRetriesWhenConfirmingBlockedStatusPublicationFails(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.statusErr = errors.New("status unavailable")
	fixture.github.statusErrState = "failure"
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views > 1 {
			github.detail.Labels = []string{labels.HoldGlobal}
		}
	}
	_, err := autoRunner(t, fixture).EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err == nil || !strings.Contains(err.Error(), "publish confirming blocked status") {
		t.Fatalf("EvaluatePullRequest() error = %v, want retryable status publication failure", err)
	}
	if len(fixture.github.merges) != 0 {
		t.Fatalf("merges = %v, want no merge after failed blocked-status publication", fixture.github.merges)
	}
}

func TestAutoRefusesWhenTheConfirmingBaseMovesAndPublishesBlockedStatus(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)
	views := 0
	fixture.github.beforeView = func(github *fakeGatekeeperGitHub) {
		views++
		if views > 1 {
			github.detail.BaseRefName = "release"
		}
	}

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible {
		t.Fatalf("report = %+v, want confirming base-move refusal", report)
	}
	if len(fixture.github.merges) != 0 {
		t.Fatalf("merged despite confirming base move: %v", fixture.github.merges)
	}
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || outcomes[0].Merged || outcomes[0].Reason != refusalBaseMoved {
		t.Fatalf("outcomes = %+v, want base-move refusal", outcomes)
	}
	if len(fixture.github.statusCalls) == 0 || fixture.github.statusCalls[len(fixture.github.statusCalls)-1].State != "error" {
		t.Fatalf("status calls = %+v, want blocked status published before return", fixture.github.statusCalls)
	}
}

// The forge refusing is a legitimate answer — branch protection, a race with
// another merge — not a lane failure.
func TestAutoRecordsAForgeRefusalWithoutFailing(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.mergeErr = errors.New("Pull request is not mergeable")
	runner := autoRunner(t, fixture)

	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v, want a recorded refusal", err)
	}

	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || outcomes[0].Merged || outcomes[0].Reason != refusalMergeFailed {
		t.Fatalf("outcomes = %+v", outcomes)
	}
}

func TestAutoPropagatesTransientMergeFailureWithoutPersistingRefusal(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.mergeErr = &githubinfra.TransientError{Err: errors.New("GitHub API temporarily unavailable")}
	runner := autoRunner(t, fixture)
	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err == nil || !githubinfra.IsTransientError(err) {
		t.Fatalf("EvaluatePullRequest() error = %v, want transient error", err)
	}
	if outcomes := mergeOutcomes(t, fixture.repos); len(outcomes) != 0 {
		t.Fatalf("outcomes = %+v, want no durable refusal for transient failure", outcomes)
	}
	if outcomes := pendingMergeOutcomes(t, fixture.repos); len(outcomes) != 1 || outcomes[0].Reason != MergeOutcomePendingReason {
		t.Fatalf("pending outcomes = %+v, want one retryable attempt", outcomes)
	}
}

// A successful forge mutation can outlive the process that was supposed to
// append its final outcome.  Discovery must settle the durable pending marker
// from the forge's merged state even though the PR is no longer open.
func TestReconcilePendingMergeOutcomeAfterStaleStateFailure(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)
	pending := MergeOutcome{
		Version: 1, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		HeadSHA: "head-1", Pending: true, Reason: MergeOutcomePendingReason,
		AttemptedAt: fixture.now.Format(time.RFC3339Nano),
	}
	if err := runner.persistMergeOutcome(context.Background(), pending); err != nil {
		t.Fatalf("persist pending outcome: %v", err)
	}
	fixture.github.mergeable.State = "MERGED"
	fixture.github.mergeable.MergedAt = fixture.now.Format(time.RFC3339Nano)
	if err := runner.reconcilePendingMergeOutcomes(context.Background(), "project_1", "acme/looper", ""); err != nil {
		t.Fatalf("reconcilePendingMergeOutcomes() error = %v", err)
	}
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || !outcomes[0].Merged || outcomes[0].HeadSHA != "head-1" {
		t.Fatalf("settled outcomes = %+v, want one successful outcome", outcomes)
	}
	events, err := fixture.repos.Events.ListByEntityAndEventTypes(context.Background(), "pull_request", "acme/looper#42", []string{MergeOutcomeEventType})
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("merge lifecycle events = %d, want pending + settled", len(events))
	}
	// pending and settled share the same fixture.now timestamp, so row order
	// falls back to the random event id and is not guaranteed to put the
	// settled outcome last. Find it by content, not position.
	var latest MergeOutcome
	found := false
	for _, event := range events {
		var candidate MergeOutcome
		if err := json.Unmarshal([]byte(event.PayloadJSON), &candidate); err != nil {
			continue
		}
		if candidate.Pending {
			continue
		}
		latest = candidate
		found = true
		break
	}
	if !found || latest.Pending || !latest.Merged {
		t.Fatalf("latest merge outcome = %+v, want settled success", latest)
	}
}

func TestReconcilePendingMergeOutcomeRejectsDifferentMergedHead(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)
	pending := MergeOutcome{
		Version: 1, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		HeadSHA: "head-1", Pending: true, Reason: MergeOutcomePendingReason,
		AttemptedAt: fixture.now.Format(time.RFC3339Nano),
	}
	if err := runner.persistMergeOutcome(context.Background(), pending); err != nil {
		t.Fatalf("persist pending outcome: %v", err)
	}
	fixture.github.mergeable.State = "MERGED"
	fixture.github.mergeable.MergedAt = fixture.now.Format(time.RFC3339Nano)
	fixture.github.mergeable.HeadSHA = "head-2"
	if err := runner.reconcilePendingMergeOutcomes(context.Background(), "project_1", "acme/looper", ""); err != nil {
		t.Fatalf("reconcilePendingMergeOutcomes() error = %v", err)
	}
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || outcomes[0].Merged || outcomes[0].Reason != refusalHeadMoved {
		t.Fatalf("settled outcomes = %+v, want an attributed head-mismatch refusal", outcomes)
	}
}

func TestReconcilePendingMergeOutcomeUsesDurableRepositoryBinding(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)
	pending := MergeOutcome{
		Version: 1, ProjectID: "project_1", Repo: "acme/renamed-looper", PRNumber: 42,
		HeadSHA: "head-1", Pending: true, Reason: MergeOutcomePendingReason,
		AttemptedAt: fixture.now.Format(time.RFC3339Nano),
	}
	if err := runner.persistMergeOutcome(context.Background(), pending); err != nil {
		t.Fatalf("persist pending outcome: %v", err)
	}
	fixture.github.mergeable.State = "MERGED"
	fixture.github.mergeable.MergedAt = fixture.now.Format(time.RFC3339Nano)
	if err := runner.reconcilePendingMergeOutcomes(context.Background(), "project_1", "acme/looper", ""); err != nil {
		t.Fatalf("reconcilePendingMergeOutcomes() error = %v", err)
	}
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || !outcomes[0].Merged || outcomes[0].Repo != "acme/renamed-looper" {
		t.Fatalf("settled outcomes = %+v, want durable repository binding to settle", outcomes)
	}
}

// A blocked pull request must not reach the merge path at all.
func TestAutoDoesNotMergeABlockedPullRequest(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	fixture.github.detail.Labels = []string{labels.HoldGlobal}
	runner := autoRunner(t, fixture)

	report, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	})
	if err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if report.Eligible {
		t.Fatal("a held pull request evaluated as eligible")
	}
	if len(fixture.github.merges) != 0 || len(mergeOutcomes(t, fixture.repos)) != 0 {
		t.Fatal("a blocked pull request reached the merge path")
	}
}

// observe and advise hold no merge authority; that is the whole point of the
// ladder being a ladder.
func TestLowerTrustLevelsNeverMerge(t *testing.T) {
	t.Parallel()
	for _, trust := range []config.GatekeeperTrustLevel{config.GatekeeperTrustObserve, config.GatekeeperTrustAdvise} {
		fixture := newGatekeeperFixture(t)
		runner := New(Options{
			Repos: fixture.repos, GitHub: fixture.github,
			Now:                 func() time.Time { return fixture.now },
			PolicyPermitsTarget: func(string, string, string) bool { return true },
			TrustForProject:     func(string) config.GatekeeperTrustLevel { return trust },
		})

		if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
			ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
		}); err != nil {
			t.Fatalf("%s: EvaluatePullRequest() error = %v", trust, err)
		}
		if len(fixture.github.merges) != 0 {
			t.Fatalf("%s merged a pull request", trust)
		}
	}
}

// The confirming pass describes the same head as the verdict already published,
// so republishing it would rewrite the comment for no new information.
func TestConfirmingPassPublishesNoVerdict(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)

	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}

	// auto publishes like advise, so exactly one verdict comment for two passes.
	if len(fixture.github.createdBodies) > 1 {
		t.Fatalf("the confirming pass published a second verdict: %v", fixture.github.createdBodies)
	}
	if len(fixture.github.updatedBodies) != 0 {
		t.Fatalf("the confirming pass rewrote the verdict: %v", fixture.github.updatedBodies)
	}
}

func TestMergeStrategyDefaultsToSquash(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := autoRunner(t, fixture)

	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err != nil {
		t.Fatalf("EvaluatePullRequest() error = %v", err)
	}
	if len(fixture.github.merges) != 1 {
		t.Fatalf("merges = %v", fixture.github.merges)
	}
	if fixture.github.merges[0].Strategy != config.MergeStrategySquash {
		t.Fatalf("strategy = %q, want squash", fixture.github.merges[0].Strategy)
	}
}

func TestAutoUsesGatekeeperMergeStrategy(t *testing.T) {
	t.Parallel()
	fixture := newGatekeeperFixture(t)
	runner := New(Options{
		Repos: fixture.repos, GitHub: fixture.github, Now: func() time.Time { return fixture.now },
		PolicyPermitsTarget: func(string, string, string) bool { return true },
		TrustForProject:     func(string) config.GatekeeperTrustLevel { return config.GatekeeperTrustAuto },
		MergeStrategyForProject: func(string) config.MergeStrategy {
			return config.MergeStrategyRebase
		},
	})

	if _, err := runner.EvaluatePullRequest(context.Background(), EvaluationInput{
		ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, ExpectedHeadSHA: "head-1",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.github.merges) != 1 || fixture.github.merges[0].Strategy != config.MergeStrategyRebase || fixture.github.merges[0].BaseBranch != "main" {
		t.Fatalf("merges = %+v, want one rebase merge", fixture.github.merges)
	}
}
