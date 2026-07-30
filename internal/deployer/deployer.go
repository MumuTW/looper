// Package deployer runs a project's deploy command against the exact commit it
// reports as deployed.
//
// It is an agent-free Role in the same sense as the Merge Gatekeeper: it decides
// from durable forge state, performs one bounded side effect, and reports it. It
// does not judge success beyond the command's exit status and never rolls a
// deploy back — deciding a deploy went bad after the fact is a separate concern
// (#118).
package deployer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCommandNotStarted reports that the deploy command never began — the shell
// could not be spawned, not that the deploy ran and failed.
var ErrCommandNotStarted = errors.New("deploy command did not start")

// DefaultTimeoutSeconds bounds one deploy when the project sets none.
const DefaultTimeoutSeconds = 900

// Decision is what the lane concluded about a project's current head.
type Decision string

const (
	DecisionDeploy     Decision = "deploy"
	DecisionUpToDate   Decision = "up_to_date"
	DecisionSkip       Decision = "skip"
	DecisionInProgress Decision = "in_progress"
)

// DeploymentState mirrors the forge's deployment status vocabulary.
type DeploymentState string

const (
	StateInProgress DeploymentState = "in_progress"
	StateSuccess    DeploymentState = "success"
	StateFailure    DeploymentState = "failure"
	// StateInactive is what the forge marks a deployment when a later one
	// supersedes it. It is the difference between "this commit was deployed once"
	// and "this commit is what is deployed now".
	StateInactive DeploymentState = "inactive"
)

// HeadState is the forge state the decision is made from.
type HeadState struct {
	SHA string
	// Deployed reports whether this exact commit already has a Looper deployment.
	Deployed bool
	// State is that deployment's latest status. Empty means a deployment record
	// exists with no status at all, which is how an interrupted deploy looks.
	State DeploymentState
	// StartedAt is when the deployment was created, used to bound how long an
	// unfinished one holds the commit.
	StartedAt time.Time
}

// Decide reports what to do with the current head.
//
// A commit whose deploy failed is not retried automatically: a deploy that fails
// tends to keep failing, and retrying every tick turns one broken deploy into an
// unbounded stream. Re-running is a human's call.
//
// An unfinished deploy holds the commit only for as long as one could still be
// running. Past that, the daemon that started it is gone — killed, crashed,
// restarted — and refusing forever would strand the commit undeployed with
// nothing to indicate why. This is the one case where Looper acts on a record it
// did not finish writing, so it is bounded explicitly rather than by feel.
func Decide(enabled bool, command string, head HeadState, timeout time.Duration, now time.Time) Decision {
	if !enabled || strings.TrimSpace(command) == "" || strings.TrimSpace(head.SHA) == "" {
		return DecisionSkip
	}
	if !head.Deployed {
		return DecisionDeploy
	}
	switch head.State {
	case StateInactive:
		// The commit was deployed and then superseded. Reaching the head again means
		// the branch moved back — a revert or a reset — and what is running is still
		// the commit that replaced it. Deploying is the whole point of a revert.
		return DecisionDeploy
	case StateSuccess, StateFailure:
		return DecisionUpToDate
	case StateInProgress, "":
		if head.StartedAt.IsZero() {
			return DecisionInProgress
		}
		if now.Sub(head.StartedAt) > abandonedAfter(timeout) {
			return DecisionDeploy
		}
		return DecisionInProgress
	default:
		// An unfamiliar state came from something other than this lane. Treat the
		// commit as spoken for rather than deploying over it.
		return DecisionUpToDate
	}
}

// abandonedAfter is how long an unfinished deploy is believed to still be
// running. Twice the timeout leaves room for the command to be killed at its
// deadline and the status write to follow.
func abandonedAfter(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = DefaultTimeoutSeconds * time.Second
	}
	return 2 * timeout
}

// Outcome is the result of one deploy.
type Outcome struct {
	SHA          string
	PreviousSHA  string
	ExitCode     int
	Succeeded    bool
	Duration     time.Duration
	DeploymentID int64
	// LogPath is where the command's output was written. The output itself is
	// deliberately not carried here: a deploy command's stdout routinely contains
	// tokens, signed URLs, and connection strings, and this value reaches
	// notifications.
	LogPath string
}

