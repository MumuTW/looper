package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/deployer"
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
