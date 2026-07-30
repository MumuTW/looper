package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/deployer"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/storage"
)

func deployNotification(succeeded bool, compareURL, tail string) DeployNotification {
	return DeployNotification{
		ProjectID: "looper", Repo: "acme/looper", BaseBranch: "main",
		CompareURL: compareURL,
		Outcome: deployer.Outcome{
			SHA: "abc123def456", PreviousSHA: "old9999", Succeeded: succeeded,
			ExitCode: map[bool]int{true: 0, false: 2}[succeeded],
			Duration: 42 * time.Second, OutputTail: tail, DeploymentID: 7,
		},
	}
}

// The point of the notification is that the human can go and check the change,
// so the comparison link has to be in it.
func TestDeployNotificationLinksWhatChanged(t *testing.T) {
	t.Parallel()

	notification := deployNotification(true, "https://github.com/acme/looper/compare/old9999...abc123def456", "")

	if !strings.Contains(notification.Title(), "acme/looper") {
		t.Fatalf("Title() = %q", notification.Title())
	}
	if !strings.Contains(notification.Subtitle(), "abc123d") {
		t.Fatalf("Subtitle() = %q, want the short sha", notification.Subtitle())
	}
	if !strings.Contains(notification.Body(), "compare/old9999...abc123def456") {
		t.Fatalf("Body() omits the comparison link:\n%s", notification.Body())
	}
}

// A failure's last output lines are usually the whole diagnosis; a success's are
// noise.
func TestDeployNotificationIncludesOutputOnlyOnFailure(t *testing.T) {
	t.Parallel()

	failed := deployNotification(false, "", "Error: connection refused")
	if !strings.Contains(failed.Body(), "connection refused") {
		t.Fatalf("failed Body() omits the output:\n%s", failed.Body())
	}
	if !strings.Contains(failed.Title(), "failed") {
		t.Fatalf("failed Title() = %q", failed.Title())
	}
	if !strings.Contains(failed.Body(), "exit code 2") {
		t.Fatalf("failed Body() omits the exit code:\n%s", failed.Body())
	}

	succeeded := deployNotification(true, "", "npm notice ...")
	if strings.Contains(succeeded.Body(), "npm notice") {
		t.Fatalf("successful Body() carries command noise:\n%s", succeeded.Body())
	}
}

func TestDeployBaseBranchPrefersTheProjectOverride(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Defaults: config.DefaultsConfig{BaseBranch: "main"}}
	override := "release"

	if got := deployBaseBranch(cfg, storage.ProjectRecord{}); got != "main" {
		t.Fatalf("deployBaseBranch() = %q, want the default", got)
	}
	if got := deployBaseBranch(cfg, storage.ProjectRecord{BaseBranch: &override}); got != "release" {
		t.Fatalf("deployBaseBranch() = %q, want the project override", got)
	}
	blank := "   "
	if got := deployBaseBranch(cfg, storage.ProjectRecord{BaseBranch: &blank}); got != "main" {
		t.Fatalf("deployBaseBranch() = %q, want a blank override ignored", got)
	}
}

// A deploy command differs per repository, so the project override is the case
// that matters rather than an extra.
func TestDeployerRoleReadsProjectOverrides(t *testing.T) {
	t.Parallel()
	enabled := true
	command := "./scripts/deploy-novel.sh"
	cfg := config.Config{
		Roles: config.RoleConfigs{Deployer: config.DeployerRoleConfig{Enabled: false, Command: "make deploy"}},
		Projects: []config.ProjectRefConfig{{
			ID: "novel",
			Roles: &config.PartialRoleConfigs{Deployer: &config.PartialDeployerRoleConfig{
				Enabled: &enabled, Command: &command,
			}},
		}},
	}

	role := deployerRoleForProject(cfg, "novel")
	if !role.Enabled || role.Command != command {
		t.Fatalf("project role = %+v, want the override applied", role)
	}
	if global := deployerRoleForProject(cfg, "other"); global.Enabled || global.Command != "make deploy" {
		t.Fatalf("global role = %+v, want the override confined to its project", global)
	}
}

func TestDeploySchedulerSingleFlightsOneProject(t *testing.T) {
	t.Parallel()

	scheduler := newDeployScheduler()
	runner := &schedulerTaskTracker{}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	if !scheduler.Schedule("project", runner, func() {
		once.Do(func() { close(started) })
		<-release
	}) {
		t.Fatal("first Schedule() = false, want admitted")
	}
	<-started
	if scheduler.Schedule("project", runner, func() { t.Error("duplicate deploy ran") }) {
		t.Fatal("second Schedule() = true, want per-project single-flight refusal")
	}
	close(release)
	runner.Wait()
}

// A blocked deployment must be a scheduler-owned background task: the next
// project still reaches its integration lane in the same tick.
func TestSchedulerTickDoesNotBlockOtherProjectOnDeploy(t *testing.T) {
	workingDir := t.TempDir()
	coordinator := openMigratedCoordinator(t, filepath.Join(workingDir, "scheduler.sqlite"), t.TempDir())
	defer coordinator.Close()
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 8, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	baseBranch := "main"
	for _, project := range []struct{ id, repo string }{{"a-deploying", "acme/a"}, {"b-integrates", "acme/b"}} {
		metadata := `{"repo":"` + project.repo + `"}`
		if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: project.id, Name: project.id, RepoPath: workingDir, BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
			t.Fatalf("Projects.Upsert(%s): %v", project.id, err)
		}
	}

	deployStarted := make(chan struct{})
	var startOnce sync.Once
	github := githubinfra.New(githubinfra.Options{GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.Contains(args, "commits/main"):
			return shell.Result{Stdout: "new-head"}, nil
		case strings.Contains(args, "deployments") && strings.Contains(args, "--method POST"):
			startOnce.Do(func() { close(deployStarted) })
			return shell.Result{Stdout: `{"id": 7}`}, nil
		case strings.Contains(args, "deployments"):
			return shell.Result{Stdout: "[]"}, nil
		default:
			return shell.Result{Stdout: "{}"}, nil
		}
	}})
	blockFile := filepath.Join(workingDir, "release-deploy")
	command := "until [ -f " + blockFile + " ]; do sleep 0.01; done"
	cfg := config.Config{
		Roles:    config.RoleConfigs{Deployer: config.DeployerRoleConfig{Enabled: true, Command: command}},
		Projects: []config.ProjectRefConfig{{ID: "a-deploying"}, {ID: "b-integrates"}},
	}
	coordinatorRunner := &stubCoordinatorScheduler{}
	tasks := &schedulerTaskTracker{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := runDefaultSchedulerTick(ctx, defaultSchedulerTickInput{
		Repos: repos, GitHubGateway: github, Config: &cfg, Now: func() time.Time { return now },
		AsyncRunner: tasks, Deploys: newDeployScheduler(), Coordinator: coordinatorRunner,
		CoordinatorEnabled: func(string) bool { return true },
	}); err != nil {
		t.Fatalf("runDefaultSchedulerTick() error = %v", err)
	}
	<-deployStarted
	coordinatorRunner.mu.Lock()
	calls := coordinatorRunner.discoverCalls
	coordinatorRunner.mu.Unlock()
	if len(calls) != 2 || calls[1].ProjectID != "b-integrates" {
		t.Fatalf("coordinator discovery calls = %#v, want second project despite blocked deploy", calls)
	}
	if err := os.WriteFile(blockFile, []byte("release"), 0o600); err != nil {
		t.Fatalf("release blocked deploy: %v", err)
	}
	tasks.Wait()
}
