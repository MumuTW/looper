package forgejocontract

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/nexu-io/looper/internal/forge"
)

const forgejoContractToken = "forgejo-contract-secret"

type forgejoRouteAuthority string

const (
	forgejoAuthorityOfficialDocs  forgejoRouteAuthority = "official Forgejo API docs: /api/v1 REST, token auth, page/limit pagination"
	forgejoAuthorityOpenAPI       forgejoRouteAuthority = "Forgejo instance OpenAPI: /swagger.v1.json, basePath /api/v1"
	forgejoAuthorityMVPCapability forgejoRouteAuthority = "Forgejo MVP capability surface: issues, pull requests, labels, assignees, comments, diffs, compare, create/update PR"
	forgejoAuthorityLiveObserved  forgejoRouteAuthority = "live observation: code.powerformer.net exposes Forgejo API 14.0.2+gitea-1.22.0"
)

type expectedForgejoRequest struct {
	Name        string
	Method      string
	Path        string
	Query       url.Values
	Body        string
	Status      int
	Response    any
	RawResponse string
	Headers     map[string]string
	Authority   forgejoRouteAuthority
}

type recordedForgejoRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

func TestInvariantForgejoGatewayUsesSupportedRESTSurface(t *testing.T) {
	routes := forgejoRESTContractRoutes()
	server, records := newStrictForgejoServer(t, routes)
	defer server.Close()

	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo-e2e", Kind: forge.ProviderKindForgejo, BaseURL: server.URL + "/forge/", Repo: "acme/looper"}, forgejoContractToken)
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}

	ctx := context.Background()
	identity, err := client.CurrentUser(ctx)
	if err != nil || identity.Login != "ralph" || identity.ID != 42 {
		t.Fatalf("CurrentUser() = %#v, %v", identity, err)
	}
	issues, err := client.ListOpenIssues(ctx, forge.ListIssuesInput{Labels: []string{"planner", "ready"}, Assignee: "ralph", Limit: 2})
	if err != nil || len(issues) != 2 || issues[0].Labels[0].Name != "planner" || issues[0].Assignees[0].Login != "ralph" || issues[1].Number != 8 {
		t.Fatalf("ListOpenIssues() = %#v, %v", issues, err)
	}
	issue, err := client.ViewIssue(ctx, 7)
	if err != nil || issue.Number != 7 || issue.User.Login != "octo" || issue.State != "open" {
		t.Fatalf("ViewIssue() = %#v, %v", issue, err)
	}
	labels, err := client.AddIssueLabels(ctx, 7, []string{"planner", "ready"})
	if err != nil || len(labels) != 2 || labels[1].Name != "ready" {
		t.Fatalf("AddIssueLabels() = %#v, %v", labels, err)
	}
	if err := client.RemoveIssueLabel(ctx, 7, "team/review"); err != nil {
		t.Fatalf("RemoveIssueLabel() error = %v", err)
	}
	if err := client.AddIssueAssignees(ctx, 7, []string{"ralph"}); err != nil {
		t.Fatalf("AddIssueAssignees() error = %v", err)
	}
	if err := client.RemoveIssueAssignees(ctx, 7, []string{"ralph"}); err != nil {
		t.Fatalf("RemoveIssueAssignees() error = %v", err)
	}
	comment, err := client.CreateIssueComment(ctx, forge.CreateCommentInput{IssueNumber: 7, Body: "comment body"})
	if err != nil || comment.ID != 301 || comment.User.Login != "ralph" {
		t.Fatalf("CreateIssueComment() = %#v, %v", comment, err)
	}
	comments, err := client.ListIssueComments(ctx, 7)
	if err != nil || len(comments) != 2 || comments[1].User.Login != "marge" {
		t.Fatalf("ListIssueComments() = %#v, %v", comments, err)
	}
	comment, err = client.UpdateIssueComment(ctx, forge.UpdateCommentInput{CommentID: 301, Body: "updated body"})
	if err != nil || comment.Body != "updated body" {
		t.Fatalf("UpdateIssueComment() = %#v, %v", comment, err)
	}
	pulls, err := client.ListOpenPullRequests(ctx, forge.ListPullRequestsInput{Labels: []string{"review"}, Limit: 1})
	if err != nil || len(pulls) != 1 || pulls[0].Number != 9 || pulls[0].Head.Name != "feature" || !pulls[0].IsDraft {
		t.Fatalf("ListOpenPullRequests() = %#v, %v", pulls, err)
	}
	pull, err := client.ViewPullRequest(ctx, 9)
	if err != nil || pull.Base.Name != "main" || !pull.IsDraft {
		t.Fatalf("ViewPullRequest() = %#v, %v", pull, err)
	}
	diff, err := client.PullRequestDiff(ctx, 9)
	if err != nil || !strings.Contains(diff, "diff --git") {
		t.Fatalf("PullRequestDiff() = %q, %v", diff, err)
	}
	comparison, err := client.CompareBranches(ctx, forge.CompareBranchesInput{Base: "main", Head: "feature"})
	if err != nil || comparison.Status != "ahead" || comparison.AheadBy != 2 || comparison.TotalCommits != 2 {
		t.Fatalf("CompareBranches() = %#v, %v", comparison, err)
	}
	pull, err = client.CreatePullRequest(ctx, forge.CreatePullRequestInput{Title: "New PR", Body: "new body", Head: "feat", Base: "main"})
	if err != nil || pull.Number != 10 || pull.Head.Name != "feat" {
		t.Fatalf("CreatePullRequest() = %#v, %v", pull, err)
	}
	title := "Updated PR"
	body := "updated body"
	pull, err = client.UpdatePullRequest(ctx, forge.UpdatePullRequestInput{Number: 10, Title: &title, Body: &body})
	if err != nil || pull.Title != "Updated PR" {
		t.Fatalf("UpdatePullRequest() = %#v, %v", pull, err)
	}
	_, err = client.ViewIssue(ctx, 99)
	if err == nil || strings.Contains(err.Error(), forgejoContractToken) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("ViewIssue(99) error = %v, want redacted provider error", err)
	}

	got := records()
	if len(got) != len(routes) {
		t.Fatalf("recorded %d requests, want %d: %#v", len(got), len(routes), got)
	}
	assertRecordedRequest(t, got, http.MethodDelete, "/forge/api/v1/repos/acme/looper/issues/7/labels/team%2Freview", "")
	assertRecordedRequest(t, got, http.MethodPost, "/forge/api/v1/repos/acme/looper/issues/7/labels", `{"labels":["planner","ready"]}`)
	assertRecordedRequest(t, got, http.MethodDelete, "/forge/api/v1/repos/acme/looper/issues/7/assignees", `{"assignees":["ralph"]}`)
	assertRecordedRequest(t, got, http.MethodPost, "/forge/api/v1/repos/acme/looper/pulls", `{"base":"main","body":"new body","head":"feat","title":"New PR"}`)
}

