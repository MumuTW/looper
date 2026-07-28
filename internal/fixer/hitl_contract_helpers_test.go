package fixer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/storage"
)

// PR #87-class scenario: PR intent hard-codes RollingUpdate; reviewer asks to
// restore a configurable strategy. Fixer with HITL must escalate, not mutate.

const (
	pr87Title        = "Hard-code deployment strategy to RollingUpdate"
	pr87Body         = "Product decision: strategy is fixed to RollingUpdate. Do not make it configurable."
	pr87ReviewerBody = "Please restore the configurable deployment strategy so operators can choose."
	pr87Head         = "head-pr87"
	pr87Base         = "base-pr87"
)

// hitlScriptedAgent records starts and optionally writes ask/dismiss sentinels
// into the worktree before returning a scripted AgentResult.
type hitlScriptedAgent struct {
	results      []AgentResult
	starts       []AgentRunInput
	writeAskJSON bool
	writeDismiss bool
	askQuestion  string
	askOptions   []string
}

func (a *hitlScriptedAgent) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	a.starts = append(a.starts, input)
	if a.writeAskJSON || a.writeDismiss {
		looperDir := filepath.Join(input.WorkingDirectory, ".looper")
		_ = os.MkdirAll(looperDir, 0o755)
		if a.writeAskJSON {
			q := a.askQuestion
			if q == "" {
				q = "Keep hard-coded RollingUpdate or restore configurable strategy?"
			}
			opts := a.askOptions
			if len(opts) == 0 {
				opts = []string{"keep RollingUpdate (PR intent)", "restore configurable strategy (reviewer)"}
			}
			payload := fmt.Sprintf(`{"question":%q,"options":[%q,%q],"recommendation":"Keep RollingUpdate per PR body.","recommendedOption":%q,"confidence":"high"}`,
				q, opts[0], opts[1], opts[0])
			if err := os.WriteFile(filepath.Join(looperDir, "ask.json"), []byte(payload), 0o644); err != nil {
				return nil, err
			}
		}
		if a.writeDismiss {
			if err := os.WriteFile(filepath.Join(looperDir, "dismiss.json"), []byte(`{"dismissals":[{"reviewer":"reviewer","reason":"conflicts with PR intent"}]}`), 0o644); err != nil {
				return nil, err
			}
		}
	}
	if len(a.results) == 0 {
		return nil, fmt.Errorf("no queued agent result")
	}
	result := a.results[0]
	a.results = a.results[1:]
	return fakeAgentExecution{result: result}, nil
}

func pr87NeedsHumanStdout(fixItemID, threadID string) string {
	return fmt.Sprintf(
		`__LOOPER_RESULT__={"summary":"unresolvable conflict with PR intent","review_thread_replies":[{"fixItemId":%q,"threadId":%q,"action":"needs_human","explanation":"Reviewer wants configurable strategy but PR title/body hard-code RollingUpdate; needs human call.","threadCommentsObserved":"abc"}]}`+"\n",
		fixItemID, threadID,
	)
}

func pr87Detail(head string) PullRequestDetail {
	return PullRequestDetail{
		Number: 87, Title: pr87Title, Body: pr87Body, State: "OPEN",
		HeadSHA: head, HeadRefName: "feature/pr87", BaseRefName: "main", BaseSHA: pr87Base,
		Comments: []map[string]any{
			{"id": "c-strategy", "threadId": "t-strategy", "body": pr87ReviewerBody, "author": "reviewer"},
		},
	}
}

func seedPR87Queue(t *testing.T, fixture *runnerFixture, github *fakeGitHubGateway, git *fakeGitGateway, agent AgentExecutor, hitlOn bool) (*Runner, storage.QueueItemRecord) {
	t.Helper()
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		GitHub: github, Git: git, AgentExecutor: agent,
		ValidationRunner: passValidation,
		AllowAutoCommit:  true, AllowAutoPush: true, AllowRiskyFixes: true,
		Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: hitlOn, HITLAnswerTransport: "feishu",
		HITLNotify: func(context.Context, HITLAskNotification) error { return nil },
	})
	if _, err := runner.DiscoverPullRequests(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverPullRequests() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "fixer-hitl-worker", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	return runner, *claim
}
