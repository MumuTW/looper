package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/deployer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

// deployOutputTailBytes bounds the captured command output kept for reporting. A
// deploy can print megabytes; what a human needs in a notification is the end.
const deployOutputTailBytes = 4000

// runDeployLane deploys a project's base branch when it carries a commit Looper
// has not acted on yet.
//
// GitHub Deployments are the authority for "this commit was deployed", following
// the same preference for forge-native state as the dependency gate (ADR-0004)
// and auto-merge (ADR-0005). Looper keeps no private record that could disagree.
func runDeployLane(ctx context.Context, input defaultSchedulerTickInput, project storage.ProjectRecord, repo string) {
	if input.Config == nil || input.GitHubGateway == nil || strings.TrimSpace(repo) == "" {
		return
	}
	role := deployerRoleForProject(*input.Config, project.ID)
	if !role.Enabled || strings.TrimSpace(role.Command) == "" {
		return
	}
	baseBranch := deployBaseBranch(*input.Config, project)
	if baseBranch == "" {
		if input.Logger != nil {
			input.Logger.Warn("deployer: project has no base branch", map[string]any{"projectId": project.ID})
		}
		return
	}

	gateway := input.GitHubGateway
	deps := deployer.Deps{
		Head: func(ctx context.Context) (deployer.HeadState, error) {
			sha, err := gateway.GetBranchHeadSHA(ctx, githubinfra.BranchHeadInput{Repo: repo, Branch: baseBranch, CWD: project.RepoPath})
			if err != nil {
				return deployer.HeadState{}, err
			}
			record, found, err := gateway.LatestDeploymentForSHA(ctx, githubinfra.ListDeploymentsInput{Repo: repo, SHA: sha, CWD: project.RepoPath})
			if err != nil {
				return deployer.HeadState{}, err
			}
			return deployer.HeadState{SHA: sha, Deployed: found, DeployedState: string(record.State)}, nil
		},
		PreviousSHA: func(ctx context.Context) (string, error) {
			return gateway.LatestSuccessfulDeploymentSHA(ctx, githubinfra.ListDeploymentsInput{Repo: repo, CWD: project.RepoPath})
		},
		CreateDeployment: func(ctx context.Context, sha string) (int64, error) {
			return gateway.CreateDeployment(ctx, githubinfra.CreateDeploymentInput{
				Repo: repo, SHA: sha, CWD: project.RepoPath,
				Description: "looper deploy of " + baseBranch,
			})
		},
		RunCommand: func(ctx context.Context) (int, string, error) {
			return runDeployCommand(ctx, project, role)
		},
		SetStatus: func(ctx context.Context, deploymentID int64, succeeded bool, description string) error {
			state := githubinfra.DeploymentStateFailure
			if succeeded {
				state = githubinfra.DeploymentStateSuccess
			}
			return gateway.SetDeploymentStatus(ctx, githubinfra.DeploymentStatusInput{
				Repo: repo, DeploymentID: deploymentID, State: state, Description: description, CWD: project.RepoPath,
			})
		},
	}
	if input.Logger != nil {
		deps.LogWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}
	if input.OnDeployFinished != nil {
		deps.Notify = func(ctx context.Context, outcome deployer.Outcome) {
			input.OnDeployFinished(ctx, DeployNotification{
				ProjectID:  project.ID,
				Repo:       repo,
				BaseBranch: baseBranch,
				Outcome:    outcome,
				CompareURL: deployer.CompareURL(repo, outcome.PreviousSHA, outcome.SHA),
			})
		}
	}

	decision, outcome, err := deployer.Run(ctx, role.Enabled, role.Command, deps)
	if err != nil {
		if input.Logger != nil {
			input.Logger.Warn("deployer: deploy failed", map[string]any{"projectId": project.ID, "repo": repo, "error": err.Error()})
		}
		return
	}
	if outcome != nil && input.Logger != nil {
		input.Logger.Info("deployer: deploy finished", map[string]any{
			"projectId": project.ID, "repo": repo, "sha": outcome.SHA,
			"succeeded": outcome.Succeeded, "exitCode": outcome.ExitCode, "durationMs": outcome.Duration.Milliseconds(),
		})
		return
	}
	if decision == deployer.DecisionRetryLater && input.Logger != nil {
		input.Logger.Debug("deployer: a deploy of this commit is still in progress", map[string]any{"projectId": project.ID})
	}
}

// runDeployCommand executes the project's deploy command from its repository
// root. Unlike validation commands this runs with network access and the
// daemon's own environment: a deploy that cannot reach anything is not a deploy.
func runDeployCommand(ctx context.Context, project storage.ProjectRecord, role config.DeployerRoleConfig) (int, string, error) {
	timeout := time.Duration(role.TimeoutSeconds) * time.Second
	if role.TimeoutSeconds <= 0 {
		timeout = deployer.DefaultTimeoutSeconds * time.Second
	}
	result, err := shell.Run(ctx, shell.Options{
		Command:          "/bin/sh",
		Args:             []string{"-c", role.Command},
		CWD:              project.RepoPath,
		Env:              role.Environment,
		Timeout:          timeout,
		MaxCapturedBytes: deployOutputTailBytes,
	})
	output := strings.TrimSpace(result.Stdout)
	if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
		if output != "" {
			output += "\n"
		}
		output += stderr
	}
	if err != nil {
		return result.ExitCode, output, err
	}
	return result.ExitCode, output, nil
}

// DeployNotification is what the daemon reports after a deploy.
type DeployNotification struct {
	ProjectID  string
	Repo       string
	BaseBranch string
	Outcome    deployer.Outcome
	// CompareURL links what changed since the last successful deploy, which is
	// the thing a human actually wants to check.
	CompareURL string
}

// Title, Subtitle, and Body render the notification. Kept beside the lane so the
// wording and the data stay together.
func (n DeployNotification) Title() string {
	if n.Outcome.Succeeded {
		return "Looper deployed " + n.Repo
	}
	return "Looper deploy failed: " + n.Repo
}

func (n DeployNotification) Subtitle() string {
	return fmt.Sprintf("%s @ %s (%s)", n.BaseBranch, shortSHA(n.Outcome.SHA), n.Outcome.Duration.Round(time.Second))
}

func (n DeployNotification) Body() string {
	var b strings.Builder
	if n.Outcome.Succeeded {
		b.WriteString("Deploy succeeded.")
	} else {
		b.WriteString(fmt.Sprintf("Deploy failed with exit code %d.", n.Outcome.ExitCode))
	}
	if n.CompareURL != "" {
		b.WriteString("\nChanges since the last deploy: " + n.CompareURL)
	}
	if tail := strings.TrimSpace(n.Outcome.OutputTail); tail != "" && !n.Outcome.Succeeded {
		// Only on failure: a successful deploy's output is noise, while a failure's
		// last lines are usually the whole diagnosis.
		b.WriteString("\n\n" + tail)
	}
	return b.String()
}

func deployerRoleForProject(cfg config.Config, projectID string) config.DeployerRoleConfig {
	return config.ProjectRoleConfigs(cfg, projectID).Deployer
}

func deployBaseBranch(cfg config.Config, project storage.ProjectRecord) string {
	if project.BaseBranch != nil && strings.TrimSpace(*project.BaseBranch) != "" {
		return strings.TrimSpace(*project.BaseBranch)
	}
	return strings.TrimSpace(cfg.Defaults.BaseBranch)
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
