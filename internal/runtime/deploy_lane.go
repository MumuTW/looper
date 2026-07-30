package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/deployer"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

// deployCleanupTimeout bounds releasing a deploy checkout. Cleanup runs on a
// context detached from the deploy so cancellation still frees the worktree, and
// that detachment is exactly why it needs its own deadline.
const deployCleanupTimeout = 2 * time.Minute

// deployLogCaptureBytes bounds what is written to a deploy's log file.
const deployLogCaptureBytes = 1 << 20

// runDeployLane deploys a project's base branch when it carries a commit Looper
// has not acted on yet.
//
// GitHub Deployments are the authority for "this commit was deployed", following
// the same preference for forge-native state as the dependency gate (ADR-0004)
// and auto-merge (ADR-0005). Looper keeps no private record that could disagree.
func runDeployLane(ctx context.Context, input defaultSchedulerTickInput, project storage.ProjectRecord, repo string) {
	if input.Config == nil || input.GitHubGateway == nil || input.GitGateway == nil || strings.TrimSpace(repo) == "" {
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

	timeout := time.Duration(role.TimeoutSeconds) * time.Second
	if role.TimeoutSeconds <= 0 {
		timeout = deployer.DefaultTimeoutSeconds * time.Second
	}
	gateway := input.GitHubGateway
	logDir := filepath.Join(input.Config.Daemon.LogDir, "deploys")

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
			return deployer.HeadState{
				SHA: sha, Deployed: found,
				State:     deployer.DeploymentState(record.State),
				StartedAt: record.CreatedAt,
			}, nil
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
		SetStatus: func(ctx context.Context, deploymentID int64, state deployer.DeploymentState, description string) error {
			return gateway.SetDeploymentStatus(ctx, githubinfra.DeploymentStatusInput{
				Repo: repo, DeploymentID: deploymentID,
				State: githubinfra.DeploymentState(state), Description: description, CWD: project.RepoPath,
			})
		},
		Materialize: func(ctx context.Context, sha string) (string, func(), error) {
			checkout, err := input.GitGateway.CreateDeployCheckout(ctx, gitinfra.DeployCheckoutInput{
				RepoPath: project.RepoPath, SHA: sha, Root: deployCheckoutRoot(*input.Config, project),
			})
			if err != nil {
				return "", func() {}, err
			}
			return checkout.Path, func() {
				// Detached from the deploy's context so a cancelled deploy still
				// releases its checkout, but bounded: an unbounded cleanup can hold
				// shutdown open indefinitely.
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployCleanupTimeout)
				defer cancel()
				if err := input.GitGateway.RemoveDeployCheckout(releaseCtx, checkout, project.RepoPath); err != nil && input.Logger != nil {
					input.Logger.Warn("deployer: could not release the deploy checkout", map[string]any{
						"path": checkout.Path, "error": err.Error(),
					})
				}
			}, nil
		},
		RunCommand: func(ctx context.Context, dir string) (int, string, error) {
			return runDeployCommand(ctx, dir, logDir, role, timeout, input.Logger)
		},
	}
	if input.Logger != nil {
		deps.LogWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}
	if input.OnDeployFinished != nil {
		host, _ := githubinfra.SplitRepoHostname(repo)
		deps.Notify = func(ctx context.Context, outcome deployer.Outcome) {
			input.OnDeployFinished(ctx, DeployNotification{
				ProjectID: project.ID, Repo: repo, BaseBranch: baseBranch, Outcome: outcome,
				CompareURL: deployer.CompareURL(host, repoWithoutHost(repo), outcome.PreviousSHA, outcome.SHA),
			})
		}
	}

	decision, outcome, err := deployer.Run(ctx, role.Enabled, role.Command, timeout, input.Now(), deps)
	if err != nil {
		if input.Logger != nil {
			input.Logger.Warn("deployer: deploy failed", map[string]any{"projectId": project.ID, "repo": repo, "error": err.Error()})
		}
		return
	}
	if outcome != nil && input.Logger != nil {
		input.Logger.Info("deployer: deploy finished", map[string]any{
			"projectId": project.ID, "repo": repo, "sha": outcome.SHA,
			"succeeded": outcome.Succeeded, "exitCode": outcome.ExitCode,
			"durationMs": outcome.Duration.Milliseconds(), "log": outcome.LogPath,
		})
		return
	}
	if decision == deployer.DecisionInProgress && input.Logger != nil {
		input.Logger.Debug("deployer: a deploy of this commit is still in progress", map[string]any{"projectId": project.ID})
	}
}

// runDeployCommand executes the deploy command in the materialized checkout and
// captures its output to a file.
//
// Output goes to a file rather than into the outcome because a deploy command's
// stdout routinely contains tokens, signed URLs, and connection strings, and the
// outcome reaches notifications. The path is reported; the contents are not.
//
// Unlike validation commands this runs with network access: a deploy that cannot
// reach anything is not a deploy.
func runDeployCommand(ctx context.Context, dir, logDir string, role config.DeployerRoleConfig, timeout time.Duration, logger bootstrap.Logger) (int, string, error) {
	result, runErr := shell.Run(ctx, shell.Options{
		Command:          "/bin/sh",
		Args:             []string{"-c", role.Command},
		CWD:              dir,
		Env:              deployEnvironment(role.Environment),
		Timeout:          timeout,
		MaxCapturedBytes: deployLogCaptureBytes,
	})

	if runErr != nil && result.ExitCode == 0 {
		// shell.Run reports a non-zero exit through ExitCode. An error with no exit
		// code means the process never ran, which is not a deploy that failed.
		runErr = fmt.Errorf("%w: %v", deployer.ErrCommandNotStarted, runErr)
	}

	logPath, writeErr := writeDeployLog(logDir, result)
	if writeErr != nil {
		// The deploy itself is what matters, so a lost log does not change its
		// outcome — but silence would leave someone hunting for a file that was
		// never written.
		logPath = ""
		if logger != nil {
			logger.Warn("deployer: could not capture the deploy log", map[string]any{"logDir": logDir, "error": writeErr.Error()})
		}
	}
	return result.ExitCode, logPath, runErr
}

