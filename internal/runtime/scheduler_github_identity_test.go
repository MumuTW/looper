package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/shell"
)

func TestCodingRoleIdentityLookupsUseProjectProviderHostname(t *testing.T) {
	root := t.TempDir()
	githubPath := filepath.Join(root, "github")
	ghesPath := filepath.Join(root, "ghes")
	githubURL := "https://github.com"
	ghesURL := "https://github.example.com"
	cfg := config.Config{
		Providers: []config.ProviderConfig{
			{ID: "github-cloud", Kind: config.ProviderKindGitHub, BaseURL: githubURL},
			{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: ghesURL},
		},
		Projects: []config.ProjectRefConfig{
			{ID: "cloud", Provider: "github-cloud", Repo: "acme/cloud", RepoPath: githubPath},
			{ID: "enterprise", Provider: "github-enterprise", RepoPath: ghesPath},
		},
	}

	var calls []string
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			args := strings.Join(options.Args, " ")
			calls = append(calls, args)
			switch args {
			case "api user --jq .login --hostname github.com":
				return shell.Result{Stdout: "cloud-bot\n"}, nil
			case "api user --jq .login --hostname github.example.com":
				return shell.Result{Stdout: "enterprise-bot\n"}, nil
			default:
				return shell.Result{}, fmt.Errorf("unexpected gh args: %s", args)
			}
		},
	})

	roles := []struct {
		name   string
		lookup func(context.Context, string, string) (string, error)
	}{
		{name: "planner", lookup: (plannerGitHubAdapter{config: &cfg, gateway: gateway}).GetCurrentUserLogin},
		{name: "worker", lookup: (workerGitHubAdapter{config: &cfg, gateway: gateway}).GetCurrentUserLogin},
		{name: "reviewer", lookup: (reviewerGitHubAdapter{config: &cfg, gateway: gateway}).GetCurrentUserLogin},
		{name: "fixer", lookup: (fixerGitHubAdapter{config: &cfg, gateway: gateway}).GetCurrentUserLogin},
	}

	for _, role := range roles {
		cloudLogin, err := role.lookup(context.Background(), "acme/cloud", githubPath)
		if err != nil || cloudLogin != "cloud-bot" {
			t.Fatalf("%s cloud lookup = %q, %v; want cloud-bot", role.name, cloudLogin, err)
		}
		enterpriseLogin, err := role.lookup(context.Background(), "acme/enterprise", ghesPath)
		if err != nil || enterpriseLogin != "enterprise-bot" {
			t.Fatalf("%s enterprise lookup = %q, %v; want enterprise-bot", role.name, enterpriseLogin, err)
		}
	}

	if len(calls) != len(roles)*2 {
		t.Fatalf("gh calls = %#v, want one provider-scoped lookup per role and project", calls)
	}
	for index, role := range roles {
		cloudCall := calls[index*2]
		enterpriseCall := calls[index*2+1]
		if !strings.HasSuffix(cloudCall, "--hostname github.com") {
			t.Errorf("%s cloud gh call = %q, want explicit github.com hostname", role.name, cloudCall)
		}
		if !strings.HasSuffix(enterpriseCall, "--hostname github.example.com") {
			t.Errorf("%s enterprise gh call = %q, want explicit GHES hostname", role.name, enterpriseCall)
		}
	}
}
