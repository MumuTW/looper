package e2e

import "testing"

type forgejoMirrorIntent string

const (
	forgejoMirrorRun           forgejoMirrorIntent = "run"
	forgejoMirrorSkip          forgejoMirrorIntent = "skip"
	forgejoMirrorNoCounterpart forgejoMirrorIntent = "no-counterpart"
)

type forgejoMirrorCase struct {
	GitHubCase  string
	ForgejoCase string
	Intent      forgejoMirrorIntent
	Reason      string
}

var forgejoSandboxMirrorCases = []forgejoMirrorCase{
	{GitHubCase: "TestGitHubSandboxWorkerCreatesPullRequest", ForgejoCase: "TestForgejoSandboxWorkerCreatesPullRequest", Intent: forgejoMirrorRun, Reason: "Forgejo supports issues, pull requests, labels, comments, and diffs"},
	{GitHubCase: "TestGitHubSandboxFixerResolvesReviewThread", ForgejoCase: "TestForgejoSandboxFixerResolvesReviewThread", Intent: forgejoMirrorRun, Reason: "Forgejo validates reviewer/fixer summary comments without native review thread resolution"},
	{GitHubCase: "TestGitHubSandboxNoDiffPathsDoNotOpenOrResolve/worker-no-diff-no-pr", ForgejoCase: "TestForgejoSandboxNoDiffPathsDoNotOpenOrResolve/worker-no-diff-no-pr", Intent: forgejoMirrorRun, Reason: "worker no-diff uses supported Forgejo issue/PR/diff behavior"},
	{GitHubCase: "TestGitHubSandboxNoDiffPathsDoNotOpenOrResolve/fixer-no-new-commit-keeps-thread-unresolved", ForgejoCase: "TestForgejoSandboxNoDiffPathsDoNotOpenOrResolve/fixer-no-new-commit-keeps-thread-unresolved", Intent: forgejoMirrorSkip, Reason: "Forgejo summary-protocol coverage runs in TestForgejoSandboxFixerResolvesReviewThread instead of native thread resolution"},
	{GitHubCase: "TestGitHubSandboxDependencyGateScenarios/looperd startup validation succeeds against real dependency API", ForgejoCase: "TestForgejoSandboxDependencyGateScenarios/looperd startup validation succeeds against real dependency API", Intent: forgejoMirrorSkip, Reason: "Forgejo MVP does not support Coordinator/dependency-gate behavior"},
	{GitHubCase: "TestGitHubSandboxDependencyGateScenarios/human gated blocked_by fails then releases after completion", ForgejoCase: "TestForgejoSandboxDependencyGateScenarios/human gated blocked_by fails then releases after completion", Intent: forgejoMirrorSkip, Reason: "Forgejo MVP does not support Coordinator/dependency-gate behavior"},
	{GitHubCase: "TestGitHubSandboxDependencyGateScenarios/GitHub rejects blocked_by cycle creation", ForgejoCase: "TestForgejoSandboxDependencyGateScenarios/Forgejo rejects blocked_by cycle creation", Intent: forgejoMirrorSkip, Reason: "Forgejo MVP does not support Coordinator/dependency-gate behavior"},
	{GitHubCase: "TestGitHubSandboxDependencyGateScenarios/not planned blocker returns dependent to retriage without cycle comment", ForgejoCase: "TestForgejoSandboxDependencyGateScenarios/not planned blocker returns dependent to retriage without cycle comment", Intent: forgejoMirrorSkip, Reason: "Forgejo MVP does not support Coordinator/dependency-gate behavior"},
}

func TestForgejoSandboxMirrorInventory(t *testing.T) {
	assertForgejoMirrorInventory(t, forgejoSandboxMirrorCases)
}

func assertForgejoMirrorInventory(t *testing.T, cases []forgejoMirrorCase) {
	t.Helper()
	seen := map[string]bool{}
	for _, tc := range cases {
		if tc.GitHubCase == "" || tc.ForgejoCase == "" || tc.Reason == "" {
			t.Fatalf("incomplete mirror case: %#v", tc)
		}
		switch tc.Intent {
		case forgejoMirrorRun, forgejoMirrorSkip, forgejoMirrorNoCounterpart:
		default:
			t.Fatalf("invalid mirror intent for %s: %q", tc.GitHubCase, tc.Intent)
		}
		if seen[tc.GitHubCase] {
			t.Fatalf("duplicate GitHub mirror case %q", tc.GitHubCase)
		}
		seen[tc.GitHubCase] = true
	}
}
