package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/fixer"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/reviewer"
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

func TestFixerReviewThreadCommandsUseConfiguredProviderHostname(t *testing.T) {
	root := t.TempDir()
	ghesPath := filepath.Join(root, "ghes")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: "https://github.example.com"}},
		Projects:  []config.ProjectRefConfig{{ID: "enterprise", Provider: "github-enterprise", Repo: "acme/enterprise", RepoPath: ghesPath}},
	}
	var calls []string
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			args := strings.Join(options.Args, " ")
			calls = append(calls, args)
			switch {
			case strings.Contains(args, "addPullRequestReviewThreadReply"):
				return shell.Result{Stdout: `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"comment-1"}}}}`}, nil
			case strings.Contains(args, "resolveReviewThread"):
				return shell.Result{Stdout: `{"data":{"resolveReviewThread":{"thread":{"id":"thread-1","isResolved":true}}}}`}, nil
			case strings.Contains(args, "comments(first: 100, after: $after)"):
				return shell.Result{Stdout: `{"data":{"node":{"comments":{"nodes":[{"id":"comment-1","body":"fix this"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`}, nil
			case strings.Contains(args, "query($threadId: ID!)"):
				return shell.Result{Stdout: `{"data":{"node":{"id":"thread-1","isResolved":false}}}`}, nil
			default:
				return shell.Result{}, fmt.Errorf("unexpected gh args: %s", args)
			}
		},
	})
	adapter := fixerGitHubAdapter{config: &cfg, gateway: gateway}
	if _, err := adapter.ViewReviewThread(context.Background(), fixer.ViewReviewThreadInput{Repo: "acme/not-authoritative", ThreadID: "thread-1", CWD: ghesPath}); err != nil {
		t.Fatalf("ViewReviewThread() error = %v", err)
	}
	if err := adapter.AddReviewThreadReply(context.Background(), fixer.AddReviewThreadReplyInput{Repo: "acme/not-authoritative", ThreadID: "thread-1", Body: "fixed", CWD: ghesPath}); err != nil {
		t.Fatalf("AddReviewThreadReply() error = %v", err)
	}
	if err := adapter.ResolveReviewThread(context.Background(), fixer.ResolveReviewThreadInput{Repo: "acme/not-authoritative", ThreadID: "thread-1", CWD: ghesPath}); err != nil {
		t.Fatalf("ResolveReviewThread() error = %v", err)
	}

	if len(calls) != 5 {
		t.Fatalf("gh calls = %#v, want read, comments, reply, read, resolve", calls)
	}
	for _, call := range calls {
		if !strings.HasSuffix(call, "--hostname github.example.com") {
			t.Fatalf("gh call = %q, want configured GHES hostname", call)
		}
	}
}

func TestReviewerAndGatekeeperReviewThreadCommandsUseConfiguredProviderHostname(t *testing.T) {
	ghesPath := filepath.Join(t.TempDir(), "ghes")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: "https://github.example.com"}},
		Projects:  []config.ProjectRefConfig{{ID: "enterprise", Provider: "github-enterprise", Repo: "acme/enterprise", RepoPath: ghesPath}},
	}
	var calls []string
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			args := strings.Join(options.Args, " ")
			calls = append(calls, args)
			switch {
			case strings.Contains(args, "reviewThreads(first: $limit, after: $after)"):
				return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
			case strings.Contains(args, "addPullRequestReviewThreadReply"):
				return shell.Result{Stdout: `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"comment-1"}}}}`}, nil
			case strings.Contains(args, "resolveReviewThread"):
				return shell.Result{Stdout: `{"data":{"resolveReviewThread":{"thread":{"id":"thread-1","isResolved":true}}}}`}, nil
			case strings.Contains(args, "query($threadId: ID!)"):
				return shell.Result{Stdout: `{"data":{"node":{"id":"thread-1","isResolved":false}}}`}, nil
			default:
				return shell.Result{}, fmt.Errorf("unexpected gh args: %s", args)
			}
		},
	})
	reviewerAdapter := reviewerGitHubAdapter{config: &cfg, gateway: gateway}
	if _, err := reviewerAdapter.ListReviewThreads(context.Background(), reviewer.ListReviewThreadsInput{Repo: "acme/not-authoritative", PRNumber: 42, CWD: ghesPath, Limit: 10}); err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if err := reviewerAdapter.AddReviewThreadReply(context.Background(), reviewer.AddReviewThreadReplyInput{Repo: "acme/not-authoritative", ThreadID: "thread-1", Body: "fixed", CWD: ghesPath}); err != nil {
		t.Fatalf("AddReviewThreadReply() error = %v", err)
	}
	if err := reviewerAdapter.ResolveReviewThread(context.Background(), reviewer.ResolveReviewThreadInput{Repo: "acme/not-authoritative", ThreadID: "thread-1", CWD: ghesPath}); err != nil {
		t.Fatalf("ResolveReviewThread() error = %v", err)
	}
	gatekeeperAdapter := gatekeeperGitHubAdapter{Gateway: gateway, config: &cfg}
	if _, err := gatekeeperAdapter.ListReviewThreads(context.Background(), githubinfra.ListReviewThreadsInput{Repo: "acme/not-authoritative", PRNumber: 42, CWD: ghesPath, Limit: 10}); err != nil {
		t.Fatalf("gatekeeper ListReviewThreads() error = %v", err)
	}

	if len(calls) != 5 {
		t.Fatalf("gh calls = %#v, want reviewer list/reply/read/resolve and gatekeeper list", calls)
	}
	for _, call := range calls {
		if !strings.HasSuffix(call, "--hostname github.example.com") {
			t.Fatalf("gh call = %q, want configured GHES hostname", call)
		}
	}
}

