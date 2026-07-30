package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestRuntimeGitHubHealthProbesHostsConcurrentlyWithoutMixingResults(t *testing.T) {
	for _, key := range config.GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}
	cfg := githubProjectConfig(map[string]string{"GH_ENTERPRISE_TOKEN": "token"})
	cfg.Projects = []config.ProjectRefConfig{{ID: "a", Repo: "https://a.example/acme/a"}, {ID: "b", Repo: "https://b.example/acme/b"}}
	entered := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	g := githubinfra.New(githubinfra.Options{Env: config.DaemonGitHubCredentialEnv(cfg), RequireCredential: true, GHRun: func(_ context.Context, o shell.Options) (shell.Result, error) {
		host := o.Args[len(o.Args)-1]
		entered <- host
		once.Do(func() { go func() { <-entered; <-entered; close(release) }() })
		<-release
		return shell.Result{Stdout: "HTTP/2 200\nX-Ratelimit-Limit: 1\nX-Ratelimit-Remaining: 1\nX-Ratelimit-Reset: 1\n\n" + host + "\n"}, nil
	}})
	r := New(Options{Config: cfg})
	r.githubGateway = g
	result := make(chan GitHubHealth, 1)
	go func() { result <- r.GitHubHealth(context.Background()) }()
	var h GitHubHealth
	select {
	case h = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("GitHubHealth did not probe hosts concurrently")
	}
	if len(h.Hosts) != 2 ||
		h.Hosts[0].Hostname != "a.example" || h.Hosts[0].Login != "a.example" ||
		h.Hosts[1].Hostname != "b.example" || h.Hosts[1].Login != "b.example" {
		t.Fatalf("hosts=%#v", h.Hosts)
	}
}

func TestRuntimeGitHubHealthUsesDaemonGatewayForConfiguredHost(t *testing.T) {
	for _, key := range config.GitHubTokenEnvKeys {
		t.Setenv(key, "")
	}
	cfg := githubProjectConfig(map[string]string{"GH_ENTERPRISE_TOKEN": "configured-token"})
	cfg.Projects[0].Repo = "https://github.example.test/acme/looper"
	gateway := githubinfra.New(githubinfra.Options{
		Env:               config.DaemonGitHubCredentialEnv(cfg),
		RequireCredential: true,
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			if got := strings.Join(options.Args, " "); !strings.HasSuffix(got, "--hostname github.example.test") {
				t.Fatalf("gh args = %q, want project hostname", got)
			}
			return shell.Result{Stdout: "HTTP/2.0 200 OK\nX-Ratelimit-Limit: 5000\nX-Ratelimit-Remaining: 4100\nX-Ratelimit-Reset: 1785414807\n\noperator\n"}, nil
		},
	})
	runtime := New(Options{Config: cfg})
	runtime.githubGateway = gateway

	health := runtime.GitHubHealth(context.Background())
	if health.Credential.Degraded() || health.AuthenticationDegraded() {
		t.Fatalf("GitHubHealth() = %#v, want healthy configured authentication", health)
	}
	if len(health.Hosts) != 1 || health.Hosts[0].Hostname != "github.example.test" || health.Hosts[0].Login != "operator" || health.Hosts[0].CoreRateRemaining != 4100 {
		t.Fatalf("GitHubHealth().Hosts = %#v", health.Hosts)
	}
}
