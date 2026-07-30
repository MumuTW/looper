package config

import "testing"

func TestDaemonGitHubCredentialEnvReadsAgentEnv(t *testing.T) {
	for _, key := range GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}
	cfg := Config{Agent: AgentConfig{Env: map[string]string{"GH_TOKEN": "config-only-token", "ANTHROPIC_API_KEY": "unrelated"}}}

	env := DaemonGitHubCredentialEnv(cfg)
	if env["GH_TOKEN"] != "config-only-token" {
		t.Fatalf("DaemonGitHubCredentialEnv()[GH_TOKEN] = %q, want the configured agent.env value", env["GH_TOKEN"])
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("DaemonGitHubCredentialEnv() leaked a non-GitHub agent.env secret into daemon gh children")
	}
	if !HasDaemonGitHubCredential(cfg) {
		t.Fatal("HasDaemonGitHubCredential() = false, want true for a configured GH_TOKEN")
	}
}

func TestDaemonGitHubCredentialEnvPrefersProcessEnv(t *testing.T) {
	for _, key := range GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("GH_TOKEN", "process-token")
	cfg := Config{Agent: AgentConfig{Env: map[string]string{"GH_TOKEN": "config-token"}}}

	if got := DaemonGitHubCredentialEnv(cfg)["GH_TOKEN"]; got != "process-token" {
		t.Fatalf("DaemonGitHubCredentialEnv()[GH_TOKEN] = %q, want the daemon process value to win", got)
	}
}

func TestDaemonGitHubCredentialEnvIsNilWithoutAnyToken(t *testing.T) {
	for _, key := range GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}
	cfg := Config{Agent: AgentConfig{Env: map[string]string{"GH_TOKEN": "   "}}}

	if env := DaemonGitHubCredentialEnv(cfg); env != nil {
		t.Fatalf("DaemonGitHubCredentialEnv() = %v, want nil so the child inherits unchanged", env)
	}
	if HasDaemonGitHubCredential(cfg) {
		t.Fatal("HasDaemonGitHubCredential() = true, want false when no token resolves")
	}
}