// deployEnvironment merges the configured values over the daemon's own.
//
// shell.Run replaces the child environment outright when Env is non-empty, so
// passing only the configured values would strip PATH, HOME, and everything else
// a deploy command needs — one entry in roles.deployer.environment was enough to
// stop /bin/sh finding the program it was asked to run.
func deployEnvironment(overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		// Empty means "inherit", which is what shell.Run already does for a nil map.
		return nil
	}
	merged := make(map[string]string, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		merged[key] = value
	}
	for key, value := range overrides {
		merged[key] = value
	}
	return merged
}

func writeDeployLog(logDir string, result shell.Result) (string, error) {
	if strings.TrimSpace(logDir) == "" {
		return "", fmt.Errorf("deploy log directory is not configured")
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(logDir, "deploy-*.log")
	if err != nil {
		return "", err
	}
	defer file.Close()
	// 0600: the file holds whatever the deploy command printed, which is exactly
	// the material that must not be widely readable.
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := file.WriteString(result.Stdout); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Stderr) != "" {
		if _, err := file.WriteString("\n--- stderr ---\n" + result.Stderr); err != nil {
			return "", err
		}
	}
	return file.Name(), nil
}

// deployCheckoutRoot is where ephemeral deploy checkouts live. It sits beside the
// project's own worktrees so it inherits the same disk and permissions.
func deployCheckoutRoot(cfg config.Config, project storage.ProjectRecord) string {
	return filepath.Join(cfg.Daemon.LogDir, "..", "deploy-checkouts", project.ID)
}

// repoWithoutHost strips an enterprise hostname prefix, leaving owner/name.
func repoWithoutHost(repo string) string {
	_, slug := githubinfra.SplitRepoHostname(repo)
	return slug
}

// DeployNotification is what the daemon reports after a deploy.
type DeployNotification struct {
	ProjectID  string
	Repo       string
	BaseBranch string
	Outcome    deployer.Outcome
	// CompareURL links what changed since the last successful deploy, which is
	// what someone actually checks after being told something shipped.
	CompareURL string
}

func (n DeployNotification) Title() string {
	if n.Outcome.Succeeded {
		return "Looper deployed " + n.Repo
	}
	return "Looper deploy failed: " + n.Repo
}

func (n DeployNotification) Subtitle() string {
	return fmt.Sprintf("%s @ %s (%s)", n.BaseBranch, shortSHA(n.Outcome.SHA), n.Outcome.Duration.Round(time.Second))
}

// Body deliberately carries no command output — only where to read it. The
// notification is delivered to chat and desktop surfaces, and deploy output is
// the most credential-dense text this daemon handles.
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
	if path := strings.TrimSpace(n.Outcome.LogPath); path != "" {
		b.WriteString("\nOutput: " + path)
	}
	return b.String()
}

func deployerRoleForProject(cfg config.Config, projectID string) config.DeployerRoleConfig {
	role := cfg.Roles.Deployer
	for _, project := range cfg.Projects {
		if !strings.EqualFold(strings.TrimSpace(project.ID), strings.TrimSpace(projectID)) {
			continue
		}
		if project.Roles != nil && project.Roles.Deployer != nil {
			config.MergeDeployerRoleConfig(&role, *project.Roles.Deployer)
		}
		break
	}
	return role
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

// deployInFlight tracks which projects have a deploy running, so the tick that
// starts one does not start another before it finishes.
var deployInFlight sync.Map

// startDeployLane runs the deploy off the scheduler tick.
//
// A deploy executes an operator command for up to roles.deployer.timeoutSeconds
// — fifteen minutes by default — and the tick is serial. Running it inline would
// stall discovery for every project behind it, which is the same mistake the
// Telegram intake lane avoids by not long-polling.
//
// One deploy per project at a time: a second would race the first over the same
// checkout path and the same deployment record.
func startDeployLane(ctx context.Context, input defaultSchedulerTickInput, project storage.ProjectRecord, repo string) {
	if input.Config == nil {
		return
	}
	role := deployerRoleForProject(*input.Config, project.ID)
	if !role.Enabled || strings.TrimSpace(role.Command) == "" {
		return
	}
	if _, running := deployInFlight.LoadOrStore(project.ID, struct{}{}); running {
		if input.Logger != nil {
			input.Logger.Debug("deployer: a deploy is already running for this project", map[string]any{"projectId": project.ID})
		}
		return
	}
	// Detached from the tick's context so a finished tick does not cancel a deploy
	// mid-flight; the command's own timeout is what bounds it.
	deployCtx := context.WithoutCancel(ctx)
	go func() {
		defer deployInFlight.Delete(project.ID)
		runDeployLane(deployCtx, input, project, repo)
	}()
}
