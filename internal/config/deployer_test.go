package config

import (
	"strings"
	"testing"
)

func deployerIssues(cfg Config) []ValidationIssue {
	var issues []ValidationIssue
	validateCoreConfig(cfg, &issues)
	deployerOnly := make([]ValidationIssue, 0, len(issues))
	for _, issue := range issues {
		if strings.Contains(issue.Path, "deployer") {
			deployerOnly = append(deployerOnly, issue)
		}
	}
	return deployerOnly
}

func TestDeployerDisabledNeedsNothing(t *testing.T) {
	t.Parallel()

	issues := deployerIssues(Config{Roles: RoleConfigs{Deployer: DeployerRoleConfig{Enabled: false}}})

	if len(issues) != 0 {
		t.Fatalf("disabled deployer produced issues: %+v", issues)
	}
}

// A project configured to deploy but unable to is otherwise only discovered on
// the first merge, which is the worst moment to learn it.
func TestDeployerEnabledRequiresACommand(t *testing.T) {
	t.Parallel()

	issues := deployerIssues(Config{Roles: RoleConfigs{Deployer: DeployerRoleConfig{Enabled: true}}})

	if len(issues) != 1 || issues[0].Path != "roles.deployer.command" {
		t.Fatalf("issues = %+v, want a missing-command issue", issues)
	}
}

func TestDeployerRejectsNegativeTimeout(t *testing.T) {
	t.Parallel()

	issues := deployerIssues(Config{Roles: RoleConfigs{Deployer: DeployerRoleConfig{
		Enabled: true, Command: "make deploy", TimeoutSeconds: -1,
	}}})

	if len(issues) != 1 || issues[0].Path != "roles.deployer.timeoutSeconds" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestDeployerRejectsInvalidEnvironmentNames(t *testing.T) {
	t.Parallel()

	issues := deployerIssues(Config{Roles: RoleConfigs{Deployer: DeployerRoleConfig{
		Enabled: true, Command: "make deploy",
		Environment: map[string]string{"not a valid name": "x"},
	}}})

	if len(issues) == 0 {
		t.Fatal("an invalid environment name passed validation")
	}
}

// A project override that enables deploys without a command must fail startup
// too: validating only the global value leaves the per-project door open.
func TestDeployerValidatesProjectOverrides(t *testing.T) {
	t.Parallel()
	enabled := true
	cfg := Config{Projects: []ProjectRefConfig{{
		ID:    "looper",
		Roles: &PartialRoleConfigs{Deployer: &PartialDeployerRoleConfig{Enabled: &enabled}},
	}}}

	issues := deployerIssues(cfg)

	found := false
	for _, issue := range issues {
		if issue.Path == "projects[0].roles.deployer.command" {
			found = true
		}
	}
	if !found {
		t.Fatalf("project override enabling deploys with no command passed; issues = %+v", issues)
	}
}

// An override inherits the global command, so enabling per project is valid on
// its own.
func TestDeployerProjectOverrideInheritsTheGlobalCommand(t *testing.T) {
	t.Parallel()
	enabled := true
	cfg := Config{
		Roles: RoleConfigs{Deployer: DeployerRoleConfig{Command: "make deploy"}},
		Projects: []ProjectRefConfig{{
			ID:    "looper",
			Roles: &PartialRoleConfigs{Deployer: &PartialDeployerRoleConfig{Enabled: &enabled}},
		}},
	}

	if issues := deployerIssues(cfg); len(issues) != 0 {
		t.Fatalf("issues = %+v, want the global command inherited", issues)
	}
}

// clonePartialRoleConfigs runs on every configuration layer. A field it forgets
// is silently dropped — the value type-checks, merges, validates, then vanishes.
// This exact omission closed a previous change.
func TestClonePartialRoleConfigsPreservesDeployer(t *testing.T) {
	t.Parallel()
	enabled := true
	command := "./deploy.sh"
	timeout := 120
	environment := map[string]string{"TOKEN": "value"}
	original := &PartialRoleConfigs{Deployer: &PartialDeployerRoleConfig{
		Enabled: &enabled, Command: &command, TimeoutSeconds: &timeout, Environment: &environment,
	}}

	cloned := clonePartialRoleConfigs(original)

	if cloned == nil || cloned.Deployer == nil {
		t.Fatal("clone dropped roles.deployer")
	}
	got := cloned.Deployer
	if got.Enabled == nil || !*got.Enabled || got.Command == nil || *got.Command != command {
		t.Fatalf("cloned deployer = %+v", got)
	}
	if got.TimeoutSeconds == nil || *got.TimeoutSeconds != timeout {
		t.Fatalf("cloned timeout = %v", got.TimeoutSeconds)
	}
	if got.Environment == nil || (*got.Environment)["TOKEN"] != "value" {
		t.Fatalf("cloned environment = %v", got.Environment)
	}

	// A shallow copy would let a later edit of one layer rewrite another. Compare
	// against a value captured before the mutation, not against the variable the
	// original points at.
	const wantCommand = "./deploy.sh"
	*original.Deployer.Command = "rm -rf /"
	environment["TOKEN"] = "rewritten"
	if *cloned.Deployer.Command != wantCommand {
		t.Fatal("clone aliases the original command pointer")
	}
	if (*cloned.Deployer.Environment)["TOKEN"] != "value" {
		t.Fatal("clone aliases the original environment map")
	}
}

func TestMergeDeployerRoleConfig(t *testing.T) {
	t.Parallel()
	role := DeployerRoleConfig{Command: "make deploy", TimeoutSeconds: 60}
	enabled := true
	command := "  ./deploy.sh  "

	MergeDeployerRoleConfig(&role, PartialDeployerRoleConfig{Enabled: &enabled, Command: &command})

	if !role.Enabled || role.Command != "./deploy.sh" {
		t.Fatalf("merged = %+v, want the command trimmed and enabled set", role)
	}
	if role.TimeoutSeconds != 60 {
		t.Fatalf("merged timeout = %d, want the unset field preserved", role.TimeoutSeconds)
	}
}
