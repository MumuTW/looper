package gatekeeper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
)

func blockedReport() Report {
	return Report{
		Version: reportVersion, ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42,
		Status: StatusBlocked, Eligible: false, ObservedHeadSHA: "abc123def456",
		Reasons: []Reason{
			{Code: ReasonCheckFailed, Subject: "verify"},
			{Code: ReasonUnresolvedReviewThread, Subject: "thread-7"},
		},
	}
}

// A verdict exists so a human does not have to redo the judgement, so every
// blocking reason and its subject has to be in it.
func TestBuildVerdictCommentStatesEveryBlockingReason(t *testing.T) {
	t.Parallel()

	body := BuildVerdictComment(blockedReport())

	for _, want := range []string{VerdictCommentMarker, "blocked", "required check failed", "verify", "review thread is unresolved", "thread-7", "abc123d"} {
		if !strings.Contains(body, want) {
			t.Fatalf("verdict missing %q:\n%s", want, body)
		}
	}
}

// The verdict describes one head. Saying so is what keeps it from being read as
// a licence to merge later.
func TestBuildVerdictCommentStatesItsOwnExpiry(t *testing.T) {
	t.Parallel()

	body := BuildVerdictComment(blockedReport())

	if !strings.Contains(body, "invalidates it") {
		t.Fatalf("verdict does not state that later changes invalidate it:\n%s", body)
	}
}

func TestBuildVerdictCommentReportsEligible(t *testing.T) {
	t.Parallel()
	report := blockedReport()
	report.Eligible = true
	report.Status = StatusEligible
	report.Reasons = nil

	body := BuildVerdictComment(report)

	if !strings.Contains(body, "eligible") || strings.Contains(body, "Blocked by") {
		t.Fatalf("eligible verdict = \n%s", body)
	}
}

// Reason codes are stable machine identifiers; a person reading a pull request
// needs the sentence.
func TestBuildVerdictCommentExplainsCodesInProse(t *testing.T) {
	t.Parallel()

	body := BuildVerdictComment(blockedReport())

	if strings.Contains(body, "required_check_failed") {
		t.Fatalf("verdict leaked a raw reason code:\n%s", body)
	}
}

// An unknown code must still appear rather than vanish, or a future reason would
// silently produce an empty explanation.
func TestBuildVerdictCommentFallsBackToTheRawCode(t *testing.T) {
	t.Parallel()
	report := blockedReport()
	report.Reasons = []Reason{{Code: ReasonCode("something_new")}}

	if body := BuildVerdictComment(report); !strings.Contains(body, "something_new") {
		t.Fatalf("unknown reason vanished from the verdict:\n%s", body)
	}
}

// newVerdictRunner reuses the package fixture's storage so persist writes a real
// event, then swaps in the GitHub fake this test needs.
func newVerdictRunner(t *testing.T, trust config.GatekeeperTrustLevel, github *fakeGatekeeperGitHub) *Runner {
	t.Helper()
	fixture := newGatekeeperFixture(t)
	return New(Options{
		Repos:  fixture.repos,
		GitHub: github,
		Now:    func() time.Time { return fixture.now },
		TrustForProject: func(string) config.GatekeeperTrustLevel {
			return trust
		},
	})
}

// observe is the default and must stay silent: it is the level an operator has
// not yet chosen to move off.
func TestObserveLevelPublishesNothing(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{}
	runner := newVerdictRunner(t, config.GatekeeperTrustObserve, github)

	if _, err := runner.persist(context.Background(), blockedReport()); err != nil {
		t.Fatalf("persist() error = %v", err)
	}
	if len(github.createdBodies) != 0 || len(github.updatedBodies) != 0 {
		t.Fatalf("observe published a comment: created=%v updated=%v", github.createdBodies, github.updatedBodies)
	}
}

func TestAdviseLevelPublishesTheVerdict(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{}
	runner := newVerdictRunner(t, config.GatekeeperTrustAdvise, github)

	if _, err := runner.persist(context.Background(), blockedReport()); err != nil {
		t.Fatalf("persist() error = %v", err)
	}
	if len(github.createdBodies) != 1 || !strings.Contains(github.createdBodies[0], VerdictCommentMarker) {
		t.Fatalf("created = %v", github.createdBodies)
	}
}

