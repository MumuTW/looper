package e2e

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	envForgejoSandboxEnabled = "LOOPER_E2E_FORGEJO"
	envForgejoBaseURL        = "LOOPER_E2E_FORGEJO_BASE_URL"
	envForgejoSandboxRepo    = "LOOPER_E2E_FORGEJO_SANDBOX_REPO"
	envForgejoToken          = "LOOPER_E2E_FORGEJO_TOKEN"
)

type forgejoSandboxConfig struct {
	BaseURL      string
	Repo         string
	Owner        string
	Name         string
	Token        string
	RunID        string
	TitlePrefix  string
	BranchPrefix string
	CloneURL     string
	CmdEnv       []string
}

func TestForgejoSandboxWorkerCreatesPullRequest(t *testing.T) {
	requireForgejoSandboxConfig(t)
	t.Skip("Forgejo live worker PR mirror is inventoried for Step 1; behavior is implemented in Step 3")
}

func TestForgejoSandboxFixerResolvesReviewThread(t *testing.T) {
	requireForgejoSandboxConfig(t)
	t.Skip("Forgejo fixer review-thread resolution is unsupported by the current MVP capability set")
}

func TestForgejoSandboxNoDiffPathsDoNotOpenOrResolve(t *testing.T) {
	requireForgejoSandboxConfig(t)
	t.Run("worker-no-diff-no-pr", func(t *testing.T) {
		t.Skip("Forgejo live worker no-diff mirror is inventoried for Step 1; behavior is implemented in Step 3")
	})
	t.Run("fixer-no-new-commit-keeps-thread-unresolved", func(t *testing.T) {
		t.Skip("Forgejo fixer review-thread resolution is unsupported by the current MVP capability set")
	})
}

func TestForgejoSandboxDependencyGateScenarios(t *testing.T) {
	requireForgejoSandboxConfig(t)
	for _, name := range []string{
		"looperd startup validation succeeds against real dependency API",
		"human gated blocked_by fails then releases after completion",
		"Forgejo rejects blocked_by cycle creation",
		"not planned blocker returns dependent to retriage without cycle comment",
	} {
		t.Run(name, func(t *testing.T) {
			t.Skip("Forgejo Coordinator/dependency-gate behavior is unsupported by the current MVP capability set")
		})
	}
}

func requireForgejoSandboxConfig(tb testing.TB) forgejoSandboxConfig {
	tb.Helper()
	cfg, enabled, err := parseForgejoSandboxConfig(os.Getenv, os.Environ)
	if !enabled {
		tb.Skipf("set %s=1 to run real Forgejo sandbox E2E", envForgejoSandboxEnabled)
	}
	if err != nil {
		tb.Fatalf("invalid Forgejo sandbox config: %v", err)
	}
	return cfg
}

func parseForgejoSandboxConfig(getenv func(string) string, environ func() []string) (forgejoSandboxConfig, bool, error) {
	if strings.TrimSpace(getenv(envForgejoSandboxEnabled)) != "1" {
		return forgejoSandboxConfig{}, false, nil
	}
	baseURL := strings.TrimSpace(getenv(envForgejoBaseURL))
	if baseURL == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s=1 requires %s", envForgejoSandboxEnabled, envForgejoBaseURL)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s must be an absolute URL, got %q", envForgejoBaseURL, baseURL)
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	baseURL = strings.TrimRight(parsed.String(), "/")
	repo := strings.TrimSpace(getenv(envForgejoSandboxRepo))
	if repo == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s=1 requires %s", envForgejoSandboxEnabled, envForgejoSandboxRepo)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return forgejoSandboxConfig{}, true, fmt.Errorf("invalid %s %q, want owner/repo", envForgejoSandboxRepo, repo)
	}
	token := strings.TrimSpace(getenv(envForgejoToken))
	if token == "" {
		return forgejoSandboxConfig{}, true, fmt.Errorf("%s=1 requires %s", envForgejoSandboxEnabled, envForgejoToken)
	}
	runID := strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	cloneURL, err := forgejoAuthenticatedRemoteURL(baseURL, repo, token)
	if err != nil {
		return forgejoSandboxConfig{}, true, err
	}
	cmdEnv := append(environ(), envForgejoToken+"=" + token, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never", "GIT_ASKPASS=/usr/bin/true")
	return forgejoSandboxConfig{BaseURL: baseURL, Repo: repo, Owner: owner, Name: name, Token: token, RunID: runID, TitlePrefix: "looper-e2e:" + runID, BranchPrefix: "looper-e2e-" + runID, CloneURL: cloneURL, CmdEnv: cmdEnv}, true, nil
}

func forgejoAuthenticatedRemoteURL(baseURL, repo, token string) (string, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/" + strings.TrimPrefix(repo, "/") + ".git")
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("build Forgejo clone URL from %q and %q", baseURL, repo)
	}
	u.User = url.UserPassword("looper-e2e", token)
	return u.String(), nil
}