func TestForgejoRESTContractRoutesHaveAuthoritySources(t *testing.T) {
	for _, route := range forgejoRESTContractRoutes() {
		if route.Name == "" || route.Method == "" || route.Path == "" {
			t.Fatalf("incomplete route: %#v", route)
		}
		if route.Authority == "" {
			t.Fatalf("route %s has no authority source", route.Name)
		}
	}
}

func TestForgejoReadOnlySmoke(t *testing.T) {
	if os.Getenv("LOOPER_E2E_FORGEJO") == "" {
		t.Skip("set LOOPER_E2E_FORGEJO=1 with LOOPER_E2E_FORGEJO_BASE_URL, LOOPER_E2E_FORGEJO_SANDBOX_REPO, and LOOPER_E2E_FORGEJO_TOKEN to run Forgejo read-only smoke")
	}
	baseURL := strings.TrimSpace(os.Getenv("LOOPER_E2E_FORGEJO_BASE_URL"))
	repo := strings.TrimSpace(os.Getenv("LOOPER_E2E_FORGEJO_SANDBOX_REPO"))
	token := strings.TrimSpace(os.Getenv("LOOPER_E2E_FORGEJO_TOKEN"))
	if baseURL == "" || repo == "" || token == "" {
		t.Fatalf("LOOPER_E2E_FORGEJO_BASE_URL, LOOPER_E2E_FORGEJO_SANDBOX_REPO, and LOOPER_E2E_FORGEJO_TOKEN are required when LOOPER_E2E_FORGEJO=1")
	}
	client, err := forge.NewForgejoClient(forge.RepositoryRef{ProviderID: "forgejo-live-smoke", Kind: forge.ProviderKindForgejo, BaseURL: baseURL, Repo: repo}, token)
	if err != nil {
		t.Fatalf("NewForgejoClient() error = %v", err)
	}
	if _, err := client.CurrentUser(context.Background()); err != nil {
		t.Fatalf("Forgejo current-user smoke failed: %v", err)
	}
}

