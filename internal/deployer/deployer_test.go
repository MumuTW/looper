package deployer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recorder struct {
	head         HeadState
	headErr      error
	previousSHA  string
	previousErr  error
	createErr    error
	created      []string
	ran          int
	exitCode     int
	output       string
	runErr       error
	statuses     []bool
	statusErr    error
	descriptions []string
	notified     []Outcome
}

func (r *recorder) deps() Deps {
	return Deps{
		Head: func(context.Context) (HeadState, error) {
			return r.head, r.headErr
		},
		PreviousSHA: func(context.Context) (string, error) {
			return r.previousSHA, r.previousErr
		},
		CreateDeployment: func(_ context.Context, sha string) (int64, error) {
			if r.createErr != nil {
				return 0, r.createErr
			}
			r.created = append(r.created, sha)
			return int64(100 + len(r.created)), nil
		},
		RunCommand: func(context.Context) (int, string, error) {
			r.ran++
			return r.exitCode, r.output, r.runErr
		},
		SetStatus: func(_ context.Context, _ int64, succeeded bool, description string) error {
			r.statuses = append(r.statuses, succeeded)
			r.descriptions = append(r.descriptions, description)
			return r.statusErr
		},
		Notify: func(_ context.Context, outcome Outcome) {
			r.notified = append(r.notified, outcome)
		},
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		enabled bool
		command string
		head    HeadState
		want    Decision
	}{
		{name: "new commit", enabled: true, command: "make deploy", head: HeadState{SHA: "abc"}, want: DecisionDeploy},
		{name: "already deployed", enabled: true, command: "make deploy", head: HeadState{SHA: "abc", Deployed: true, DeployedState: "success"}, want: DecisionUpToDate},
		// A failed deploy tends to keep failing. Retrying every tick would turn one
		// broken deploy into an unbounded stream of them.
		{name: "previous attempt failed", enabled: true, command: "make deploy", head: HeadState{SHA: "abc", Deployed: true, DeployedState: "failure"}, want: DecisionUpToDate},
		{name: "still running", enabled: true, command: "make deploy", head: HeadState{SHA: "abc", Deployed: true, DeployedState: "in_progress"}, want: DecisionRetryLater},
		{name: "disabled", enabled: false, command: "make deploy", head: HeadState{SHA: "abc"}, want: DecisionSkip},
		{name: "no command", enabled: true, command: "  ", head: HeadState{SHA: "abc"}, want: DecisionSkip},
		{name: "no head", enabled: true, command: "make deploy", head: HeadState{}, want: DecisionSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Decide(tc.enabled, tc.command, tc.head); got != tc.want {
				t.Fatalf("Decide() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunDeploysANewCommit(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc123def456"}, previousSHA: "old999"}

	decision, outcome, err := Run(context.Background(), true, "make deploy", rec.deps())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if decision != DecisionDeploy || outcome == nil || !outcome.Succeeded {
		t.Fatalf("decision = %q, outcome = %+v", decision, outcome)
	}
	if len(rec.created) != 1 || rec.created[0] != "abc123def456" {
		t.Fatalf("created = %v", rec.created)
	}
	if len(rec.statuses) != 1 || !rec.statuses[0] {
		t.Fatalf("statuses = %v", rec.statuses)
	}
	if outcome.PreviousSHA != "old999" {
		t.Fatalf("PreviousSHA = %q", outcome.PreviousSHA)
	}
	if len(rec.notified) != 1 {
		t.Fatalf("notified = %d, want 1", len(rec.notified))
	}
}

// The deployment must exist before the command runs, or a crash mid-deploy
// leaves a side effect nothing recorded.
func TestRunRecordsTheDeploymentBeforeRunningTheCommand(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, createErr: errors.New("403 forbidden")}

	_, outcome, err := Run(context.Background(), true, "make deploy", rec.deps())
	if err == nil {
		t.Fatal("Run() succeeded despite failing to record the deployment")
	}
	if rec.ran != 0 {
		t.Fatal("the deploy command ran without a recorded deployment")
	}
	if outcome != nil {
		t.Fatalf("outcome = %+v, want none", outcome)
	}
}

func TestRunReportsAFailedCommand(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, exitCode: 2, output: "connection refused"}

	_, outcome, err := Run(context.Background(), true, "make deploy", rec.deps())
	if err != nil {
		t.Fatalf("Run() error = %v — a failed deploy is a reported outcome, not a lane error", err)
	}
	if outcome.Succeeded || outcome.ExitCode != 2 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if len(rec.statuses) != 1 || rec.statuses[0] {
		t.Fatalf("statuses = %v, want one failure", rec.statuses)
	}
	if !strings.Contains(rec.descriptions[0], "exit 2") {
		t.Fatalf("description = %q", rec.descriptions[0])
	}
	if len(rec.notified) != 1 || rec.notified[0].Succeeded {
		t.Fatalf("notified = %+v", rec.notified)
	}
}