// Deps are the effects a deploy needs, injected so the sequencing is testable
// without a forge, a git repository, or a shell.
type Deps struct {
	Head        func(ctx context.Context) (HeadState, error)
	PreviousSHA func(ctx context.Context) (string, error)
	// CreateDeployment records the intent and returns the deployment id.
	CreateDeployment func(ctx context.Context, sha string) (int64, error)
	// SetStatus records a state against the deployment.
	SetStatus func(ctx context.Context, deploymentID int64, state DeploymentState, description string) error
	// Materialize checks the exact commit out and returns the directory to run in
	// plus a release function.
	Materialize func(ctx context.Context, sha string) (dir string, release func(), err error)
	// RunCommand executes the deploy command in dir and returns where its output
	// was captured.
	RunCommand func(ctx context.Context, dir string) (exitCode int, logPath string, err error)
	Notify     func(ctx context.Context, outcome Outcome)
	LogWarn    func(msg string, fields map[string]any)
}

// Run performs at most one deploy.
func Run(ctx context.Context, enabled bool, command string, timeout time.Duration, now time.Time, deps Deps) (Decision, *Outcome, error) {
	head, err := deps.Head(ctx)
	if err != nil {
		return "", nil, err
	}
	decision := Decide(enabled, command, head, timeout, now)
	if decision != DecisionDeploy {
		return decision, nil, nil
	}

	previousSHA := ""
	if deps.PreviousSHA != nil {
		// Only used to build a comparison link, so a failure must not stop a deploy.
		if sha, err := deps.PreviousSHA(ctx); err == nil {
			previousSHA = sha
		} else if deps.LogWarn != nil {
			deps.LogWarn("deployer: could not resolve the previously deployed commit", map[string]any{"error": err.Error()})
		}
	}

	// Materialize before recording intent. A deployment that cannot be built is
	// not a deploy that failed; recording one would leave a failure on the commit
	// for something that never ran.
	dir, release, err := deps.Materialize(ctx, head.SHA)
	if err != nil {
		return decision, nil, fmt.Errorf("materialize %s: %w", short(head.SHA), err)
	}
	defer release()

	deploymentID, err := deps.CreateDeployment(ctx, head.SHA)
	if err != nil {
		return decision, nil, fmt.Errorf("record deployment for %s: %w", short(head.SHA), err)
	}
	// Claim the commit before running. Without this the deployment carries no
	// status at all while the command runs, and an interrupted deploy is
	// indistinguishable from one that was never started.
	if err := deps.SetStatus(ctx, deploymentID, StateInProgress, "looper deploy of "+short(head.SHA)+" started"); err != nil {
		return decision, nil, fmt.Errorf("claim deployment %d: %w", deploymentID, err)
	}

	started := time.Now()
	exitCode, logPath, runErr := deps.RunCommand(ctx, dir)
	if errors.Is(runErr, ErrCommandNotStarted) {
		// Nothing ran. Recording a failure would mark the commit permanently acted
		// on for a transient local condition — the same reasoning that keeps a
		// materialization failure from being recorded as a failed deploy. The
		// in_progress claim stands and the abandonment window releases it.
		return decision, nil, fmt.Errorf("start deploy command for %s: %w", short(head.SHA), runErr)
	}
	outcome := Outcome{
		SHA: head.SHA, PreviousSHA: previousSHA, ExitCode: exitCode,
		Succeeded: runErr == nil && exitCode == 0, Duration: time.Since(started),
		DeploymentID: deploymentID, LogPath: logPath,
	}

	// The description is built from the exit status only. Command output can
	// carry credentials and this string is published on the commit.
	description := fmt.Sprintf("looper deploy of %s succeeded", short(head.SHA))
	state := StateSuccess
	if !outcome.Succeeded {
		state = StateFailure
		description = fmt.Sprintf("looper deploy of %s failed with exit code %d", short(head.SHA), exitCode)
	}
	if err := deps.SetStatus(ctx, deploymentID, state, description); err != nil && deps.LogWarn != nil {
		// The deploy happened. A missing final status leaves the commit claimed
		// until the abandonment window elapses, so this is loud rather than silent.
		deps.LogWarn("deployer: could not record the final deployment status", map[string]any{
			"sha": head.SHA, "deploymentId": deploymentID, "error": err.Error(),
		})
	}
	if deps.Notify != nil {
		deps.Notify(ctx, outcome)
	}
	return decision, &outcome, nil
}

func short(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// CompareURL links what changed since the last successful deploy. The host comes
// from the repository rather than being assumed: an enterprise install serves the
// same paths from its own domain.
func CompareURL(host, repo, previousSHA, sha string) string {
	previousSHA = strings.TrimSpace(previousSHA)
	sha = strings.TrimSpace(sha)
	repo = strings.TrimSpace(repo)
	if repo == "" || previousSHA == "" || sha == "" || previousSHA == sha {
		return ""
	}
	host = strings.TrimSpace(host)
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("https://%s/%s/compare/%s...%s", host, repo, previousSHA, sha)
}
