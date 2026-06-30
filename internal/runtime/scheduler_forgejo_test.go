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
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/planner"
	"github.com/nexu-io/looper/internal/reviewer"
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
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 101, "html_url": serverURL(r) + "/acme/looper/pulls/101", "head": map[string]any{"ref": "feature", "sha": "abc"}, "base": map[string]any{"ref": "main", "sha": "def"}})
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

func TestWorkerGitHubAdapterForgejoCreatePullRequestQueuesReviewerDiscoveryLabel(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var createdBody map[string]any
	var labelBody map[string][]string
	currentLabels := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			if err := json.NewDecoder(r.Body).Decode(&createdBody); err != nil {
				t.Fatalf("decode create PR body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 201, "html_url": serverURL(r) + "/acme/looper/pulls/201", "head": map[string]any{"ref": "worker-branch", "sha": "abc"}, "base": map[string]any{"ref": "main", "sha": "def"}, "labels": currentLabels})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/201/labels":
			if err := json.NewDecoder(r.Body).Decode(&labelBody); err != nil {
				t.Fatalf("decode labels body: %v", err)
			}
			currentLabels = currentLabels[:0]
			for i, label := range labelBody["labels"] {
				currentLabels = append(currentLabels, map[string]any{"id": i + 1, "name": label})
			}
			_ = json.NewEncoder(w).Encode(currentLabels)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 201, "title": "Implement worker", "body": "Body", "state": "open",
				"head":   map[string]any{"ref": "worker-branch", "sha": "abc"},
				"base":   map[string]any{"ref": "main", "sha": "def"},
				"user":   map[string]any{"login": "worker", "id": 1},
				"labels": currentLabels,
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	repoPath := filepath.Join(t.TempDir(), "repo")
	cfg := config.Config{
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{
					Triggers: config.ReviewerRoleTriggersConfig{Labels: []string{"team-review"}},
				},
			},
		},
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
	if err := adapter.AddPullRequestReviewers(context.Background(), worker.PullRequestReviewersInput{Repo: "acme/looper", PRNumber: 201, Reviewers: []string{"reviewer"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestReviewers() error = %v", err)
	}
	if createdBody["head"] != "worker-branch" || createdBody["base"] != "main" {
		t.Fatalf("create body = %#v, want worker-branch->main", createdBody)
	}
	if got := labelBody["labels"]; len(got) != 1 || got[0] != "team-review" {
		t.Fatalf("label body = %#v, want configured reviewer discovery label", labelBody)
	}
	reviewerAdapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	prs, err := reviewerAdapter.ListOpenPullRequests(context.Background(), reviewer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath, Labels: []string{"team-review"}})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 201 {
		t.Fatalf("prs = %#v, want worker-created PR rediscovered by reviewer label", prs)
	}
}

func TestReviewerGitHubAdapterForgejoCommentOnlyFlow(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var listLabels string
	var commentBody map[string]any
	existingMarker := "<!-- looper:review id=reviewer:loop_123:abc123:key head=abc123 outcome=non_blocking -->"
	var removedPaths []string
	var comparePath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			listLabels = r.URL.Query().Get("labels")
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 42, "title": "Review me", "body": "PR body", "state": "open", "draft": true,
				"head":   map[string]any{"ref": "feature/review-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:review"}},
			}, {
				"number": 99, "title": "Skip me", "body": "PR body", "state": "open",
				"head":   map[string]any{"ref": "feature/skip-me", "sha": "def456"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "bob", "id": 2},
				"labels": []map[string]any{{"id": 2, "name": "other"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "Review me", "body": "PR body", "state": "open", "draft": true,
				"head":   map[string]any{"ref": "feature/review-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:review"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42.diff":
			_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id":         77,
				"body":       "Existing review\n\n" + existingMarker,
				"html_url":   serverURL(r) + "/acme/looper/issues/42#issuecomment-77",
				"updated_at": "2026-06-18T00:00:00Z",
				"user":       map[string]any{"login": "reviewer-bot", "id": 7},
			}})
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v1/repos/acme/looper/compare/main...feature%2Freview-me":
			comparePath = r.URL.EscapedPath()
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "ahead_by": 1, "behind_by": 0, "total_commits": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			if err := json.NewDecoder(r.Body).Decode(&commentBody); err != nil {
				t.Fatalf("decode comment body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99, "html_url": serverURL(r) + "/acme/looper/issues/42#comment-99"})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/42/labels/"):
			removedPaths = append(removedPaths, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
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
	adapter := reviewerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}

	prs, err := adapter.ListOpenPullRequests(context.Background(), reviewer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath, Labels: []string{"looper:review"}})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].HeadSHA != "abc123" || !prs[0].IsDraft {
		t.Fatalf("prs = %#v, want Forgejo PR summary", prs)
	}
	detail, err := adapter.ViewPullRequest(context.Background(), reviewer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if !strings.Contains(detail.Diff, "diff --git") {
		t.Fatalf("detail.Diff = %q, want fetched Forgejo diff", detail.Diff)
	}
	if !detail.IsDraft {
		t.Fatalf("detail = %#v, want draft preserved", detail)
	}
	if len(detail.IssueComments) != 1 {
		t.Fatalf("detail.IssueComments = %#v, want existing Forgejo issue comment", detail.IssueComments)
	}
	if body, _ := detail.IssueComments[0]["body"].(string); !strings.Contains(body, existingMarker) {
		t.Fatalf("detail.IssueComments = %#v, want marker-bearing comment body", detail.IssueComments)
	}
	snapshot, err := adapter.CapturePullRequestSnapshot(context.Background(), reviewer.CapturePullRequestSnapshotInput{ProjectID: "project_1", Repo: "acme/looper", PRNumber: 42, CWD: repoPath, CapturedAt: "2026-06-18T00:00:00Z"})
	if err != nil {
		t.Fatalf("CapturePullRequestSnapshot() error = %v", err)
	}
	if snapshot.HeadSHA != "abc123" || snapshot.PayloadJSON == nil || !strings.Contains(*snapshot.PayloadJSON, "diff --git") {
		t.Fatalf("snapshot = %#v, want captured Forgejo diff payload", snapshot)
	}
	comment, err := adapter.CreateIssueComment(context.Background(), reviewer.IssueCommentInput{Repo: "acme/looper", IssueNumber: 42, Body: "Needs a test", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if comment.ID != 99 {
		t.Fatalf("comment = %#v, want created comment id", comment)
	}
	if err := adapter.RemovePullRequestLabels(context.Background(), reviewer.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:review"}, CWD: repoPath}); err != nil {
		t.Fatalf("RemovePullRequestLabels() error = %v", err)
	}
	workerAdapter := workerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	comparison, err := workerAdapter.CompareBranches(context.Background(), worker.CompareBranchesInput{Repo: "acme/looper", BaseBranch: "main", HeadBranch: "feature/review-me", CWD: repoPath})
	if err != nil {
		t.Fatalf("CompareBranches() error = %v", err)
	}
	if comparison.AheadBy != 1 || comparison.Status != "ahead" {
		t.Fatalf("comparison = %#v, want Forgejo compare result", comparison)
	}
	if listLabels != "" {
		t.Fatalf("labels query = %q, want local label filtering", listLabels)
	}
	if body, _ := commentBody["body"].(string); !strings.Contains(body, "Needs a test") {
		t.Fatalf("comment body = %#v, want stamped comment content", commentBody)
	}
	if len(removedPaths) != 1 || !strings.Contains(removedPaths[0], "/issues/42/labels/looper:review") {
		t.Fatalf("removedPaths = %#v, want Forgejo label delete", removedPaths)
	}
	if comparePath != "/api/v1/repos/acme/looper/compare/main...feature%2Freview-me" {
		t.Fatalf("comparePath = %q, want encoded Forgejo compare path", comparePath)
	}
}

func TestFixerGitHubAdapterForgejoSummaryCommentNoResolveFlow(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "secret")
	var createdCommentBody map[string]any
	var updatedCommentBody map[string]any
	var addedLabels map[string][]string
	var removedLabelPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "login": "fixer-bot"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 42, "title": "Fix me", "body": "PR body", "state": "open",
				"head":   map[string]any{"ref": "feature/fix-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:fix"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "Fix me", "body": "PR body", "state": "open",
				"head":   map[string]any{"ref": "feature/fix-me", "sha": "abc123"},
				"base":   map[string]any{"ref": "main", "sha": "base123"},
				"user":   map[string]any{"login": "alice", "id": 1},
				"labels": []map[string]any{{"id": 1, "name": "looper:fix"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id":         77,
				"body":       "<!-- looper:forgejo-reviewer-summary {\"kind\":\"looper.forgejo.reviewer_summary\",\"schema_version\":1,\"review_round_id\":1,\"items\":[{\"review_item_id\":\"R-001\",\"status\":\"open\",\"title\":\"Fix it\",\"body\":\"Needs repair\",\"last_seen_round_id\":1}]} -->",
				"html_url":   serverURL(r) + "/acme/looper/issues/42#issuecomment-77",
				"updated_at": "2026-06-30T00:00:00Z",
				"user":       map[string]any{"login": "reviewer-bot", "id": 8},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/comments":
			if err := json.NewDecoder(r.Body).Decode(&createdCommentBody); err != nil {
				t.Fatalf("decode created comment body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 88, "html_url": serverURL(r) + "/acme/looper/issues/42#issuecomment-88"})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/acme/looper/issues/comments/88":
			if err := json.NewDecoder(r.Body).Decode(&updatedCommentBody); err != nil {
				t.Fatalf("decode updated comment body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 88, "html_url": serverURL(r) + "/acme/looper/issues/42#issuecomment-88"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/acme/looper/issues/42/labels":
			if err := json.NewDecoder(r.Body).Decode(&addedLabels); err != nil {
				t.Fatalf("decode added labels: %v", err)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 2, "name": "looper:fixing"}})
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/api/v1/repos/acme/looper/issues/42/labels/"):
			removedLabelPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v1/repos/acme/looper/compare/base123...abc123":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "ahead_by": 1})
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
	adapter := fixerGitHubAdapter{stamper: disclosure.FromConfig(cfg), config: &cfg}
	ctx := context.Background()

	login, err := adapter.GetCurrentUserLogin(ctx, repoPath)
	if err != nil || login != "fixer-bot" {
		t.Fatalf("GetCurrentUserLogin() = %q, %v; want fixer-bot", login, err)
	}
	prs, err := adapter.ListOpenPullRequests(ctx, fixer.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: repoPath, Labels: []string{"looper:fix"}, BaseRefName: "main"})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 || prs[0].Author != "alice" || prs[0].HeadSHA != "abc123" {
		t.Fatalf("prs = %#v, want Forgejo fixer PR summary", prs)
	}
	detail, err := adapter.ViewPullRequest(ctx, fixer.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath})
	if err != nil {
		t.Fatalf("ViewPullRequest() error = %v", err)
	}
	if len(detail.IssueComments) != 1 || detail.Comments != nil {
		t.Fatalf("detail = %#v, want top-level issue comments only", detail)
	}
	created, err := adapter.CreateIssueComment(ctx, fixer.IssueCommentInput{Repo: "acme/looper", IssueNumber: 42, Body: "fixer summary", CWD: repoPath})
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if created.ID != 88 {
		t.Fatalf("created = %#v, want comment 88", created)
	}
	if err := adapter.UpdateIssueComment(ctx, fixer.UpdateIssueCommentInput{Repo: "acme/looper", CommentID: 88, Body: "updated fixer summary", CWD: repoPath}); err != nil {
		t.Fatalf("UpdateIssueComment() error = %v", err)
	}
	if err := adapter.AddPullRequestLabels(ctx, fixer.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:fixing"}, CWD: repoPath}); err != nil {
		t.Fatalf("AddPullRequestLabels() error = %v", err)
	}
	if err := adapter.RemovePullRequestLabels(ctx, fixer.PullRequestLabelsInput{Repo: "acme/looper", PRNumber: 42, Labels: []string{"looper:fix"}, CWD: repoPath}); err != nil {
		t.Fatalf("RemovePullRequestLabels() error = %v", err)
	}
	compare, err := adapter.CompareCommits(ctx, fixer.CompareCommitsInput{Repo: "acme/looper", Base: "base123", Head: "abc123", CWD: repoPath})
	if err != nil || compare.Status != "ahead" {
		t.Fatalf("CompareCommits() = %#v, %v; want ahead", compare, err)
	}
	if _, err := adapter.ListReviewThreads(ctx, fixer.ListReviewThreadsInput{Repo: "acme/looper", PRNumber: 42, CWD: repoPath}); err == nil || !strings.Contains(err.Error(), "does not support native review threads") {
		t.Fatalf("ListReviewThreads() error = %v, want Forgejo unsupported native review threads", err)
	}
	if err := adapter.ResolveReviewThread(ctx, fixer.ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1", CWD: repoPath}); err == nil || !strings.Contains(err.Error(), "does not support native review thread resolution") {
		t.Fatalf("ResolveReviewThread() error = %v, want Forgejo unsupported native thread resolution", err)
	}
	if body, _ := createdCommentBody["body"].(string); !strings.Contains(body, "fixer summary") {
		t.Fatalf("createdCommentBody = %#v, want stamped summary body", createdCommentBody)
	}
	if body, _ := updatedCommentBody["body"].(string); !strings.Contains(body, "updated fixer summary") {
		t.Fatalf("updatedCommentBody = %#v, want stamped summary body", updatedCommentBody)
	}
	if got := addedLabels["labels"]; len(got) != 1 || got[0] != "looper:fixing" {
		t.Fatalf("addedLabels = %#v, want looper:fixing", addedLabels)
	}
	if !strings.Contains(removedLabelPath, "/issues/42/labels/looper:fix") {
		t.Fatalf("removedLabelPath = %q, want Forgejo label removal", removedLabelPath)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
