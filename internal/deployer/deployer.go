// Package deployer runs a project's deploy command after its base branch moves,
// and reports the outcome where a human will see it.
//
// It is an agent-free Role in the same sense as the Merge Gatekeeper: it makes a
// policy decision from durable forge state and performs one bounded side effect.
// It does not decide what a successful deploy means beyond the command's exit
// status, and it never rolls one back — detecting a bad deploy after the fact is
// a separate concern (#118).
package deployer

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DefaultTimeoutSeconds bounds one deploy when the project does not set its own.
const DefaultTimeoutSeconds = 900

// Decision is what the lane concluded about a project this tick.
type Decision string

const (
	// DecisionDeploy means the base branch carries a commit that has never been
	// deployed successfully.
	DecisionDeploy Decision = "deploy"
	// DecisionUpToDate means the current head is already deployed.
	DecisionUpToDate Decision = "up_to_date"
	// DecisionSkip means the project is not configured for deploys.
	DecisionSkip Decision = "skip"
	// DecisionRetryLater means a previous deploy of this commit is still running
	// somewhere; Looper does not start a second one.
	DecisionRetryLater Decision = "retry_later"
)

// HeadState is the forge state the decision is made from.
type HeadState struct {
	// SHA is the current head of the project's base branch.
	SHA string
	// Deployed reports whether this exact commit already has a Looper deployment.
	Deployed bool
	// DeployedState is that deployment's latest status, empty when none was
	// recorded.
	DeployedState string
}

// Decide reports what to do with the current head.
//
// A commit whose deployment failed is deliberately *not* retried automatically:
// a deploy that fails tends to keep failing, and a lane that retries every tick
// turns one broken deploy into an unbounded stream of them. The failure is
// visible on the commit and in the notification; re-running is a human's call.
func Decide(enabled bool, command string, head HeadState) Decision {
	if !enabled || strings.TrimSpace(command) == "" || strings.TrimSpace(head.SHA) == "" {
		return DecisionSkip
	}
	if !head.Deployed {
		return DecisionDeploy
	}
	switch head.DeployedState {
	case string(stateInProgress):
		return DecisionRetryLater
	default:
		// success, failure, or any state GitHub adds later: the commit has been
		// acted on, so this lane is done with it.
		return DecisionUpToDate
	}
}

const stateInProgress = "in_progress"

// Outcome is the result of one deploy.
type Outcome struct {
	SHA          string
	PreviousSHA  string
	ExitCode     int
	Succeeded    bool
	Duration     time.Duration
	OutputTail   string
	DeploymentID int64
}

// Deps are the effects a deploy needs, injected so the sequencing below is
// testable without a forge or a shell.
type Deps struct {
	// Head reports the current base-branch head and whether it is already
	// deployed.
	Head func(ctx context.Context) (HeadState, error)
	// PreviousSHA reports the last commit successfully deployed, or "" when there
	// is none. It is used only to build a comparison link for the human.
	PreviousSHA func(ctx context.Context) (string, error)
	// CreateDeployment records the intent and returns the deployment id.
	CreateDeployment func(ctx context.Context, sha string) (int64, error)
	// RunCommand executes the deploy command.
	RunCommand func(ctx context.Context) (exitCode int, output string, err error)
	// SetStatus records the outcome against the deployment.
	SetStatus func(ctx context.Context, deploymentID int64, succeeded bool, description string) error
	// Notify tells a human what happened.
	Notify  func(ctx context.Context, outcome Outcome)
	LogWarn func(msg string, fields map[string]any)
}

// Run performs at most one deploy.
//
// The deployment is created *before* the command runs, so a crash mid-deploy
// leaves an in-progress record rather than an untracked side effect: the next
// tick sees it and declines to run a second copy of a deploy that may still be
// going. That is the deliberate direction — a stalled lane is visible and
// recoverable by a human, whereas two concurrent deploys of different commits
// are not.
func Run(ctx context.Context, enabled bool, command string, deps Deps) (Decision, *Outcome, error) {
	head, err := deps.Head(ctx)
	if err != nil {
		return "", nil, err
	}
	decision := Decide(enabled, command, head)
	if decision != DecisionDeploy {
		return decision, nil, nil
	}

	previousSHA := ""
	if deps.PreviousSHA != nil {
		// Only used to build a comparison link, so a failure here must not stop the
		// deploy.
		if sha, err := deps.PreviousSHA(ctx); err == nil {
			previousSHA = sha
		} else if deps.LogWarn != nil {
			deps.LogWarn("deployer: could not resolve the previously deployed commit", map[string]any{"error": err.Error()})
		}
	}

	deploymentID, err := deps.CreateDeployment(ctx, head.SHA)
	if err != nil {
		return decision, nil, fmt.Errorf("record deployment for %s: %w", short(head.SHA), err)
	}

	started := time.Now()
	exitCode, output, runErr := deps.RunCommand(ctx)
	outcome := Outcome{
		SHA:          head.SHA,
		PreviousSHA:  previousSHA,
		ExitCode:     exitCode,
		Succeeded:    runErr == nil && exitCode == 0,
		Duration:     time.Since(started),
		OutputTail:   output,
		DeploymentID: deploymentID,
	}

	description := fmt.Sprintf("looper deploy of %s succeeded", short(head.SHA))
	if !outcome.Succeeded {
		description = fmt.Sprintf("looper deploy of %s failed (exit %d)", short(head.SHA), exitCode)
		if runErr != nil {
			description = fmt.Sprintf("looper deploy of %s failed: %v", short(head.SHA), runErr)
		}
	}
	if err := deps.SetStatus(ctx, deploymentID, outcome.Succeeded, description); err != nil && deps.LogWarn != nil {
		// The deploy itself already happened. Failing to record the status leaves
		// the deployment in-progress, which the next tick will decline to redo —
		// so this is loud rather than silent.
		deps.LogWarn("deployer: could not record the deployment status; the lane will not retry this commit", map[string]any{
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

// CompareURL links the commits between the last deployed commit and this one —
// the "what changed" a human wants when told something shipped. Empty when there
// is no previous deploy to compare against.
func CompareURL(repo, previousSHA, sha string) string {
	previousSHA = strings.TrimSpace(previousSHA)
	sha = strings.TrimSpace(sha)
	repo = strings.TrimSpace(repo)
	if repo == "" || previousSHA == "" || sha == "" || previousSHA == sha {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/compare/%s...%s", repo, previousSHA, sha)
}
