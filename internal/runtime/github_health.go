package runtime

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

// ForgeAuthenticationDegradedReason means a credential was configured but the
// daemon's own gh environment could not prove an authenticated identity.
const ForgeAuthenticationDegradedReason = "forge_auth_unhealthy"

// GitHubHealth is the operator-facing view of the exact credentials used by
// daemon-owned gh children. Host snapshots are cached by the gateway and carry
// CheckedAt so the rate budget is never presented as timeless live state.
type GitHubHealth struct {
	Credential ForgeCredentialReadiness `json:"credential"`
	Hosts      []githubinfra.AuthHealth `json:"hosts"`
}

// AuthenticationDegraded distinguishes an absent credential (reported by
// ForgeCredentialReadiness) from one that is present but invalid or unprobeable.
func (h GitHubHealth) AuthenticationDegraded() bool {
	if !h.Credential.Resolved {
		return false
	}
	for _, host := range h.Hosts {
		if !host.Authenticated {
			return true
		}
	}
	return false
}

// GitHubHealth reports actual identity/rate state using the same gateway and
// child environment as scheduler, reviewer, fixer, and worker forge calls.
func (r *Runtime) GitHubHealth(ctx context.Context) GitHubHealth {
	cfg := r.Config()
	out := GitHubHealth{
		Credential: ForgeCredentialReadinessFor(cfg),
		Hosts:      []githubinfra.AuthHealth{},
	}
	hostnames := githubProjectHostnames(cfg)
	if len(hostnames) == 0 {
		return out
	}

	r.mu.RLock()
	gateway := r.githubGateway
	r.mu.RUnlock()
	out.Hosts = make([]githubinfra.AuthHealth, len(hostnames))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for index, hostname := range hostnames {
		wg.Add(1)
		go func(index int, hostname string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out.Hosts[index] = githubinfra.AuthHealth{Hostname: hostname, Error: ctx.Err().Error()}
				return
			}
			defer func() { <-sem }()
			if gateway == nil {
				out.Hosts[index] = githubinfra.AuthHealth{Hostname: hostname, Error: "GitHub gateway is not configured"}
				return
			}
			out.Hosts[index] = gateway.AuthHealth(ctx, "", hostname)
		}(index, hostname)
	}
	wg.Wait()
	return out
}

func githubProjectHostnames(cfg config.Config) []string {
	hosts := map[string]struct{}{}
	for _, project := range cfg.Projects {
		if config.ResolvedProjectProviderKind(cfg, project) != config.ProviderKindGitHub {
			continue
		}
		hostname := strings.TrimSpace(githubAuthHostname(project.Repo))
		if hostname == "" {
			hostname = "github.com"
		}
		hosts[hostname] = struct{}{}
	}
	result := make([]string, 0, len(hosts))
	for hostname := range hosts {
		result = append(result, hostname)
	}
	sort.Strings(result)
	return result
}
