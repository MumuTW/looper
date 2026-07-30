package forge

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

const deploySecret = "s3cr3t-deploy-token"

func configWithProjectDeploySecret() config.Config {
	environment := map[string]string{"DEPLOY_TOKEN": deploySecret}
	return config.Config{
		Roles: config.RoleConfigs{Deployer: config.DeployerRoleConfig{
			Environment: map[string]string{"GLOBAL_DEPLOY_TOKEN": deploySecret},
		}},
		Projects: []config.ProjectRefConfig{{
			ID: "looper",
			Roles: &config.PartialRoleConfigs{Deployer: &config.PartialDeployerRoleConfig{
				Environment: &environment,
			}},
		}},
	}
}

// Driven through marshalTrustedReviewConfigSnapshot rather than the redaction
// helper, because the helper being correct proves nothing about whether the
// snapshot path still calls it. A caller that stops using it, or reintroduces an
// in-place clear beside it, has to fail here.
func TestTrustedReviewSnapshotWithholdsDeployCredentials(t *testing.T) {
	t.Parallel()
	source := configWithProjectDeploySecret()

	encoded, err := marshalTrustedReviewConfigSnapshot(source)
	if err != nil {
		t.Fatalf("marshalTrustedReviewConfigSnapshot() error = %v", err)
	}

	if strings.Contains(string(encoded), deploySecret) {
		t.Fatalf("the snapshot carries a deploy credential:\n%s", encoded)
	}
}

// Withholding a secret must not destroy it.
//
// `snapshot := source` copies the slice header, so the snapshot's projects share
// a backing array with the configuration and their Roles pointers are the same
// objects. Clearing through the snapshot therefore erased the operator's real
// deploy credentials, and every deploy afterwards failed for want of values that
// were in the config file all along.
func TestTrustedReviewSnapshotLeavesTheLiveConfigurationIntact(t *testing.T) {
	t.Parallel()
	source := configWithProjectDeploySecret()

	if _, err := marshalTrustedReviewConfigSnapshot(source); err != nil {
		t.Fatalf("marshalTrustedReviewConfigSnapshot() error = %v", err)
	}

	if source.Roles.Deployer.Environment["GLOBAL_DEPLOY_TOKEN"] != deploySecret {
		t.Fatal("minting a snapshot erased the global deploy credentials")
	}
	projectEnv := source.Projects[0].Roles.Deployer.Environment
	if projectEnv == nil {
		t.Fatal("minting a snapshot erased the project deploy credentials")
	}
	if (*projectEnv)["DEPLOY_TOKEN"] != deploySecret {
		t.Fatalf("minting a snapshot altered the project deploy credentials: %v", *projectEnv)
	}
}

// Minting twice must be as safe as minting once: a first call that mutated the
// source would leave the second describing a different configuration.
func TestTrustedReviewSnapshotIsRepeatable(t *testing.T) {
	t.Parallel()
	source := configWithProjectDeploySecret()

	first, err := marshalTrustedReviewConfigSnapshot(source)
	if err != nil {
		t.Fatalf("first marshal error = %v", err)
	}
	second, err := marshalTrustedReviewConfigSnapshot(source)
	if err != nil {
		t.Fatalf("second marshal error = %v", err)
	}

	if string(first) != string(second) {
		t.Fatalf("snapshots differ between calls, so the first mutated the source.\nfirst:  %s\nsecond: %s", first, second)
	}
}

// The other secrets this snapshot withholds are covered here too, so a future
// edit to the redaction block cannot quietly drop one of them.
func TestTrustedReviewSnapshotWithholdsEveryProcessSecret(t *testing.T) {
	t.Parallel()
	source := configWithProjectDeploySecret()
	token := "local-token-secret"
	source.Server.LocalToken = &token
	source.Agent.Env = map[string]string{"AGENT_SECRET": "agent-secret-value"}
	source.Daemon.Environment = map[string]string{"DAEMON_SECRET": "daemon-secret-value"}

	encoded, err := marshalTrustedReviewConfigSnapshot(source)
	if err != nil {
		t.Fatalf("marshalTrustedReviewConfigSnapshot() error = %v", err)
	}

	for _, secret := range []string{token, "agent-secret-value", "daemon-secret-value", deploySecret} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the snapshot carries %q:\n%s", secret, encoded)
		}
	}
}