func TestReviewThreadCommandsTreatGitHubAPIHostnameAsPublic(t *testing.T) {
	githubPath := filepath.Join(t.TempDir(), "github")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "github-cloud", Kind: config.ProviderKindGitHub, BaseURL: "https://api.github.com"}},
		Projects:  []config.ProjectRefConfig{{ID: "cloud", Provider: "github-cloud", Repo: "acme/cloud", RepoPath: githubPath}},
	}
	var calls []string
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			args := strings.Join(options.Args, " ")
			calls = append(calls, args)
			if !strings.Contains(args, "reviewThreads(first: $limit, after: $after)") {
				return shell.Result{}, fmt.Errorf("unexpected gh args: %s", args)
			}
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
		},
	})
	adapter := reviewerGitHubAdapter{config: &cfg, gateway: gateway}
	if _, err := adapter.ListReviewThreads(context.Background(), reviewer.ListReviewThreadsInput{Repo: "acme/not-authoritative", PRNumber: 42, CWD: githubPath, Limit: 10}); err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if len(calls) != 1 || strings.Contains(calls[0], "--hostname") {
		t.Fatalf("gh calls = %#v, want public GitHub command without hostname selector", calls)
	}
}

func TestReviewerAndFixerViewPullRequestQualifyEmbeddedReviewThreadFetches(t *testing.T) {
	ghesPath := filepath.Join(t.TempDir(), "ghes")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: "https://github.example.com"}},
		Projects:  []config.ProjectRefConfig{{ID: "enterprise", Provider: "github-enterprise", Repo: "acme/enterprise", RepoPath: ghesPath}},
	}
	var calls []string
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			args := strings.Join(options.Args, " ")
			calls = append(calls, args)
			switch {
			case strings.HasPrefix(args, "pr view "):
				return shell.Result{Stdout: `{"number":42}`}, nil
			case strings.Contains(args, "reviewThreads(first: 100, after: $after)"):
				return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread-1","isResolved":false,"path":"file.go","line":1,"comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"comment-cursor"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
			case strings.Contains(args, "comments(first: 100, after: $after)"):
				return shell.Result{Stdout: `{"data":{"node":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`}, nil
			case strings.HasPrefix(args, "api --paginate repos/acme/enterprise/issues/42/comments "):
				return shell.Result{Stdout: "[]"}, nil
			default:
				return shell.Result{}, fmt.Errorf("unexpected gh args: %s", args)
			}
		},
	})

	reviewerAdapter := reviewerGitHubAdapter{config: &cfg, gateway: gateway}
	if _, err := reviewerAdapter.ViewPullRequest(context.Background(), reviewer.ViewPullRequestInput{Repo: "acme/not-authoritative", PRNumber: 42, CWD: ghesPath}); err != nil {
		t.Fatalf("reviewer ViewPullRequest() error = %v", err)
	}
	fixerAdapter := fixerGitHubAdapter{config: &cfg, gateway: gateway}
	if _, err := fixerAdapter.ViewPullRequest(context.Background(), fixer.ViewPullRequestInput{Repo: "acme/not-authoritative", PRNumber: 42, CWD: ghesPath}); err != nil {
		t.Fatalf("fixer ViewPullRequest() error = %v", err)
	}

	if len(calls) != 8 {
		t.Fatalf("gh calls = %#v, want two complete pull-request reads", calls)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "pr view ") {
			if !strings.Contains(call, "--repo github.example.com/acme/enterprise") {
				t.Fatalf("gh PR view = %q, want configured GHES repository", call)
			}
			continue
		}
		if !strings.Contains(call, "--hostname github.example.com") {
			t.Fatalf("gh embedded review-thread call = %q, want configured GHES hostname", call)
		}
	}
}

