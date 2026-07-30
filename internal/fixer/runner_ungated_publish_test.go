package fixer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestUngatedRepairStillPublishesThroughLooper covers the configuration this change
// alters: auto-push on with no validation gate configured. That flow used to rely on
// the agent pushing during the repair, which let a partial fix reach the PR before
// the run's outcome existed. The agent is now local-only in every configuration, so
// the guarantee that has to hold is that Looper's own push step still publishes —
// otherwise removing the instruction would simply stop this flow from shipping.
func TestUngatedRepairStillPublishesThroughLooper(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := PullRequestDetail{
		Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42",
		BaseRefName: "main", BaseSHA: "base-1",
		Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}},
	}
	pushed := detail
	pushed.HeadSHA = "new-head"
	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{detail, detail, pushed, pushed, pushed},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
		inspectResults: []InspectHeadResult{
			{HeadSHA: "base-head"},
			{HeadSHA: "new-head", NewCommitSHAs: []string{"new-head"}},
			{HeadSHA: "new-head"},
		},
	}
	stdout := fmt.Sprintf(`__LOOPER_RESULT__={"outcome":"completed","summary":"applied fixes","review_thread_replies":[{"fixItemId":"c1","threadId":"t1","explanation":"Adjusted the off-by-one handling and verified the fix.","threadCommentsObserved":"%s"}]}`+"\n", hashReviewThreadComments(ReviewThread{Comments: []ReviewThreadComment{{ID: "c1"}}}))
	agentExec := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "applied fixes", ParseStatus: "parsed", Stdout: stdout}}}
	// No ValidationCommands: this is the previously agent-published configuration.
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agentExec, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now})

	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-worker-1", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	// Looper published, not the agent.
	if len(git.pushCalls) != 1 {
		t.Fatalf("git push calls = %d, want 1 from Looper's push step", len(git.pushCalls))
	}
	if len(agentExec.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agentExec.starts))
	}
	if strings.Contains(agentExec.starts[0].Prompt, "Commit and push") {
		t.Fatalf("ungated repair prompt still instructs the agent to push:\n%s", agentExec.starts[0].Prompt)
	}
	if !strings.Contains(agentExec.starts[0].Prompt, "Do not push the branch") {
		t.Fatalf("ungated repair prompt lacks the local-only instruction:\n%s", agentExec.starts[0].Prompt)
	}
}
