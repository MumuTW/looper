package forge

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

// Withholding a secret from a snapshot must not destroy it.
//
// `snapshot := source` copies the slice header, so the projects share a backing
// array and their Roles pointers are the same objects. Clearing Environment
// through the snapshot therefore erased the operator's real deploy credentials,
// breaking every later deploy rather than hiding them from one payload.
func TestRedactedProjectsLeavesTheLiveConfigurationIntact(t *testing.T) {
	t.Parallel()
	environment := map[string]string{"DEPLOY_TOKEN": "secret-value"}
	live := []config.ProjectRefConfig{{
		ID: "looper",
		Roles: &config.PartialRoleConfigs{Deployer: &config.PartialDeployerRoleConfig{
			Environment: &environment,
		}},
	}}

	redacted := redactedProjects(live)

	if redacted[0].Roles.Deployer.Environment != nil {
		t.Fatalf("credentials survived redaction: %v", *redacted[0].Roles.Deployer.Environment)
	}
	if live[0].Roles.Deployer.Environment == nil {
		t.Fatal("redaction erased the live deploy credentials")
	}
	if (*live[0].Roles.Deployer.Environment)["DEPLOY_TOKEN"] != "secret-value" {
		t.Fatalf("redaction altered the live values: %v", *live[0].Roles.Deployer.Environment)
	}
}

// The snapshot must not share the Roles object either, or a later redaction of a
// different field would reach back into the configuration the same way.
func TestRedactedProjectsDoesNotShareRoles(t *testing.T) {
	t.Parallel()
	environment := map[string]string{"DEPLOY_TOKEN": "secret-value"}
	live := []config.ProjectRefConfig{{
		ID: "looper",
		Roles: &config.PartialRoleConfigs{Deployer: &config.PartialDeployerRoleConfig{
			Environment: &environment,
		}},
	}}

	redacted := redactedProjects(live)

	if redacted[0].Roles == live[0].Roles {
		t.Fatal("the snapshot shares the live Roles pointer")
	}
	if redacted[0].Roles.Deployer == live[0].Roles.Deployer {
		t.Fatal("the snapshot shares the live Deployer pointer")
	}
}

// A project with nothing to redact is passed through unchanged rather than
// copied into a subtly different shape.
func TestRedactedProjectsLeavesUnaffectedProjectsAlone(t *testing.T) {
	t.Parallel()
	live := []config.ProjectRefConfig{
		{ID: "no-roles"},
		{ID: "no-deployer", Roles: &config.PartialRoleConfigs{}},
	}

	redacted := redactedProjects(live)

	if len(redacted) != 2 {
		t.Fatalf("redacted %d projects, want 2", len(redacted))
	}
	if redacted[0].ID != "no-roles" || redacted[1].ID != "no-deployer" {
		t.Fatalf("redaction reordered or renamed projects: %+v", redacted)
	}
	if redacted[1].Roles != live[1].Roles {
		t.Fatal("a project with nothing to redact was copied needlessly")
	}
}
