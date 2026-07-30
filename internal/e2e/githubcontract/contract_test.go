package githubcontract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/e2e/harness"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/labels"
)

type fakeGHState struct {
	Routes       map[string]json.RawMessage       `json:"routes,omitempty"`
	GraphQL      map[string]json.RawMessage       `json:"graphql,omitempty"`
	PullRequests map[string]harness.GHPullRequest `json:"pullRequests,omitempty"`
}

func TestInvariantGatewayUsesSupportedGHJSONFields(t *testing.T) {
	bins := harness.MustBinaries(t)
	schema := loadFixtureSchema(t)
	fakeGH := harness.NewFakeGH(t, bins, schema)
	writeFakeState(t, fakeGH.StatePath, fakeGHState{
		Routes: map[string]json.RawMessage{
			"repos/acme/looper/issues/7":          json.RawMessage(`{"number":7,"title":"Issue title","body":"body","html_url":"https://github.com/acme/looper/issues/7","state":"open","state_reason":"completed","updated_at":"2026-05-12T00:00:00Z","user":{"login":"octocat"},"author_association":"COLLABORATOR","assignees":[{"login":"octocat"}],"labels":[{"name":"bug"}]}`),
			"repos/acme/looper/issues/7/comments": json.RawMessage(`[]`),
		},
		GraphQL: map[string]json.RawMessage{
			"default":             json.RawMessage(`{"data":{"node":{"id":"thread-1","isResolved":false,"path":"foo.go","line":7,"comments":{"nodes":[{"id":"comment-1","body":"please fix","author":{"login":"octocat"},"createdAt":"2026-05-12T00:00:00Z","updatedAt":"2026-05-12T00:00:00Z","path":"foo.go","line":7,"originalCommit":{"oid":"base-head"},"commit":{"oid":"head-1"},"url":"https://example.test/thread-1"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`),
			"resolveReviewThread": json.RawMessage(`{"data":{"resolveReviewThread":{"thread":{"id":"thread-1","isResolved":true}}}}`),
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})

	ctx := context.Background()
	issues, err := gateway.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: "acme/looper", CWD: root, Limit: 5, Assignee: "reviewer", Label: "phase-1"})
	if err != nil {
		t.Fatalf("ListOpenIssues() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListOpenIssues() len = %d, want 1", len(issues))
	}
	if issues[0].AuthorAssociation != "" {
		t.Fatalf("issues[0].AuthorAssociation = %q, want empty unsupported field", issues[0].AuthorAssociation)
	}
	prs, err := gateway.ListOpenPullRequests(ctx, githubinfra.ListOpenPullRequestsInput{Repo: "acme/looper", CWD: root, Limit: 5})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("ListOpenPullRequests() len = %d, want 1", len(prs))
	}
	if prs[0].AuthorAssociation != "" {
		t.Fatalf("prs[0].AuthorAssociation = %q, want empty unsupported field", prs[0].AuthorAssociation)
	}
	issue, err := gateway.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: "acme/looper", IssueNumber: 7, CWD: root})
	if err != nil {
		t.Fatalf("ViewIssue() error = %v", err)
	}
	if issue.AuthorAssociation != "COLLABORATOR" {
		t.Fatalf("issue.AuthorAssociation = %q, want COLLABORATOR", issue.AuthorAssociation)
	}
	if issue.StateReason != "completed" {
		t.Fatalf("issue.StateReason = %q, want completed", issue.StateReason)
	}
	if err := gateway.ResolveReviewThread(ctx, githubinfra.ResolveReviewThreadInput{Repo: "acme/looper", ThreadID: "thread-1", CWD: root}); err != nil {
		t.Fatalf("ResolveReviewThread() error = %v", err)
	}
	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	assertInvocationHasJSONFields(t, invocations, "issue", "list", []string{"number", "title", "body", "url", "state", "updatedAt", "author", "assignees", "labels"})
	assertInvocationMissingJSONField(t, invocations, "issue", "list", "authorAssociation")
	assertInvocationHasJSONFields(t, invocations, "pr", "list", []string{"number", "title", "url", "state", "updatedAt", "isDraft", "reviewDecision", "labels", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "author", "reviewRequests", "reviews", "mergeStateStatus"})
	assertInvocationMissingJSONField(t, invocations, "pr", "list", "authorAssociation")
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/7"})
	assertInvocationContains(t, invocations, []string{"api", "graphql"})
}