func TestReviewerSnapshotQualifiesGHESReviewThreadFetches(t *testing.T) {
	ghesPath := filepath.Join(t.TempDir(), "ghes")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: "https://github.example.com"}},
		Projects:  []config.ProjectRefConfig{{ID: "enterprise", Provider: "github-enterprise", Repo: "acme/enterprise", RepoPath: ghesPath}},
	}
	var calls []string
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			args := strings.Join(options.Args, " ")
			calls = append(calls, args)
			switch {
			case strings.HasPrefix(args, "pr view 42 --repo github.example.com/acme/enterprise --json "):
				return shell.Result{Stdout: `{"number":42}`}, nil
			case strings.Contains(args, "reviewThreads(first: 100, after: $after)"):
				return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
			case strings.HasPrefix(args, "api --paginate repos/acme/enterprise/issues/42/comments "):
				return shell.Result{Stdout: "[]"}, nil
			case strings.HasPrefix(args, "pr diff 42 --repo github.example.com/acme/enterprise"):
				return shell.Result{Stdout: "diff --git a/a.go b/a.go\n"}, nil
			default:
				return shell.Result{}, fmt.Errorf("unexpected gh args: %s", args)
			}
		},
	})

	adapter := reviewerGitHubAdapter{config: &cfg, gateway: gateway}
	if _, err := adapter.CapturePullRequestSnapshot(context.Background(), reviewer.CapturePullRequestSnapshotInput{ProjectID: "enterprise", Repo: "acme/not-authoritative", PRNumber: 42, CWD: ghesPath}); err != nil {
		t.Fatalf("CapturePullRequestSnapshot() error = %v", err)
	}

	if len(calls) != 4 {
		t.Fatalf("gh calls = %#v, want PR, review-thread, comment, and diff commands", calls)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "pr view ") || strings.HasPrefix(call, "pr diff ") {
			if !strings.Contains(call, "--repo github.example.com/acme/enterprise") {
				t.Fatalf("gh pull-request command = %q, want configured GHES repository", call)
			}
			continue
		}
		if !strings.HasSuffix(call, "--hostname github.example.com") {
			t.Fatalf("gh nested fetch = %q, want configured GHES hostname", call)
		}
	}
}

func TestReviewThreadCommandsPreserveConfiguredGHESPort(t *testing.T) {
	ghesPath := filepath.Join(t.TempDir(), "ghes")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "github-enterprise", Kind: config.ProviderKindGitHub, BaseURL: "https://github.example.com:8443"}},
		Projects:  []config.ProjectRefConfig{{ID: "enterprise", Provider: "github-enterprise", Repo: "acme/enterprise", RepoPath: ghesPath}},
	}
	var calls []string
	gateway := githubinfra.New(githubinfra.Options{
		GHPath: "gh",
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			args := strings.Join(options.Args, " ")
			calls = append(calls, args)
			if !strings.Contains(args, "reviewThreads(first: $limit, after: $after)") {
				return shell.Result{}, fmt.Errorf("unexpected gh args: %s", args)
			}
			return shell.Result{Stdout: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`}, nil
		},
	})

	adapter := reviewerGitHubAdapter{config: &cfg, gateway: gateway}
	if _, err := adapter.ListReviewThreads(context.Background(), reviewer.ListReviewThreadsInput{Repo: "acme/not-authoritative", PRNumber: 42, CWD: ghesPath, Limit: 10}); err != nil {
		t.Fatalf("ListReviewThreads() error = %v", err)
	}
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "--hostname github.example.com:8443") {
		t.Fatalf("gh calls = %#v, want configured GHES authority including port", calls)
	}
}
