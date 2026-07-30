package runtime

import (
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
)

// ForgeCredentialDegradedReason is the /status degraded reason emitted when the
// daemon would make anonymous GitHub calls.
const ForgeCredentialDegradedReason = "forge_credential_missing"

// ForgeCredentialReadiness is the operator-facing view of whether
// daemon-internal GitHub calls carry a credential.
//
// The daemon's own `gh` invocations (discovery, PR metadata, review threads)
// run as children of looperd, not of an agent. A detached daemon inherits no
// login-shell token and may not be able to read gh's keyring, so without an
// explicitly propagated credential those calls go out anonymous and die on
// GitHub's per-IP rate limit. That failure looks like a transient forge outage,
// which is why it is surfaced here instead of only in per-run errors.
type ForgeCredentialReadiness struct {
	// GitHubProjects is true when at least one configured Project resolves to a
	// provider that uses GitHub pull requests.
	GitHubProjects bool `json:"githubProjects"`
	// Resolved is true when a GitHub credential is available for the daemon's
	// own gh children.
	Resolved bool `json:"resolved"`
	// Reason explains a missing credential. Empty when Resolved, or when no
	// GitHub project is configured. It never contains a credential value.
	Reason string `json:"reason,omitempty"`
}

// Degraded reports whether the daemon will make anonymous GitHub calls.
func (r ForgeCredentialReadiness) Degraded() bool {
	return r.GitHubProjects && !r.Resolved
}

// ForgeCredentialReadinessFor reports whether cfg supplies a credential for
// daemon-internal forge calls. It performs no network or keyring access.
func ForgeCredentialReadinessFor(cfg config.Config) ForgeCredentialReadiness {
	out := ForgeCredentialReadiness{
		GitHubProjects: configHasGitHubProviderProject(cfg),
		Resolved:       config.HasDaemonGitHubCredential(cfg),
	}
	if out.Degraded() {
		out.Reason = "no GitHub token is configured for daemon-internal forge calls; set agent.env.GH_TOKEN or export GH_TOKEN for looperd"
	}
	return out
}

// configHasGitHubProviderProject reports whether any configured Project is
// bound to a GitHub-pull-request provider. Unlike
// runtimeConfigHasGitHubProjects, an empty project list is not GitHub: a fresh
// install must not be reported as missing a credential it does not yet need.
func configHasGitHubProviderProject(cfg config.Config) bool {
	providers := forge.NewResolver(cfg)
	for _, project := range cfg.Projects {
		if providers.ForProject(project.ID).Capabilities().GitHubPullRequests {
			return true
		}
	}
	return false
}