func forgejoRESTContractRoutes() []expectedForgejoRequest {
	return []expectedForgejoRequest{
		{Name: "current user", Method: http.MethodGet, Path: "/forge/api/v1/user", Status: http.StatusOK, Response: map[string]any{"id": 42, "login": "ralph"}, Authority: forgejoAuthorityOfficialDocs},
		{Name: "list issues page 1", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/issues", Query: values("assignee", "ralph", "labels", "planner,ready", "limit", "2", "page", "1", "state", "open"), Status: http.StatusOK, Headers: map[string]string{"X-Total-Pages": "2"}, Response: []map[string]any{{"number": 7, "title": "Issue 7", "body": "body 7", "state": "open", "html_url": "https://example.test/issues/7", "updated_at": "2026-06-18T00:00:00Z", "user": map[string]any{"id": 1, "login": "octo"}, "labels": []map[string]any{{"id": 11, "name": "planner"}}, "assignees": []map[string]any{{"id": 42, "login": "ralph"}}}}, Authority: forgejoAuthorityOfficialDocs},
		{Name: "list issues page 2", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/issues", Query: values("assignee", "ralph", "labels", "planner,ready", "limit", "1", "page", "2", "state", "open"), Status: http.StatusOK, Response: []map[string]any{{"number": 8, "title": "Issue 8", "body": "body 8", "state": "open", "html_url": "https://example.test/issues/8", "updated_at": "2026-06-18T01:00:00Z", "user": map[string]any{"id": 2, "login": "marge"}}}, Authority: forgejoAuthorityOfficialDocs},
		{Name: "view issue", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/issues/7", Status: http.StatusOK, Response: map[string]any{"number": 7, "title": "Issue 7", "body": "body 7", "state": "open", "html_url": "https://example.test/issues/7", "updated_at": "2026-06-18T00:00:00Z", "user": map[string]any{"id": 1, "login": "octo"}, "labels": []map[string]any{{"id": 11, "name": "planner"}}, "assignees": []map[string]any{{"id": 42, "login": "ralph"}}}, Authority: forgejoAuthorityOpenAPI},
		{Name: "add labels", Method: http.MethodPost, Path: "/forge/api/v1/repos/acme/looper/issues/7/labels", Body: `{"labels":["planner","ready"]}`, Status: http.StatusOK, Response: []map[string]any{{"id": 11, "name": "planner"}, {"id": 12, "name": "ready"}}, Authority: forgejoAuthorityMVPCapability},
		{Name: "remove slash label", Method: http.MethodDelete, Path: "/forge/api/v1/repos/acme/looper/issues/7/labels/team%2Freview", Status: http.StatusNoContent, Authority: forgejoAuthorityMVPCapability},
		{Name: "add assignee", Method: http.MethodPost, Path: "/forge/api/v1/repos/acme/looper/issues/7/assignees", Body: `{"assignees":["ralph"]}`, Status: http.StatusNoContent, Authority: forgejoAuthorityMVPCapability},
		{Name: "remove assignee", Method: http.MethodDelete, Path: "/forge/api/v1/repos/acme/looper/issues/7/assignees", Body: `{"assignees":["ralph"]}`, Status: http.StatusNoContent, Authority: forgejoAuthorityMVPCapability},
		{Name: "create comment", Method: http.MethodPost, Path: "/forge/api/v1/repos/acme/looper/issues/7/comments", Body: `{"body":"comment body"}`, Status: http.StatusOK, Response: map[string]any{"id": 301, "body": "comment body", "html_url": "https://example.test/comments/301", "updated_at": "2026-06-18T02:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}}, Authority: forgejoAuthorityMVPCapability},
		{Name: "list comments", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/issues/7/comments", Query: values("limit", "50", "page", "1"), Status: http.StatusOK, Response: []map[string]any{{"id": 301, "body": "comment body", "html_url": "https://example.test/comments/301", "updated_at": "2026-06-18T02:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}}, {"id": 302, "body": "follow-up", "html_url": "https://example.test/comments/302", "updated_at": "2026-06-18T02:30:00Z", "user": map[string]any{"id": 7, "login": "marge"}}}, Authority: forgejoAuthorityOfficialDocs},
		{Name: "update comment", Method: http.MethodPatch, Path: "/forge/api/v1/repos/acme/looper/issues/comments/301", Body: `{"body":"updated body"}`, Status: http.StatusOK, Response: map[string]any{"id": 301, "body": "updated body", "html_url": "https://example.test/comments/301", "updated_at": "2026-06-18T03:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}}, Authority: forgejoAuthorityMVPCapability},
		{Name: "list pulls page 1", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/pulls", Query: values("limit", "50", "page", "1", "state", "open"), Status: http.StatusOK, Headers: map[string]string{"Link": `</forge/api/v1/repos/acme/looper/pulls?page=2>; rel="next"`}, Response: []map[string]any{{"number": 8, "title": "Other PR", "body": "body 8", "state": "open", "html_url": "https://example.test/pulls/8", "updated_at": "2026-06-18T03:30:00Z", "user": map[string]any{"id": 5, "login": "bart"}, "head": map[string]any{"ref": "other", "sha": "othersha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}, "labels": []map[string]any{{"id": 14, "name": "other"}}}}, Authority: forgejoAuthorityOfficialDocs},
		{Name: "list pulls page 2", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/pulls", Query: values("limit", "50", "page", "2", "state", "open"), Status: http.StatusOK, Response: []map[string]any{{"number": 9, "title": "PR 9", "body": "body 9", "state": "open", "draft": true, "html_url": "https://example.test/pulls/9", "updated_at": "2026-06-18T04:00:00Z", "user": map[string]any{"id": 3, "login": "lisa"}, "head": map[string]any{"ref": "feature", "sha": "headsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}, "labels": []map[string]any{{"id": 13, "name": "review"}}, "assignees": []map[string]any{{"id": 42, "login": "ralph"}}}}, Authority: forgejoAuthorityOfficialDocs},
		{Name: "view pull", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/pulls/9", Status: http.StatusOK, Response: map[string]any{"number": 9, "title": "PR 9", "body": "body 9", "state": "open", "draft": true, "html_url": "https://example.test/pulls/9", "updated_at": "2026-06-18T04:00:00Z", "user": map[string]any{"id": 3, "login": "lisa"}, "head": map[string]any{"ref": "feature", "sha": "headsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}}, Authority: forgejoAuthorityOpenAPI},
		{Name: "pull diff", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/pulls/9.diff", Status: http.StatusOK, RawResponse: "diff --git a/foo b/foo", Authority: forgejoAuthorityOpenAPI},
		{Name: "compare", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/compare/main...feature", Status: http.StatusOK, Response: map[string]any{"status": "ahead", "ahead_by": 2, "behind_by": 0, "total_commits": 2}, Authority: forgejoAuthorityOpenAPI},
		{Name: "create pull", Method: http.MethodPost, Path: "/forge/api/v1/repos/acme/looper/pulls", Body: `{"base":"main","body":"new body","head":"feat","title":"New PR"}`, Status: http.StatusCreated, Response: map[string]any{"number": 10, "title": "New PR", "body": "new body", "state": "open", "html_url": "https://example.test/pulls/10", "updated_at": "2026-06-18T05:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}, "head": map[string]any{"ref": "feat", "sha": "newsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}}, Authority: forgejoAuthorityMVPCapability},
		{Name: "update pull", Method: http.MethodPatch, Path: "/forge/api/v1/repos/acme/looper/pulls/10", Body: `{"body":"updated body","title":"Updated PR"}`, Status: http.StatusOK, Response: map[string]any{"number": 10, "title": "Updated PR", "body": "updated body", "state": "open", "html_url": "https://example.test/pulls/10", "updated_at": "2026-06-18T06:00:00Z", "user": map[string]any{"id": 42, "login": "ralph"}, "head": map[string]any{"ref": "feat", "sha": "newsha"}, "base": map[string]any{"ref": "main", "sha": "basesha"}}, Authority: forgejoAuthorityMVPCapability},
		{Name: "sanitized error", Method: http.MethodGet, Path: "/forge/api/v1/repos/acme/looper/issues/99", Status: http.StatusUnauthorized, RawResponse: "token forgejo-contract-secret rejected", Authority: forgejoAuthorityLiveObserved},
	}
}

func newStrictForgejoServer(tb testing.TB, routes []expectedForgejoRequest) (*httptest.Server, func() []recordedForgejoRequest) {
	tb.Helper()
	var mu sync.Mutex
	var index int
	var records []recordedForgejoRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			tb.Fatalf("read request body: %v", err)
		}
		body := string(bodyBytes)
		mu.Lock()
		defer mu.Unlock()
		if index >= len(routes) {
			tb.Fatalf("unexpected extra request: %s %s?%s body=%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery, body)
		}
		expected := routes[index]
		index++
		records = append(records, recordedForgejoRequest{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: body})
		if r.Method != expected.Method || r.URL.EscapedPath() != expected.Path {
			tb.Fatalf("request %s = %s %s, want %s %s", expected.Name, r.Method, r.URL.EscapedPath(), expected.Method, expected.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token "+forgejoContractToken {
			tb.Fatalf("request %s Authorization = %q, want token auth", expected.Name, got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			tb.Fatalf("request %s Accept = %q, want application/json", expected.Name, got)
		}
		if expected.Body != "" {
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				tb.Fatalf("request %s Content-Type = %q, want application/json", expected.Name, got)
			}
			assertJSONEqual(tb, expected.Name, body, expected.Body)
		} else if strings.TrimSpace(body) != "" {
			tb.Fatalf("request %s body = %q, want empty", expected.Name, body)
		}
		if !equalValues(r.URL.Query(), expected.Query) {
			tb.Fatalf("request %s query = %q (%#v), want %#v", expected.Name, r.URL.RawQuery, r.URL.Query(), expected.Query)
		}
		for key, value := range expected.Headers {
			w.Header().Set(key, value)
		}
		if expected.Response != nil {
			w.Header().Set("Content-Type", "application/json")
		}
		if expected.Status != 0 {
			w.WriteHeader(expected.Status)
		}
		if expected.Response != nil {
			if err := json.NewEncoder(w).Encode(expected.Response); err != nil {
				tb.Fatalf("encode response for %s: %v", expected.Name, err)
			}
			return
		}
		if expected.RawResponse != "" {
			_, _ = w.Write([]byte(expected.RawResponse))
		}
	}))
	return server, func() []recordedForgejoRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedForgejoRequest(nil), records...)
	}
}

func values(pairs ...string) url.Values {
	if len(pairs)%2 != 0 {
		panic("values requires key/value pairs")
	}
	out := url.Values{}
	for i := 0; i < len(pairs); i += 2 {
		out.Add(pairs[i], pairs[i+1])
	}
	return out
}

func equalValues(got, want url.Values) bool {
	if len(got) != len(want) {
		return false
	}
	for key, wantValues := range want {
		gotValues := got[key]
		if len(gotValues) != len(wantValues) {
			return false
		}
		for i := range wantValues {
			if gotValues[i] != wantValues[i] {
				return false
			}
		}
	}
	return true
}

func assertJSONEqual(tb testing.TB, name string, got string, want string) {
	tb.Helper()
	var gotJSON any
	if err := json.Unmarshal([]byte(got), &gotJSON); err != nil {
		tb.Fatalf("request %s decode got JSON %q: %v", name, got, err)
	}
	var wantJSON any
	if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
		tb.Fatalf("request %s decode want JSON %q: %v", name, want, err)
	}
	gotCanonical, _ := json.Marshal(gotJSON)
	wantCanonical, _ := json.Marshal(wantJSON)
	if string(gotCanonical) != string(wantCanonical) {
		tb.Fatalf("request %s body = %s, want %s", name, gotCanonical, wantCanonical)
	}
}

func assertRecordedRequest(tb testing.TB, records []recordedForgejoRequest, method string, path string, body string) {
	tb.Helper()
	for _, record := range records {
		if record.Method != method || record.Path != path {
			continue
		}
		if body == "" {
			return
		}
		assertJSONEqual(tb, fmt.Sprintf("%s %s", method, path), record.Body, body)
		return
	}
	tb.Fatalf("did not find recorded request %s %s in %#v", method, path, records)
}
