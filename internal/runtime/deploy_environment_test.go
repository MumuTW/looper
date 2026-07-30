package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/deployer"
)

// This test runs a real /bin/sh on purpose.
//
// The defect it covers survived a full test suite because every deploy test
// injected a fake RunCommand, so nothing ever reached shell.Run — which replaces
// the child environment outright when Env is non-empty. One entry in
// roles.deployer.environment was enough to strip PATH and stop the shell finding
// the program it was told to run.
func TestDeployCommandInheritsTheDaemonEnvironment(t *testing.T) {
	logDir := t.TempDir()
	dir := t.TempDir()
	t.Setenv("LOOPER_DEPLOY_INHERITED", "inherited-value")

	role := config.DeployerRoleConfig{
		Command:     `printf '%s|%s|%s' "$LOOPER_DEPLOY_INHERITED" "$LOOPER_DEPLOY_OVERRIDE" "$PATH"`,
		Environment: map[string]string{"LOOPER_DEPLOY_OVERRIDE": "override-value"},
	}

	exitCode, logPath, err := runDeployCommand(context.Background(), dir, logDir, role, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("runDeployCommand() error = %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exit = %d", exitCode)
	}
	captured, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read deploy log: %v", readErr)
	}
	parts := strings.SplitN(strings.TrimSpace(string(captured)), "|", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected output %q", captured)
	}
	if parts[0] != "inherited-value" {
		t.Fatalf("inherited variable = %q, want it preserved alongside the override", parts[0])
	}
	if parts[1] != "override-value" {
		t.Fatalf("configured variable = %q", parts[1])
	}
	if strings.TrimSpace(parts[2]) == "" {
		t.Fatal("PATH was empty — the configured environment replaced the inherited one")
	}
}

// No configured environment means inherit, which is what shell.Run already does
// for a nil map.
func TestDeployEnvironmentIsNilWhenNothingIsConfigured(t *testing.T) {
	t.Parallel()

	if env := deployEnvironment(nil); env != nil {
		t.Fatalf("deployEnvironment(nil) = %v, want nil", env)
	}
	if env := deployEnvironment(map[string]string{}); env != nil {
		t.Fatalf("deployEnvironment(empty) = %v, want nil", env)
	}
}

func TestDeployEnvironmentOverridesInheritedValues(t *testing.T) {
	t.Setenv("LOOPER_DEPLOY_CONFLICT", "from-daemon")

	env := deployEnvironment(map[string]string{"LOOPER_DEPLOY_CONFLICT": "from-config"})

	if env["LOOPER_DEPLOY_CONFLICT"] != "from-config" {
		t.Fatalf("conflict resolved to %q, want the configured value", env["LOOPER_DEPLOY_CONFLICT"])
	}
	if _, ok := env["PATH"]; !ok {
		t.Fatal("merged environment lost PATH")
	}
}

// A shell that could not start is not a deploy that failed. Recording a failure
// would mark the commit permanently acted on for a transient local condition.
func TestUnstartableCommandIsNotAFailedDeploy(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	role := config.DeployerRoleConfig{Command: "true"}

	// A working directory that does not exist prevents the process from starting.
	_, _, err := runDeployCommand(context.Background(), filepath.Join(t.TempDir(), "missing"), logDir, role, 30*time.Second, nil)

	if err == nil {
		t.Fatal("runDeployCommand() succeeded with an unusable working directory")
	}
	if !strings.Contains(err.Error(), deployer.ErrCommandNotStarted.Error()) {
		t.Fatalf("error = %v, want it classified as a command that did not start", err)
	}
}