// The lane re-evaluates every tick. Posting a new comment each time would bury
// the pull request the verdict is meant to clarify.
func TestAdviseUpdatesTheExistingVerdictInPlace(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{}
	runner := newVerdictRunner(t, config.GatekeeperTrustAdvise, github)
	ctx := context.Background()

	if _, err := runner.persist(ctx, blockedReport()); err != nil {
		t.Fatalf("first persist() error = %v", err)
	}
	changed := blockedReport()
	changed.Reasons = []Reason{{Code: ReasonCheckPending, Subject: "verify"}}
	if _, err := runner.persist(ctx, changed); err != nil {
		t.Fatalf("second persist() error = %v", err)
	}

	if len(github.createdBodies) != 1 {
		t.Fatalf("created %d comments, want the verdict updated in place", len(github.createdBodies))
	}
	if len(github.updatedBodies) != 1 || !strings.Contains(github.updatedBodies[0], "still running") {
		t.Fatalf("updated = %v", github.updatedBodies)
	}
}

// An unchanged verdict must not rewrite the owned comment or scan its discussion
// again. Routing labels are intentionally reconciled separately on every
// published evaluation so an external label edit cannot disable the queue route.
func TestUnchangedVerdictPerformsNoForgeCalls(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{}
	runner := newVerdictRunner(t, config.GatekeeperTrustAdvise, github)
	ctx := context.Background()

	if _, err := runner.persist(ctx, blockedReport()); err != nil {
		t.Fatalf("first persist() error = %v", err)
	}
	listsAfterFirst, loginsAfterFirst := github.listCalls, github.loginCalls

	if _, err := runner.persist(ctx, blockedReport()); err != nil {
		t.Fatalf("second persist() error = %v", err)
	}

	if len(github.updatedBodies) != 0 {
		t.Fatalf("an unchanged verdict was rewritten: %v", github.updatedBodies)
	}
	if github.listCalls != listsAfterFirst || github.loginCalls != loginsAfterFirst {
		t.Fatalf("an unchanged verdict still read the forge: lists %d->%d, logins %d->%d",
			listsAfterFirst, github.listCalls, loginsAfterFirst, github.loginCalls)
	}
}

// Demotion has to withdraw what the previous level published. Silence would leave
// stale advice — possibly "eligible" — visible indefinitely on a project that no
// longer offers advice at all.
func TestDemotionToObserveRetiresThePublishedVerdict(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{}
	fixture := newGatekeeperFixture(t)
	ctx := context.Background()

	trust := config.GatekeeperTrustAdvise
	runner := New(Options{
		Repos: fixture.repos, GitHub: github, Now: func() time.Time { return fixture.now },
		TrustForProject: func(string) config.GatekeeperTrustLevel { return trust },
	})

	if _, err := runner.persist(ctx, blockedReport()); err != nil {
		t.Fatalf("advise persist() error = %v", err)
	}
	trust = config.GatekeeperTrustObserve
	if _, err := runner.persist(ctx, blockedReport()); err != nil {
		t.Fatalf("observe persist() error = %v", err)
	}

	if len(github.updatedBodies) != 1 || !strings.Contains(github.updatedBodies[0], "withdrawn") {
		t.Fatalf("updated = %v, want the verdict withdrawn", github.updatedBodies)
	}
	// Retirement keeps the marker so a later promotion reuses the same comment
	// rather than creating a second one.
	if !strings.Contains(github.updatedBodies[0], VerdictCommentMarker) {
		t.Fatalf("retired body lost the ownership marker:\n%s", github.updatedBodies[0])
	}
}

// Retirement must be idempotent: an already-withdrawn verdict is not rewritten on
// every subsequent tick.
func TestRetirementHappensOnceAndThenGoesQuiet(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{}
	fixture := newGatekeeperFixture(t)
	ctx := context.Background()
	trust := config.GatekeeperTrustAdvise
	runner := New(Options{
		Repos: fixture.repos, GitHub: github, Now: func() time.Time { return fixture.now },
		TrustForProject: func(string) config.GatekeeperTrustLevel { return trust },
	})

	if _, err := runner.persist(ctx, blockedReport()); err != nil {
		t.Fatalf("advise persist() error = %v", err)
	}
	trust = config.GatekeeperTrustObserve
	for i := 0; i < 3; i++ {
		if _, err := runner.persist(ctx, blockedReport()); err != nil {
			t.Fatalf("observe persist() %d error = %v", i, err)
		}
	}

	if len(github.updatedBodies) != 1 {
		t.Fatalf("retired %d times, want once: %v", len(github.updatedBodies), github.updatedBodies)
	}
}

