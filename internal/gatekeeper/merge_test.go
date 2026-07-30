package gatekeeper

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/labels"
	"github.com/nexu-io/looper/internal/storage"
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
		outcomes = append(outcomes, outcome)
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
	outcomes := mergeOutcomes(t, fixture.repos)
	if len(outcomes) != 1 || !outcomes[0].Merged {
		t.Fatalf("outcomes = %+v", outcomes)
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
	if fixture.github.merges[0].Strategy != config.ReviewerAutoMergeStrategySquash {
		t.Fatalf("strategy = %q, want squash", fixture.github.merges[0].Strategy)
	}
}

var _ = strings.TrimSpace
var _ = githubinfra.EnableAutoMergeInput{}
