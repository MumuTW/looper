package github

import (
	"os"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// authEnvKeys are the variables `gh` reads a credential from, in the order gh
// itself prefers them.
var authEnvKeys = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// AuthEnv resolves the GitHub credential the daemon's own `gh` invocations must
// carry, as environment overrides for the child process.
//
// This exists because the daemon cannot rely on ambient credentials. `gh` stores
// its login in the OS keyring by default, and a daemon started by launchd or
// systemd has no login session to read that keyring with — so an unconfigured
// daemon's `gh` calls go out anonymous and share GitHub's per-IP budget of 60
// requests an hour, failing with a 403 that names an IP address rather than a
// user. Config is the only credential source a daemon can depend on.
//
// The daemon's own environment wins over configuration: an operator who exports
// a token for the service is making a deliberate, more specific choice than the
// config file's default.
func AuthEnv(cfg config.Config) map[string]string {
	return AuthEnvFrom(cfg, os.Getenv)
}

// AuthEnvFrom is AuthEnv against an explicit environment reader. Callers whose
// output must be reproducible — frozen response contracts, for one — cannot let
// an ambient token on the build machine change the answer.
func AuthEnvFrom(cfg config.Config, getenv func(string) string) map[string]string {
	if getenv == nil {
		getenv = os.Getenv
	}
	env := map[string]string{}
	for _, key := range authEnvKeys {
		if value := strings.TrimSpace(cfg.Agent.Env[key]); value != "" {
			env[key] = value
		}
	}
	for _, key := range authEnvKeys {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			env[key] = value
		}
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

// HasAmbientAuth reports whether the current process environment already carries
// a GitHub credential. Distinguishing this from AuthEnv's result is what lets a
// startup check say "configuration supplied the token" rather than guessing.
func HasAmbientAuth() bool {
	return HasAmbientAuthFrom(os.Getenv)
}

// HasAmbientAuthFrom is HasAmbientAuth against an explicit environment reader.
func HasAmbientAuthFrom(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, key := range authEnvKeys {
		if strings.TrimSpace(getenv(key)) != "" {
			return true
		}
	}
	return false
}

// MissingAuthWarning describes what an operator must fix when neither config nor
// environment yields a credential, or returns "" when one is available.
//
// The daemon cannot prove the keyring is unreadable without spending a request,
// so this reports the condition it can prove — no token from either source —
// and names the consequence rather than asserting a diagnosis.
func MissingAuthWarning(cfg config.Config) string {
	return MissingAuthWarningFrom(cfg, os.Getenv)
}

// MissingAuthWarningFrom is MissingAuthWarning against an explicit environment
// reader.
func MissingAuthWarningFrom(cfg config.Config, getenv func(string) string) string {
	if len(AuthEnvFrom(cfg, getenv)) > 0 {
		return ""
	}
	return "No GitHub token resolved from agent.env or the daemon environment. " +
		"gh may still find a credential in the OS keyring when the daemon has a login session, " +
		"but a service-managed daemon usually does not: its gh calls then go out unauthenticated and " +
		"share GitHub's limit of 60 requests per hour per IP address. " +
		"Set GH_TOKEN under [agent.env] in the config file, or in the daemon's service environment."
}