func TestInvariantFixerDiscoveryUsesBoundedIssueCommentProjection(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	writeFakeState(t, fakeGH.StatePath, fakeGHState{
		Routes: map[string]json.RawMessage{
			"repos/acme/looper/issues/42/comments": json.RawMessage(`[{"id":202,"body":"<!-- looper:fixer-round head=head-42 -->","html_url":"https://example.test/pull/42#issuecomment-202","user":{"login":"looper"}}]`),
		},
		GraphQL: map[string]json.RawMessage{
			"default": json.RawMessage(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`),
		},
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {Number: 42, Repo: "acme/looper", State: "OPEN", HeadSHA: "head-42", BaseSHA: "base-42", HeadRefName: "feature", BaseRefName: "main"},
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})

	detail, err := gateway.ViewPullRequestForFixer(context.Background(), githubinfra.ViewPullRequestInput{Repo: "acme/looper", PRNumber: 42, CWD: root})
	if err != nil {
		t.Fatalf("ViewPullRequestForFixer() error = %v", err)
	}
	if len(detail.IssueComments) != 1 || detail.IssueComments[0].ID != 202 {
		t.Fatalf("IssueComments = %#v, want projected automation comment", detail.IssueComments)
	}

	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	for _, invocation := range invocations {
		argv := argvStrings(invocation)
		if !containsOrdered(argv, []string{"api", "--paginate", "repos/acme/looper/issues/42/comments", "--jq"}) {
			continue
		}
		if containsOrdered(argv, []string{"--slurp"}) {
			t.Fatalf("comment invocation = %v, want page-wise output without --slurp", argv)
		}
		filter := argv[len(argv)-1]
		for _, required := range []string{"looper:fixer-round", "looper:conflict-notice", "looper:reviewer:automerge-refused", "{id,body,html_url,updated_at,user:{login:.user.login}}"} {
			if !strings.Contains(filter, required) {
				t.Fatalf("comment projection = %q, want %q", filter, required)
			}
		}
		if strings.Contains(filter, `contains("looper:")`) || strings.Contains(filter, "looper:stamp") {
			t.Fatalf("comment projection = %q, want only consumed protocol markers", filter)
		}
		return
	}
	t.Fatalf("missing bounded issue-comment projection in %#v", invocations)
}

func TestInvariantGatewayDependencyWrappersUseSupportedRoutes(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	writeFakeState(t, fakeGH.StatePath, fakeGHState{
		Routes: map[string]json.RawMessage{
			"repos/acme/looper/issues/22/dependencies/blocked_by": json.RawMessage(`[{"id":101,"number":101,"title":"blocked by","url":"https://api.example.test/issues/101","html_url":"https://example.test/issues/101","repository_url":"https://api.example.test/repos/acme/looper","state":"open","state_reason":"","repository":{"name":"looper","full_name":"acme/looper","url":"https://api.example.test/repos/acme/looper","html_url":"https://example.test/acme/looper"}}]`),
			"repos/acme/looper/issues/22/dependencies/blocking":   json.RawMessage(`[{"id":102,"number":102,"title":"blocking","url":"https://api.example.test/issues/102","html_url":"https://example.test/issues/102","repository_url":"https://api.example.test/repos/acme/looper","state":"closed","state_reason":"completed","repository":{"name":"looper","full_name":"acme/looper","url":"https://api.example.test/repos/acme/looper","html_url":"https://example.test/acme/looper"}}]`),
			"repos/acme/looper/issues/22/sub_issues":              json.RawMessage(`[{"id":103,"number":103,"title":"sub issue","url":"https://api.example.test/issues/103","html_url":"https://example.test/issues/103","repository_url":"https://api.example.test/repos/acme/looper","state":"open","state_reason":"","repository":{"name":"looper","full_name":"acme/looper","url":"https://api.example.test/repos/acme/looper","html_url":"https://example.test/acme/looper"}}]`),
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})

	blockedBy, err := gateway.ListBlockedByIssues(context.Background(), githubinfra.ViewIssueInput{Repo: "acme/looper", IssueNumber: 22, CWD: root})
	if err != nil {
		t.Fatalf("ListBlockedByIssues() error = %v", err)
	}
	blocking, err := gateway.ListBlockingIssues(context.Background(), githubinfra.ViewIssueInput{Repo: "acme/looper", IssueNumber: 22, CWD: root})
	if err != nil {
		t.Fatalf("ListBlockingIssues() error = %v", err)
	}
	subIssues, err := gateway.ListSubIssues(context.Background(), githubinfra.ViewIssueInput{Repo: "acme/looper", IssueNumber: 22, CWD: root})
	if err != nil {
		t.Fatalf("ListSubIssues() error = %v", err)
	}
	if len(blockedBy) != 1 || blockedBy[0].Number != 101 || blockedBy[0].Repository.FullName != "acme/looper" {
		t.Fatalf("blockedBy = %#v, want parsed blocked-by route", blockedBy)
	}
	if len(blocking) != 1 || blocking[0].Number != 102 || blocking[0].StateReason != "completed" {
		t.Fatalf("blocking = %#v, want parsed blocking route", blocking)
	}
	if len(subIssues) != 1 || subIssues[0].Number != 103 {
		t.Fatalf("subIssues = %#v, want parsed sub-issue route", subIssues)
	}
	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	assertInvocationContains(t, invocations, []string{"api", "--paginate", "--slurp", "repos/acme/looper/issues/22/dependencies/blocked_by"})
	assertInvocationContains(t, invocations, []string{"api", "--paginate", "--slurp", "repos/acme/looper/issues/22/dependencies/blocking"})
	assertInvocationContains(t, invocations, []string{"api", "--paginate", "--slurp", "repos/acme/looper/issues/22/sub_issues"})
}

func TestInvariantGatewaySupportsRepoForms(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	writeFakeState(t, fakeGH.StatePath, fakeGHState{
		Routes: map[string]json.RawMessage{
			"repos/acme/looper/issues/11":          json.RawMessage(`{"number":11,"title":"Issue title","body":"body","html_url":"https://example.test/acme/looper/issues/11","state":"open","updated_at":"2026-05-12T00:00:00Z","user":{"login":"octocat"}}`),
			"repos/acme/looper/issues/11/comments": json.RawMessage(`[]`),
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})
	for _, repo := range []string{"acme/looper", "github.com/acme/looper", "ghe.example.com/acme/looper"} {
		if _, err := gateway.ViewIssue(context.Background(), githubinfra.ViewIssueInput{Repo: repo, IssueNumber: 11, CWD: root}); err != nil {
			t.Fatalf("ViewIssue(%q) error = %v", repo, err)
		}
	}
	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/11"})
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/11", "--hostname", "github.com"})
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/11", "--hostname", "ghe.example.com"})
}

func TestFakeGHFixtureRejectsUnsupportedJSONField(t *testing.T) {
	t.Parallel()
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	cmd := exec.Command(fakeGH.Path, "pr", "list", "--json", "number,authorAssociation")
	cmd.Env = append(os.Environ(), flattenEnv(fakeGH.EnvMap())...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected unsupported field failure")
	}
	if !strings.Contains(string(output), `unknown JSON field: "authorAssociation"`) {
		t.Fatalf("output = %s, want unsupported-field error", string(output))
	}
}

// TestFakeGHFixtureRejectsUnsupportedLabelListField guards the label-list JSON
// contract: fake-gh must validate requested fields against the schema
// allowlist, like the other JSON-producing list commands, so a caller that
// requests an unsupported field is rejected instead of silently returning
// complete label objects.
func TestFakeGHFixtureRejectsUnsupportedLabelListField(t *testing.T) {
	t.Parallel()
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	cmd := exec.Command(fakeGH.Path, "label", "list", "--repo", "acme/looper", "--json", "name,id")
	cmd.Env = append(os.Environ(), flattenEnv(fakeGH.EnvMap())...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected unsupported label-list field failure")
	}
	if !strings.Contains(string(output), `unknown JSON field: "id"`) {
		t.Fatalf("output = %s, want unsupported-field error", string(output))
	}
}

// TestInvariantApplyingLabelsNeverRewritesAnExistingLabel is the contract-level
// coverage for the read-before-write label path. It seeds a hand-worded
// existing label through the supported harness API (harness.GHState), runs the
// real gateway against fake-gh, and confirms: no --force is passed, the
// existing label is not recreated, the missing label is created, and the
// stored wording survives the create round trip. This exercises the stateful
// fake-gh label model end-to-end rather than asserting on argv strings alone.
func TestInvariantApplyingLabelsNeverRewritesAnExistingLabel(t *testing.T) {
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	fakeGH.WriteState(t, harness.GHState{
		RepositoryLabels: map[string][]harness.GHLabel{
			"acme/looper": {
				{Name: labels.HoldGlobal, Color: "b60205", Description: "Human veto, hand-worded"},
			},
		},
	})
	root := t.TempDir()
	for key, value := range fakeGH.EnvMap() {
		t.Setenv(key, value)
	}
	t.Setenv("HOME", root)
	gateway := githubinfra.New(githubinfra.Options{GHPath: fakeGH.Path, CWD: root})

	if err := gateway.AddIssueLabels(context.Background(), githubinfra.IssueLabelsInput{
		Repo:        "acme/looper",
		IssueNumber: 7,
		Labels:      []string{labels.HoldGlobal, labels.DefaultPlanTrigger},
	}); err != nil {
		t.Fatalf("AddIssueLabels() error = %v", err)
	}

	invocations := readInvocationsForContract(t, fakeGH.InvocationLog)
	assertInvocationHasJSONFields(t, invocations, "label", "list", []string{"name", "color", "description"})
	log := invocationLogString(invocations)
	if strings.Contains(log, "--force") {
		t.Errorf("gh was called with --force, which rewrites existing labels:\n%s", log)
	}
	if strings.Contains(log, "label create "+labels.HoldGlobal+" ") {
		t.Errorf("existing label %s was recreated:\n%s", labels.HoldGlobal, log)
	}
	if !strings.Contains(log, "label create "+labels.DefaultPlanTrigger+" ") {
		t.Errorf("missing label %s was not created:\n%s", labels.DefaultPlanTrigger, log)
	}
	assertInvocationContains(t, invocations, []string{"api", "repos/acme/looper/issues/7/labels", "--method", "POST"})

	// The stored wording must survive the create round trip: looper:hold keeps
	// its hand-worded description, and looper:plan is now present.
	state := readFakeGHStateFile(t, fakeGH.StatePath)
	labelsForRepo := state.RepositoryLabels["acme/looper"]
	hold := findGHLabel(t, labelsForRepo, labels.HoldGlobal)
	if hold.Description != "Human veto, hand-worded" {
		t.Errorf("looper:hold description = %q, want hand-worded wording preserved", hold.Description)
	}
	if findGHLabel(t, labelsForRepo, labels.DefaultPlanTrigger) == nil {
		t.Errorf("looper:plan missing from fake-gh state after AddIssueLabels: %#v", labelsForRepo)
	}
}

// TestFakeGHLabelCreateModelsBothDuplicateOutcomes pins fake-gh's fidelity to
// real `gh label create`: a duplicate without --force is rejected with "already
// exists" (the error the gateway tolerates when it loses a list/create race),
// and --force updates the existing label's color and description in place
// rather than failing. Modeling both outcomes is what lets the harness expose a
// reintroduced --force instead of masking it.
func TestFakeGHLabelCreateModelsBothDuplicateOutcomes(t *testing.T) {
	t.Parallel()
	bins := harness.MustBinaries(t)
	fakeGH := harness.NewFakeGH(t, bins, loadFixtureSchema(t))
	fakeGH.WriteState(t, harness.GHState{
		RepositoryLabels: map[string][]harness.GHLabel{
			"acme/looper": {
				{Name: labels.DefaultPlanTrigger, Color: "5319e7", Description: "Picked up automatically by planner"},
			},
		},
	})

	duplicate := exec.Command(fakeGH.Path, "label", "create", labels.DefaultPlanTrigger, "--repo", "acme/looper", "--color", "5319e7", "--description", "rewritten wording")
	duplicate.Env = append(os.Environ(), flattenEnv(fakeGH.EnvMap())...)
	output, err := duplicate.CombinedOutput()
	if err == nil {
		t.Fatal("expected duplicate label create to fail without --force")
	}
	if !strings.Contains(string(output), "already exists") {
		t.Fatalf("output = %s, want already-exists error", string(output))
	}
	state := readFakeGHStateFile(t, fakeGH.StatePath)
	plan := findGHLabel(t, state.RepositoryLabels["acme/looper"], labels.DefaultPlanTrigger)
	if plan.Description != "Picked up automatically by planner" {
		t.Errorf("looper:hold description changed without --force = %q, want original preserved", plan.Description)
	}

	forced := exec.Command(fakeGH.Path, "label", "create", labels.DefaultPlanTrigger, "--repo", "acme/looper", "--color", "000000", "--description", "rewritten wording", "--force")
	forced.Env = append(os.Environ(), flattenEnv(fakeGH.EnvMap())...)
	if output, err := forced.CombinedOutput(); err != nil {
		t.Fatalf("label create --force on existing label failed: %v\n%s", err, string(output))
	}
	state = readFakeGHStateFile(t, fakeGH.StatePath)
	plan = findGHLabel(t, state.RepositoryLabels["acme/looper"], labels.DefaultPlanTrigger)
	if plan.Description != "rewritten wording" || plan.Color != "000000" {
		t.Errorf("looper:plan after --force = %+v, want rewritten wording and color 000000", plan)
	}
}

func TestRealGHReadOnlySmoke(t *testing.T) {
	if os.Getenv("LOOPER_E2E_REAL_GH") == "" {
		t.Skip("set LOOPER_E2E_REAL_GH=1 to run real-gh smoke")
	}
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		t.Skipf("gh not available: %v", err)
	}
	root := t.TempDir()
	gateway := githubinfra.New(githubinfra.Options{GHPath: ghPath, CWD: root})
	if _, err := gateway.ListOpenPullRequests(context.Background(), githubinfra.ListOpenPullRequestsInput{Repo: "nexu-io/looper", CWD: root, Limit: 1}); err != nil {
		t.Fatalf("real-gh pr list smoke failed; fixture may be stale: %v", err)
	}
}

// TestRealGHLabelCreateModelsDuplicateSemantics pins the fake-gh label model
// against real `gh` behavior. Issue #223: nothing validates the fake-gh label
// model against real `gh`. This test runs only with LOOPER_E2E_REAL_GH=1 against
// a sandbox repo (LOOPER_E2E_GITHUB_SANDBOX_REPO, or its legacy alias) and asserts:
//   - creating a label succeeds
//   - creating it again without --force fails with "already exists"
//   - creating it again with --force updates the label
//
// If real `gh` behavior drifts from the fake-gh model, this test fails and
// signals that handleLabelCreate must be updated to match.
func TestRealGHLabelCreateModelsDuplicateSemantics(t *testing.T) {
	if strings.TrimSpace(os.Getenv("LOOPER_E2E_REAL_GH")) != "1" {
		t.Skip("set LOOPER_E2E_REAL_GH=1 to run real-gh label contract")
	}
	sandboxRepo := realGHSandboxRepo(t)
	if sandboxRepo == "" {
		t.Skip("set LOOPER_E2E_GITHUB_SANDBOX_REPO (e.g. user/looper-sandbox) to run real-gh label contract")
	}
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		t.Skipf("gh not available: %v", err)
	}

	uniqueLabel := fmt.Sprintf("looper-test-%d", time.Now().UnixNano())
	createArgs := []string{"label", "create", uniqueLabel, "--repo", sandboxRepo, "--color", "5319e7", "--description", "Looper E2E contract test"}
	cmd := exec.Command(ghPath, createArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real-gh label create failed: %v\n%s", err, string(out))
	}
	t.Cleanup(func() {
		out, err := exec.Command(ghPath, "label", "delete", uniqueLabel, "--repo", sandboxRepo, "--yes").CombinedOutput()
		if err != nil {
			t.Errorf("real-gh label cleanup failed: %v\n%s", err, string(out))
		}
	})

	dupCmd := exec.Command(ghPath, "label", "create", uniqueLabel, "--repo", sandboxRepo, "--color", "5319e7", "--description", "rewritten wording")
	if out, err := dupCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected real-gh duplicate label create to fail without --force\n%s", string(out))
	} else if !strings.Contains(string(out), "already exists") {
		t.Fatalf("expected real-gh \"already exists\" error, got: %v\n%s", err, string(out))
	}

	forceCmd := exec.Command(ghPath, "label", "create", uniqueLabel, "--repo", sandboxRepo, "--color", "000000", "--description", "rewritten wording", "--force")
	if out, err := forceCmd.CombinedOutput(); err != nil {
		t.Fatalf("real-gh label create --force failed: %v\n%s", err, string(out))
	}

	listCmd := exec.Command(ghPath, "label", "list", "--repo", sandboxRepo, "--search", uniqueLabel, "--json", "name,color,description", "--limit", "100")
	out, err := listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real-gh label readback failed: %v\n%s", err, string(out))
	}
	var listed []struct {
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		t.Fatalf("decode real-gh label readback: %v\n%s", err, string(out))
	}
	for _, label := range listed {
		if label.Name != uniqueLabel {
			continue
		}
		if label.Color != "000000" || label.Description != "rewritten wording" {
			t.Fatalf("real-gh label after --force = %+v, want color 000000 and rewritten wording", label)
		}
		return
	}
	t.Fatalf("real-gh label %q missing after --force; labels = %+v", uniqueLabel, listed)
}

func realGHSandboxRepo(t *testing.T) string {
	t.Helper()
	preferred := strings.TrimSpace(os.Getenv("LOOPER_E2E_GITHUB_SANDBOX_REPO"))
	legacy := strings.TrimSpace(os.Getenv("LOOPER_E2E_SANDBOX_REPO"))
	if preferred != "" && legacy != "" && preferred != legacy {
		t.Fatalf("LOOPER_E2E_GITHUB_SANDBOX_REPO and LOOPER_E2E_SANDBOX_REPO select different repositories (%q != %q)", preferred, legacy)
	}
	if preferred != "" {
		return preferred
	}
	return legacy
}

func loadFixtureSchema(tb testing.TB) harness.GHSchema {
	tb.Helper()
	path := filepath.Join("testdata", "gh-schema", "schema.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read gh schema fixture: %v", err)
	}
	var schema harness.GHSchema
	if err := json.Unmarshal(payload, &schema); err != nil {
		tb.Fatalf("decode gh schema fixture: %v", err)
	}
	return schema
}

func writeFakeState(tb testing.TB, path string, state fakeGHState) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatalf("mkdir fake-gh state dir: %v", err)
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		tb.Fatalf("marshal fake-gh state: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		tb.Fatalf("write fake-gh state: %v", err)
	}
}

func flattenEnv(env map[string]string) []string {
	items := make([]string, 0, len(env))
	for key, value := range env {
		items = append(items, key+"="+value)
	}
	return items
}

func readInvocationsForContract(tb testing.TB, path string) []map[string]any {
	tb.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read invocation log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			tb.Fatalf("decode invocation line %q: %v", line, err)
		}
		out = append(out, item)
	}
	return out
}

func assertInvocationContains(tb testing.TB, invocations []map[string]any, want []string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv, _ := invocation["argv"].([]any)
		parts := make([]string, 0, len(argv))
		for _, part := range argv {
			parts = append(parts, part.(string))
		}
		if containsOrdered(parts, want) {
			return
		}
	}
	tb.Fatalf("did not find invocation containing %v in %#v", want, invocations)
}

func assertInvocationHasJSONFields(tb testing.TB, invocations []map[string]any, noun string, verb string, want []string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv := argvStrings(invocation)
		if len(argv) < 2 || argv[0] != noun || argv[1] != verb {
			continue
		}
		for index, arg := range argv {
			if arg == "--json" && index+1 < len(argv) {
				got := strings.Split(argv[index+1], ",")
				if strings.Join(got, ",") != strings.Join(want, ",") {
					tb.Fatalf("%s %s json fields = %v, want %v", noun, verb, got, want)
				}
				return
			}
		}
	}
	tb.Fatalf("no %s %s invocation found", noun, verb)
}

func assertInvocationMissingJSONField(tb testing.TB, invocations []map[string]any, noun string, verb string, field string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv := argvStrings(invocation)
		if len(argv) < 2 || argv[0] != noun || argv[1] != verb {
			continue
		}
		for index, arg := range argv {
			if arg == "--json" && index+1 < len(argv) && strings.Contains(argv[index+1], field) {
				tb.Fatalf("%s %s unexpectedly requested %s: %v", noun, verb, field, argv)
			}
		}
	}
}

func argvStrings(invocation map[string]any) []string {
	argv, _ := invocation["argv"].([]any)
	parts := make([]string, 0, len(argv))
	for _, part := range argv {
		parts = append(parts, part.(string))
	}
	return parts
}

func containsOrdered(haystack []string, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	start := 0
	for _, want := range needle {
		found := false
		for start < len(haystack) {
			if haystack[start] == want {
				found = true
				start++
				break
			}
			start++
		}
		if !found {
			return false
		}
	}
	return true
}

func invocationLogString(invocations []map[string]any) string {
	var lines []string
	for _, invocation := range invocations {
		lines = append(lines, strings.Join(argvStrings(invocation), " "))
	}
	return strings.Join(lines, "\n")
}

func readFakeGHStateFile(tb testing.TB, path string) harness.GHState {
	tb.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read fake-gh state: %v", err)
	}
	var state harness.GHState
	if err := json.Unmarshal(payload, &state); err != nil {
		tb.Fatalf("decode fake-gh state: %v", err)
	}
	return state
}

func findGHLabel(tb testing.TB, labels []harness.GHLabel, name string) *harness.GHLabel {
	tb.Helper()
	for index := range labels {
		if strings.EqualFold(labels[index].Name, name) {
			return &labels[index]
		}
	}
	tb.Fatalf("label %q not found in %#v", name, labels)
	return nil
}