// The deploy already happened; failing to record its status must not look like
// the deploy failed.
func TestRunStillReportsTheOutcomeWhenTheStatusWriteFails(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, statusErr: errors.New("502")}

	_, outcome, err := Run(context.Background(), true, "make deploy", rec.deps())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome == nil || !outcome.Succeeded {
		t.Fatalf("outcome = %+v, want the successful deploy reported", outcome)
	}
	if len(rec.notified) != 1 {
		t.Fatal("the human was not told about a deploy that did happen")
	}
}

// The comparison link is a convenience. Losing it must not stop a deploy.
func TestRunDeploysEvenWhenThePreviousCommitCannotBeResolved(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc"}, previousErr: errors.New("rate limited")}

	_, outcome, err := Run(context.Background(), true, "make deploy", rec.deps())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome == nil || !outcome.Succeeded {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.PreviousSHA != "" {
		t.Fatalf("PreviousSHA = %q, want empty", outcome.PreviousSHA)
	}
}

func TestRunDoesNothingWhenTheHeadIsAlreadyDeployed(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc", Deployed: true, DeployedState: "success"}}

	decision, outcome, err := Run(context.Background(), true, "make deploy", rec.deps())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if decision != DecisionUpToDate || outcome != nil {
		t.Fatalf("decision = %q, outcome = %+v", decision, outcome)
	}
	if rec.ran != 0 || len(rec.created) != 0 {
		t.Fatal("an already-deployed commit was deployed again")
	}
}

// Two deploys of different commits running at once is the failure this guards
// against; a stalled lane is visible and recoverable, that is not.
func TestRunDeclinesWhileADeployIsInProgress(t *testing.T) {
	t.Parallel()
	rec := &recorder{head: HeadState{SHA: "abc", Deployed: true, DeployedState: "in_progress"}}

	decision, _, err := Run(context.Background(), true, "make deploy", rec.deps())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if decision != DecisionRetryLater || rec.ran != 0 {
		t.Fatalf("decision = %q, ran = %d", decision, rec.ran)
	}
}

func TestRunPropagatesHeadLookupFailures(t *testing.T) {
	t.Parallel()
	rec := &recorder{headErr: errors.New("gh: rate limited")}

	if _, _, err := Run(context.Background(), true, "make deploy", rec.deps()); err == nil {
		t.Fatal("Run() swallowed a head lookup failure")
	}
	if rec.ran != 0 {
		t.Fatal("the deploy command ran without knowing the head")
	}
}

func TestCompareURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, repo, previous, sha, want string
	}{
		{name: "normal", repo: "acme/looper", previous: "aaa", sha: "bbb", want: "https://github.com/acme/looper/compare/aaa...bbb"},
		{name: "no previous deploy", repo: "acme/looper", sha: "bbb"},
		{name: "same commit", repo: "acme/looper", previous: "aaa", sha: "aaa"},
		{name: "no repo", previous: "aaa", sha: "bbb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CompareURL(tc.repo, tc.previous, tc.sha); got != tc.want {
				t.Fatalf("CompareURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
