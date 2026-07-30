package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
)

// NewGatewayPullRequestLookup must answer from the selected project's forge, not
// always from GitHub. A Forgejo project is read through its Forgejo client — the
// same selection workerGitHubAdapter.ViewPullRequest applies — so a Forgejo PR
// is not silently queried against an unrelated same-slug GitHub repository.

func forgejoPullRequestJSON(number int64, state string) []byte {
	body, _ := json.Marshal(map[string]any{
		"number": number,
		"title":  "PR",
		"body":   "",
		"state":  state,
		"head":   map[string]any{"name": "feature", "sha": "sha"},
		"base":   map[string]any{"name": "main", "sha": "base"},
		"user":   map[string]any{"id": 1, "login": "octo"},
	})
	return body
}

func forgejoLookupConfig(t *testing.T, baseURL, repo, repoPath string) config.Config {
	t.Helper()
	tokenEnv := "LOOPER_FORGEJO_LOOKUP_TOKEN"
	t.Setenv(tokenEnv, "secret")
	return config.Config{
		Providers: []config.ProviderConfig{{
			ID:       "forgejo",
			Kind:     config.ProviderKindForgejo,
			BaseURL:  baseURL,
			Auth:     config.ProviderAuthTokenEnv,
			TokenEnv: &tokenEnv,
		}},
		Projects: []config.ProjectRefConfig{
			{ID: "forgejo", Provider: "forgejo", Repo: repo, RepoPath: repoPath},
		},
	}
}

func TestNewGatewayPullRequestLookupRoutesForgejoProjectToForgejo(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/acme/looper/pulls/77" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(forgejoPullRequestJSON(77, "open"))
	}))
	defer server.Close()

	cfg := forgejoLookupConfig(t, server.URL, "acme/looper", t.TempDir())
	lookup := NewGatewayPullRequestLookup(cfg, time.Now)

	target, err := lookup(context.Background(), "acme/looper", 77, t.TempDir())
	if err != nil {
		t.Fatalf("lookup() error = %v", err)
	}
	if target.Number != 77 || target.Merged {
		t.Fatalf("lookup() = %#v, want number 77 open", target)
	}
	if target.State != "open" {
		t.Fatalf("lookup().State = %q, want open (sourced from Forgejo)", target.State)
	}
	mu.Lock()
	gotPaths := paths
	mu.Unlock()
	if len(gotPaths) != 1 {
		t.Fatalf("forgejo requests = %#v, want one ViewPullRequest against the Forgejo server", gotPaths)
	}
}

func TestNewGatewayPullRequestLookupClassifiesForgejoNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	cfg := forgejoLookupConfig(t, server.URL, "acme/looper", t.TempDir())
	lookup := NewGatewayPullRequestLookup(cfg, time.Now)

	if _, err := lookup(context.Background(), "acme/looper", 77, t.TempDir()); err == nil {
		t.Fatalf("lookup() error = nil, want not-found")
	} else if !IsPullRequestLookupNotFound(err) {
		t.Fatalf("lookup() err = %v, want a classified not-found", err)
	}
}

func TestNewGatewayPullRequestLookupPreservesForgejoOperationalFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	cfg := forgejoLookupConfig(t, server.URL, "acme/looper", t.TempDir())
	lookup := NewGatewayPullRequestLookup(cfg, time.Now)

	if _, err := lookup(context.Background(), "acme/looper", 77, t.TempDir()); err == nil {
		t.Fatalf("lookup() error = nil, want operational failure")
	} else if IsPullRequestLookupNotFound(err) {
		t.Fatalf("lookup() err = %v, classified as not-found; want an operational failure", err)
	}
}
