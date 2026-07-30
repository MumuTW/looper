package deployer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)

type recorder struct {
	head           HeadState
	headErr        error
	previousSHA    string
	previousErr    error
	created        []string
	createErr      error
	states         []DeploymentState
	descriptions   []string
	statusErr      error
	inProgressErr  error
	materialized   []string
	materializeAt  int
	materializeErr error
	released       int
	ranIn          []string
	exitCode       int
	runErr         error
	notified       []Outcome
}

func (r *recorder) deps() Deps {
	return Deps{
		Head:        func(context.Context) (HeadState, error) { return r.head, r.headErr },
		PreviousSHA: func(context.Context) (string, error) { return r.previousSHA, r.previousErr },
		CreateDeployment: func(_ context.Context, sha string) (int64, error) {
			if r.createErr != nil {
				return 0, r.createErr
			}
			r.created = append(r.created, sha)
			return int64(100 + len(r.created)), nil
		},
		SetStatus: func(_ context.Context, _ int64, state DeploymentState, description string) error {
			if state == StateInProgress {
				if r.inProgressErr != nil {
					return r.inProgressErr
				}
				r.states = append(r.states, state)
				r.descriptions = append(r.descriptions, description)
				return nil
			}
			r.states = append(r.states, state)
			r.descriptions = append(r.descriptions, description)
			// statusErr models the terminal write failing after the deploy has
			// already happened.
			return r.statusErr
		},
		Materialize: func(_ context.Context, sha string) (string, func(), error) {
			if r.materializeErr != nil {
				return "", func() {}, r.materializeErr
			}
			r.materialized = append(r.materialized, sha)
			return "/tmp/deploy-" + sha, func() { r.released++ }, nil
		},
		RunCommand: func(_ context.Context, dir string) (int, string, error) {
			r.ranIn = append(r.ranIn, dir)
			return r.exitCode, "/logs/deploy.log", r.runErr
		},
		Notify: func(_ context.Context, outcome Outcome) { r.notified = append(r.notified, outcome) },
	}
}

func run(rec *recorder) (Decision, *Outcome, error) {
	return Run(context.Background(), true, "make deploy", 60*time.Second, now, rec.deps())
}

func TestDecide(t *testing.T) {
	t.Parallel()
	timeout := 60 * time.Second
	cases := []struct {
		name string
		head HeadState
		want Decision
	}{
		{name: "new commit", head: HeadState{SHA: "abc"}, want: DecisionDeploy},
		{name: "already succeeded", head: HeadState{SHA: "abc", Deployed: true, State: StateSuccess}, want: DecisionUpToDate},
		// A failed deploy tends to keep failing; retrying every tick would turn one
		// broken deploy into a stream of them.
		{name: "previously failed", head: HeadState{SHA: "abc", Deployed: true, State: StateFailure}, want: DecisionUpToDate},
		{name: "running now", head: HeadState{SHA: "abc", Deployed: true, State: StateInProgress, StartedAt: now.Add(-time.Minute)}, want: DecisionInProgress},
		// A deployment with no status at all is what an interrupted deploy leaves.
		{name: "claimed but statusless", head: HeadState{SHA: "abc", Deployed: true, StartedAt: now.Add(-time.Minute)}, want: DecisionInProgress},
		{name: "abandoned past the window", head: HeadState{SHA: "abc", Deployed: true, State: StateInProgress, StartedAt: now.Add(-10 * time.Minute)}, want: DecisionDeploy},
		// Without a start time there is nothing to bound, so the commit stays claimed.
		{name: "unfinished with no start time", head: HeadState{SHA: "abc", Deployed: true, State: StateInProgress}, want: DecisionInProgress},
		{name: "unknown state", head: HeadState{SHA: "abc", Deployed: true, State: "queued"}, want: DecisionUpToDate},
		{name: "no head", head: HeadState{}, want: DecisionSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Decide(true, "make deploy", tc.head, timeout, now); got != tc.want {
				t.Fatalf("Decide() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecideSkipsWhenNotConfigured(t *testing.T) {
	t.Parallel()
	head := HeadState{SHA: "abc"}

	if got := Decide(false, "make deploy", head, time.Minute, now); got != DecisionSkip {
		t.Fatalf("disabled = %q", got)
	}
	if got := Decide(true, "   ", head, time.Minute, now); got != DecisionSkip {
		t.Fatalf("no command = %q", got)
	}
}

// The deploy must run against the commit it reports, not against whatever the
// project checkout happens to contain.
func TestRunExecutesInTheMaterializedCommit(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc123"}}

	_, outcome, err := run(rec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(rec.materialized) != 1 || rec.materialized[0] != "abc123" {
		t.Fatalf("materialized = %v", rec.materialized)
	}
	if len(rec.ranIn) != 1 || rec.ranIn[0] != "/tmp/deploy-abc123" {
		t.Fatalf("ran in %v, want the materialized checkout", rec.ranIn)
	}
	if rec.released != 1 {
		t.Fatalf("released = %d, want the checkout released once", rec.released)
	}
	if !outcome.Succeeded {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// A commit that cannot be checked out never ran, so it must not be recorded as a
// failed deploy: that would mark the commit permanently done.
func TestRunRecordsNothingWhenMaterializationFails(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, materializeErr: errors.New("unknown revision")}

	_, outcome, err := run(rec)
	if err == nil {
		t.Fatal("Run() succeeded despite failing to materialize")
	}
	if len(rec.created) != 0 || len(rec.states) != 0 {
		t.Fatalf("recorded a deployment for a commit that never ran: created=%v states=%v", rec.created, rec.states)
	}
	if outcome != nil {
		t.Fatalf("outcome = %+v, want none", outcome)
	}
}

// Without an in_progress claim, an interrupted deploy is indistinguishable from
// one that never started.
func TestRunClaimsTheDeploymentBeforeRunning(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}}

	if _, _, err := run(rec); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(rec.states) != 2 || rec.states[0] != StateInProgress {
		t.Fatalf("states = %v, want in_progress then a terminal state", rec.states)
	}
	if rec.states[1] != StateSuccess {
		t.Fatalf("final state = %q", rec.states[1])
	}
}

func TestRunDoesNotRunWhenTheClaimFails(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, inProgressErr: errors.New("403")}

	if _, _, err := run(rec); err == nil {
		t.Fatal("Run() proceeded without claiming the deployment")
	}
	if len(rec.ranIn) != 0 {
		t.Fatalf("the command ran unclaimed: %v", rec.ranIn)
	}
}

func TestRunReportsAFailedCommand(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, exitCode: 2}

	_, outcome, err := run(rec)
	if err != nil {
		t.Fatalf("Run() error = %v — a failed deploy is an outcome, not a lane error", err)
	}
	if outcome.Succeeded || outcome.ExitCode != 2 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if rec.states[len(rec.states)-1] != StateFailure {
		t.Fatalf("states = %v", rec.states)
	}
}

// A deploy command's output routinely contains tokens and signed URLs, and the
// status description is published on the commit.
func TestStatusDescriptionsCarryNoCommandOutput(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, exitCode: 1, runErr: errors.New("AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI leaked into stderr")}

	if _, _, err := run(rec); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for _, description := range rec.descriptions {
		if strings.Contains(description, "wJalrXUtnFEMI") || strings.Contains(description, "AWS_SECRET") {
			t.Fatalf("status description leaked command output: %q", description)
		}
	}
}

// The same reasoning applies to the notification: it carries a path to the log,
// never the log.
func TestOutcomeCarriesALogPathRatherThanOutput(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}}

	_, outcome, err := run(rec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.LogPath != "/logs/deploy.log" {
		t.Fatalf("LogPath = %q", outcome.LogPath)
	}
	if len(rec.notified) != 1 || rec.notified[0].LogPath != "/logs/deploy.log" {
		t.Fatalf("notified = %+v", rec.notified)
	}
}