// A project that never published has nothing to withdraw, and must not read the
// forge looking for it.
func TestObserveWithNoPriorVerdictTouchesNothing(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{}
	runner := newVerdictRunner(t, config.GatekeeperTrustObserve, github)

	if _, err := runner.persist(context.Background(), blockedReport()); err != nil {
		t.Fatalf("persist() error = %v", err)
	}
	if github.listCalls != 0 || github.loginCalls != 0 {
		t.Fatalf("observe read the forge: lists=%d logins=%d", github.listCalls, github.loginCalls)
	}
}

// Two evaluators racing can each create a verdict. Both must converge on the same
// survivor, or the pull request keeps two contradictory verdicts forever.
func TestDuplicateOwnedVerdictsAreReconciled(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{
		currentLogin: "looper-bot",
		comments: []githubinfra.CommentInfo{
			{ID: 20, Author: "looper-bot", Body: VerdictCommentMarker + "\nstale duplicate"},
			{ID: 10, Author: "looper-bot", Body: VerdictCommentMarker + "\nolder original"},
		},
	}
	runner := newVerdictRunner(t, config.GatekeeperTrustAdvise, github)

	if _, err := runner.persist(context.Background(), blockedReport()); err != nil {
		t.Fatalf("persist() error = %v", err)
	}

	if len(github.createdBodies) != 0 {
		t.Fatalf("created a third verdict: %v", github.createdBodies)
	}
	if len(github.updatedBodies) != 1 {
		t.Fatalf("updated = %v, want the survivor rewritten once", github.updatedBodies)
	}
	// Oldest id wins so every evaluator picks the same one.
	if len(github.deletedIDs) != 1 || github.deletedIDs[0] != 20 {
		t.Fatalf("deleted = %v, want the newer duplicate removed", github.deletedIDs)
	}
}

// Reports written before the trust ladder carry the historical mode and never
// published, so they must not be mistaken for something needing withdrawal.
func TestLegacyObserveOnlyReportIsNotTreatedAsPublished(t *testing.T) {
	t.Parallel()
	legacy := blockedReport()
	legacy.Mode = "observe_only"

	if previousPublished(legacy) {
		t.Fatal("a version-1 observe_only report was read as having published")
	}
	if action := decideVerdictAction(config.GatekeeperTrustObserve, &legacy, blockedReport()); action != verdictActionNone {
		t.Fatalf("action = %q, want none", action)
	}
}

// A human quoting the marker back must not have their comment rewritten.
func TestAdviseIgnoresAMarkerInSomeoneElsesComment(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{
		currentLogin: "looper-bot",
		comments: []githubinfra.CommentInfo{
			{ID: 1, Author: "a-human", Body: "why is this blocked? " + VerdictCommentMarker},
		},
	}
	runner := newVerdictRunner(t, config.GatekeeperTrustAdvise, github)

	if _, err := runner.persist(context.Background(), blockedReport()); err != nil {
		t.Fatalf("persist() error = %v", err)
	}
	if len(github.updatedBodies) != 0 {
		t.Fatalf("rewrote a human's comment: %v", github.updatedBodies)
	}
	if len(github.createdBodies) != 1 {
		t.Fatalf("created = %v, want its own verdict posted", github.createdBodies)
	}
}

// The report is the durable record; the comment is a convenience. A forge that
// refuses the comment must not discard an evaluation already stored.
func TestPublishFailureDoesNotLoseTheReport(t *testing.T) {
	t.Parallel()
	github := &fakeGatekeeperGitHub{commentErr: errors.New("forge refused the comment")}
	runner := newVerdictRunner(t, config.GatekeeperTrustAdvise, github)

	report, err := runner.persist(context.Background(), blockedReport())
	if err != nil {
		t.Fatalf("persist() error = %v — a failed comment discarded the report", err)
	}
	if report.Status != StatusBlocked {
		t.Fatalf("report = %+v", report)
	}
}
