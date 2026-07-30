package config

import (
	"os"
	"strings"
)

// GitHubTokenEnvKeys are the environment variables the `gh` CLI accepts as a
// GitHub credential, in gh's own precedence order. GH_TOKEN wins over
// GITHUB_TOKEN, and the enterprise pair applies to GHES hosts.
var GitHubTokenEnvKeys = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// DaemonGitHubCredentialEnv resolves the credential variables that
// daemon-internal `gh` invocations must carry explicitly.
//
// Daemon-internal forge calls are not agent processes. looperd normally runs
// detached from the login session, so its own environment carries no token and
// gh's keyring-backed auth may be unreadable. gh then falls back to anonymous
// requests, which GitHub rate-limits per IP — the failure mode this resolver
// exists to make impossible.
//
// [agent.env] is already the config-only home for GH_TOKEN (see
// trustedReviewChildEnv in internal/runtime), so the daemon reads its own
// credential from the same place instead of introducing a parallel setting.
// This map is handed only to daemon-owned gh children; it never widens what an
// agent process can see, because those values are already in the agent env.
//
// The daemon's process environment wins on collisions so an operator-exported
// token stays authoritative over a stale config value.
func DaemonGitHubCredentialEnv(cfg Config) map[string]string {
	env := map[string]string{}
	for _, key := range GitHubTokenEnvKeys {
		if value := strings.TrimSpace(cfg.Agent.Env[key]); value != "" {
			env[key] = value
		}
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env[key] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// HasDaemonGitHubCredential reports whether any GitHub credential is resolvable
// for daemon-internal forge calls.
func HasDaemonGitHubCredential(cfg Config) bool {
	return len(DaemonGitHubCredentialEnv(cfg)) > 0
}