// The deploy already happened; failing to record its status must not look like
// the deploy failed.
func TestRunStillReportsWhenTheFinalStatusWriteFails(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, statusErr: errors.New("502")}

	_, outcome, err := run(rec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome == nil || !outcome.Succeeded || len(rec.notified) != 1 {
		t.Fatalf("outcome = %+v notified = %d", outcome, len(rec.notified))
	}
}

func TestRunDeploysEvenWhenThePreviousCommitCannotBeResolved(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, previousErr: errors.New("rate limited")}

	_, outcome, err := run(rec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome == nil || outcome.PreviousSHA != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestRunPropagatesHeadLookupFailures(t *testing.T) {
	t.Parallel()
	rec := &recorder{headErr: errors.New("gh: rate limited")}

	if _, _, err := run(rec); err == nil {
		t.Fatal("Run() swallowed a head lookup failure")
	}
	if len(rec.materialized) != 0 {
		t.Fatal("materialized a commit without knowing the head")
	}
}

func TestRunDoesNothingWhenTheHeadIsAlreadyDeployed(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc", Deployed: true, State: StateSuccess}}

	decision, outcome, err := run(rec)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if decision != DecisionUpToDate || outcome != nil {
		t.Fatalf("decision = %q outcome = %+v", decision, outcome)
	}
	if len(rec.materialized) != 0 || len(rec.ranIn) != 0 {
		t.Fatal("an already-deployed commit was deployed again")
	}
}

func TestCompareURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, host, repo, previous, sha, want string
	}{
		{name: "github", repo: "acme/looper", previous: "aaa", sha: "bbb", want: "https://github.com/acme/looper/compare/aaa...bbb"},
		// An enterprise install serves the same paths from its own domain.
		{name: "enterprise host", host: "git.acme.internal", repo: "acme/looper", previous: "aaa", sha: "bbb", want: "https://git.acme.internal/acme/looper/compare/aaa...bbb"},
		{name: "no previous deploy", repo: "acme/looper", sha: "bbb"},
		{name: "same commit", repo: "acme/looper", previous: "aaa", sha: "aaa"},
		{name: "no repo", previous: "aaa", sha: "bbb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CompareURL(tc.host, tc.repo, tc.previous, tc.sha); got != tc.want {
				t.Fatalf("CompareURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
