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

func TestForgeCredentialReadinessIgnoresNonGitHubConfigs(t *testing.T) {
	for _, key := range config.GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test"}},
		Projects:  []config.ProjectRefConfig{{ID: "forgejo", Provider: "forgejo", Repo: "acme/forgejo", RepoPath: "/tmp/forgejo"}},
	}

	if readiness := ForgeCredentialReadinessFor(cfg); readiness.GitHubProjects || readiness.Degraded() {
		t.Fatalf("ForgeCredentialReadinessFor() = %#v, want no GitHub credential requirement", readiness)
	}
	if readiness := ForgeCredentialReadinessFor(config.Config{}); readiness.Degraded() {
		t.Fatal("Degraded() = true for a project-less config, want false")
	}
}
