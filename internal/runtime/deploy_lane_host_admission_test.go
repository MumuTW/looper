package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/hostresources"
	gitinfra "github.com/MumuTW/looper/internal/infra/git"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/storage"
)

// Host pressure must gate the deploy lane, not only discovery. The
// discovery-lane hold breaks the inner lane loop, but the tick then reaches
// startDeployLane, whose runDeployCommand executes an operator shell command
// for up to the deploy timeout — the most resource-intensive action a tick
// takes. The same host admission decision that withholds claims and discovery
// must withhold a deploy, or low disk / high load still launches one.
//
// The deploy goroutine is kept in flight by a ghRun that blocks at Head
// (GetBranchHeadSHA), so deployInFlight stays set when the host admits and
// stays empty when a hold withholds the launch — the regression is the gap
// between those two.
func TestStartDeployLaneWithholdsUnderHostPressure(t *testing.T) {
	projectID := "host-pressure-deploy"
	repo := "acme/looper"
	baseBranch := "main"

	// release unblocks the in-flight deploy's Head call so its goroutine can
	// drain; deployDone waits for that drain so the test leaves no leaked
	// goroutine behind.
	release := make(chan struct{})
	deployDone := make(chan struct{})
	var once sync.Once
	signalDone := func() { once.Do(func() { close(deployDone) }) }

	ghGateway := githubinfra.New(githubinfra.Options{
		GHRun: func(ctx context.Context, _ shell.Options) (shell.Result, error) {
			select {
			case <-release:
				// Head returns an error from here, deployer.Run returns, and the
				// deploy goroutine drains — signal once that drain has begun.
				signalDone()
				return shell.Result{}, errors.New("test: deploy head released")
			case <-ctx.Done():
				signalDone()
				return shell.Result{}, ctx.Err()
			}
		},
	})
	gitGateway := gitinfra.New(gitinfra.Options{})

	cfg := &config.Config{
		Defaults: config.DefaultsConfig{BaseBranch: baseBranch},
		Roles:    config.RoleConfigs{Deployer: config.DeployerRoleConfig{Enabled: true, Command: "true"}},
		Daemon:   config.DaemonConfig{LogDir: t.TempDir()},
	}
	project := storage.ProjectRecord{ID: projectID, RepoPath: t.TempDir()}

	hold := &hostresources.Decision{
		Admit:   false,
		Reasons: []string{hostresources.ReasonDiskLow},
	}
	admit := &hostresources.Decision{Admit: true}
	now := time.Now()

	// When the host is under pressure, no deploy may launch: deployInFlight
	// must not carry the project, even though the deployer role is enabled and
	// the base head is eligible.
	holdInput := defaultSchedulerTickInput{
		Config: cfg, GitHubGateway: ghGateway, GitGateway: gitGateway,
		Now:           func() time.Time { return now },
		HostAdmission: func() *hostresources.Decision { return hold },
	}
	startDeployLane(context.Background(), holdInput, project, repo)
	if _, set := deployInFlight.Load(projectID); set {
		t.Fatalf("deployInFlight holds %s under host pressure; the deploy lane launched a resource-intensive command", projectID)
	}

	// When the host admits, the deploy lane launches and the goroutine blocks
	// at Head, so deployInFlight stays set — proving the gate withholds under
	// pressure rather than blocking every deploy.
	admittedInput := defaultSchedulerTickInput{
		Config: cfg, GitHubGateway: ghGateway, GitGateway: gitGateway,
		Now:           func() time.Time { return now },
		HostAdmission: func() *hostresources.Decision { return admit },
	}
	startDeployLane(context.Background(), admittedInput, project, repo)
	if _, set := deployInFlight.Load(projectID); !set {
		t.Fatalf("deployInFlight missing %s when the host admits; the gate over-blocks a healthy deploy", projectID)
	}

	// Release the in-flight deploy and wait for its goroutine to drain so the
	// package-level deployInFlight is clean for later tests.
	close(release)
	select {
	case <-deployDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("in-flight deploy goroutine did not drain after release")
	}
	deployInFlight.Delete(projectID)
}

func TestStartDeployLaneOwnsDetachedLifecycleUntilShutdown(t *testing.T) {
	projectID := "detached-deploy-operation"
	repo := "acme/looper"
	baseBranch := "main"
	headEntered := make(chan struct{})
	var enteredOnce sync.Once

	ghGateway := githubinfra.New(githubinfra.Options{
		GHRun: func(ctx context.Context, _ shell.Options) (shell.Result, error) {
			enteredOnce.Do(func() { close(headEntered) })
			<-ctx.Done()
			return shell.Result{}, ctx.Err()
		},
	})
	gitGateway := gitinfra.New(gitinfra.Options{})
	cfg := &config.Config{
		Defaults: config.DefaultsConfig{BaseBranch: baseBranch},
		Roles:    config.RoleConfigs{Deployer: config.DeployerRoleConfig{Enabled: true, Command: "true"}},
		Daemon:   config.DaemonConfig{LogDir: t.TempDir()},
	}
	project := storage.ProjectRecord{ID: projectID, RepoPath: t.TempDir()}
	registry := NewActiveExecutionRegistry()
	input := defaultSchedulerTickInput{
		Config: cfg, GitHubGateway: ghGateway, GitGateway: gitGateway,
		Now: func() time.Time { return time.Now() }, OperationOwner: registry,
	}

	startDeployLane(context.Background(), input, project, repo)
	select {
	case <-headEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("detached deploy did not reach its GitHub read")
	}
	if snapshot := registry.DrainSnapshot(); snapshot.BoundOperations != 1 {
		t.Fatalf("drain snapshot while GitHub read is blocked = %#v, want one bound deploy operation", snapshot)
	}

	if err := registry.BeginShutdown("test deploy shutdown"); err != nil {
		t.Fatalf("BeginShutdown() error = %v", err)
	}
	if snapshot := registry.DrainSnapshot(); !snapshot.Drained() {
		t.Fatalf("drain snapshot after deploy shutdown = %#v, want fully drained", snapshot)
	}
	if _, running := deployInFlight.Load(projectID); running {
		t.Fatalf("deployInFlight still contains %s after shutdown", projectID)
	}
}
