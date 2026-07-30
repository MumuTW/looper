package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func githubProjectConfig(agentEnv map[string]string) config.Config {
	return config.Config{
		Agent:    config.AgentConfig{Env: agentEnv},
		Projects: []config.ProjectRefConfig{{ID: "looper", Repo: "acme/looper", RepoPath: "/tmp/looper"}},
	}
}

func TestForgeCredentialReadinessDegradesWithoutToken(t *testing.T) {
	for _, key := range config.GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}

	readiness := ForgeCredentialReadinessFor(githubProjectConfig(nil))
	if !readiness.GitHubProjects {
		t.Fatal("GitHubProjects = false, want true for a GitHub-bound project")
	}
	if !readiness.Degraded() {
		t.Fatal("Degraded() = false, want true when the daemon would call GitHub anonymously")
	}
	if readiness.Reason == "" {
		t.Fatal("Reason is empty, want an operator-actionable explanation")
	}
}

func TestForgeCredentialReadinessResolvesConfiguredToken(t *testing.T) {
	for _, key := range config.GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}

	readiness := ForgeCredentialReadinessFor(githubProjectConfig(map[string]string{"GH_TOKEN": "config-only-token"}))
	if !readiness.Resolved || readiness.Degraded() {
		t.Fatalf("ForgeCredentialReadinessFor() = %#v, want resolved and not degraded", readiness)
	}
	if readiness.Reason != "" {
		t.Fatalf("Reason = %q, want empty when a credential resolves", readiness.Reason)
	}
}

// A fresh install has no projects yet, so it must not be reported as missing a
// credential it does not need. The companion case — a project bound to a
// non-GitHub provider — is gone with the last non-GitHub provider kind.
func TestForgeCredentialReadinessIgnoresProjectlessConfigs(t *testing.T) {
	for _, key := range config.GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}

	readiness := ForgeCredentialReadinessFor(config.Config{})
	if readiness.GitHubProjects {
		t.Fatalf("GitHubProjects = true for a project-less config, want false")
	}
	if readiness.Degraded() {
		t.Fatal("Degraded() = true for a project-less config, want false")
	}
}
