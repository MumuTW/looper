package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/worker"
)

func TestPlannerGitHubAdapterForgejoCreatePullRequestAndLabels(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var authHeader string
	var createdBody map[string]any
	var labelBody map[string][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create PR body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 101, "html_url": serverURL(r)+"/acme/looper/pulls/101", "head": map[string]any{"ref": "feature", "sha": "abc"}, "base": map[string]any{"ref": "main", "sha": "def"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/101/labels":
			if err := json.NewDecoder(r.Body).Decode(&labelBody); err != nil {
				t.Fatalf("decode labels body: %v", err)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "name": "looper:spec-reviewing"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := plannerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	created, err := adapter.CreatePullRequest(context.Background(), planner.CreatePullRequestInput{Repo: "acme/looper", HeadBranch: "feature", BaseBranch: "main", Title: "Spec: add forgejo", Body: "Body", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if created.Number != 101 {
		t.Fatalf("created = %#v, want PR 101", created)
	}
	if err := adapter.AddPullRequestLabels(context.Background(), planner.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 101, Labels: []string{"looper:spec-reviewing"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestLabels() error = %v", err)
	}
	if authHeader != "token secret" {
		t.Fatalf("Authorization = %q, want Forgejo token auth", authHeader)
	}
	if createdBody["head"] != "feature" || createdBody["base"] != "main" {
		t.Fatalf("create body = %#v, want feature->main", createdBody)
	}
	if len(labelBody["labels"]) != 1 || labelBody["labels"][0] != "looper:spec-reviewing" {
		t.Fatalf("label body = %#v, want reviewing label", labelBody)
	}
	if body, _ := createdBody["body"].(string); !strings.Contains(body, "Body") {
		t.Fatalf("create PR body = %q, want stamped body content", body)
	}
}

func TestWorkerGitHubAdapterForgejoCreatePullRequest(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var createdBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/acme/looper/pulls" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
			t.Fatalf("decode create PR body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 201, "html_url": serverURL(r) + "/acme/looper/pulls/201", "head": map[string]any{"ref": "worker-branch", "sha": "abc"}, "base": map[string]any{"ref": "main", "sha": "def"}})
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: server.URL, TokenEnv: stringPtr("FORGEJO_TOKEN")}},
		Projects:  []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}},
	}
	adapter := workerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	created, err := adapter.CreatePullRequest(context.Background(), worker.CreatePullRequestInput{Repo: "acme/looper", HeadBranch: "worker-branch", BaseBranch: "main", Title: "Implement worker", Body: "Body", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if created.Number != 201 {
		t.Fatalf("created = %#v, want PR 201", created)
	}
	if createdBody["head"] != "worker-branch" || createdBody["base"] != "main" {
		t.Fatalf("create body = %#v, want worker-branch->main", createdBody)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
