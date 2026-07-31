package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/deployer"
	"github.com/MumuTW/looper/internal/storage"
)

func deployNotification(succeeded bool, compareURL, logPath string) DeployNotification {
	return DeployNotification{
		ProjectID: "looper", Repo: "acme/looper", BaseBranch: "main",
		CompareURL: compareURL,
		Outcome: deployer.Outcome{
			SHA: "abc123def456", PreviousSHA: "old9999", Succeeded: succeeded,
			ExitCode: map[bool]int{true: 0, false: 2}[succeeded],
			Duration: 42 * time.Second, DeploymentID: 7, LogPath: logPath,
		},
	}
}

// The point of the notification is that a person can go and check the change.
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

// Deploy output is the most credential-dense text this daemon handles, and
// notifications reach chat and desktop surfaces. The body carries where to read
// the log, never the log.
func TestDeployNotificationCarriesALogPathNotOutput(t *testing.T) {
	t.Parallel()

	failed := deployNotification(false, "", "/var/log/looper/deploys/deploy-123.log")
	body := failed.Body()

	if !strings.Contains(body, "/var/log/looper/deploys/deploy-123.log") {
		t.Fatalf("Body() omits the log path:\n%s", body)
	}
	if !strings.Contains(body, "exit code 2") {
		t.Fatalf("Body() omits the exit code:\n%s", body)
	}
	// The Outcome type has no output field at all, so there is nothing to leak;
	// this pins that the notification does not reintroduce one.
	if strings.Contains(body, "stderr") || strings.Contains(body, "stdout") {
		t.Fatalf("Body() carries command output:\n%s", body)
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
		t.Fatalf("deployBaseBranch() = %q, want the override", got)
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

// An enterprise install serves the same paths from its own domain, so the host
// has to come from the repository rather than being assumed.
func TestCompareURLUsesTheRepositoryHost(t *testing.T) {
	t.Parallel()

	if got := repoWithoutHost("git.acme.internal/acme/looper"); got != "acme/looper" {
		t.Fatalf("repoWithoutHost() = %q", got)
	}
	if got := repoWithoutHost("acme/looper"); got != "acme/looper" {
		t.Fatalf("repoWithoutHost() = %q", got)
	}
}
