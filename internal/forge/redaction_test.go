package forge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
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

// The snapshot is returned as a detached config so the test observes the
// production boundary directly. A caller that stops using the helper, or
// reintroduces an in-place clear beside it, has to fail here.
func TestTrustedReviewSnapshotWithholdsDeployCredentials(t *testing.T) {
	t.Parallel()
	source := configWithProjectDeploySecret()

	snapshot := trustedReviewConfigSnapshot(source)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal(snapshot) error = %v", err)
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

	_ = trustedReviewConfigSnapshot(source)

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

	firstSnapshot := trustedReviewConfigSnapshot(source)
	secondSnapshot := trustedReviewConfigSnapshot(source)
	first, err := json.Marshal(firstSnapshot)
	if err != nil {
		t.Fatalf("first snapshot marshal error = %v", err)
	}
	second, err := json.Marshal(secondSnapshot)
	if err != nil {
		t.Fatalf("second snapshot marshal error = %v", err)
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

	snapshot := trustedReviewConfigSnapshot(source)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal(snapshot) error = %v", err)
	}

	for _, secret := range []string{token, "agent-secret-value", "daemon-secret-value", deploySecret} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the snapshot carries %q:\n%s", secret, encoded)
		}
	}
}
