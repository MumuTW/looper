package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/labels"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestConsumeAskSentinelReadsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	path := filepath.Join(dir, ".looper", "ask.json")
	if err := os.WriteFile(path, []byte(`{"question":"Redis or Postgres?","options":["redis","postgres"]}`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	ask, err := consumeAskSentinel(dir)
	if err != nil {
		t.Fatalf("consumeAskSentinel error = %v", err)
	}
	if ask == nil || ask.Question != "Redis or Postgres?" || len(ask.Options) != 2 {
		t.Fatalf("ask = %#v, want the parsed question + 2 options", ask)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("sentinel file must be removed after consumption")
	}

	// No sentinel -> nil, nil (both a missing file and an absent .looper dir).
	if again, err := consumeAskSentinel(dir); err != nil || again != nil {
		t.Fatalf("second consumeAskSentinel = (%#v, %v), want (nil, nil)", again, err)
	}
	if missing, err := consumeAskSentinel(t.TempDir()); err != nil || missing != nil {
		t.Fatalf("consumeAskSentinel(empty dir) = (%#v, %v), want (nil, nil)", missing, err)
	}
}

// appendingAgentExecutor simulates a human message arriving while the agent is
// running. Start appends it to the fresh loop metadata before completing.
type appendingAgentExecutor struct {
	inner  fakeAgentExecutor
	repos  *storage.Repositories
	loopID string
	text   string
	at     string
	ask    string
}

func (a *appendingAgentExecutor) Start(ctx context.Context, input AgentRunInput) (AgentExecution, error) {
	if a.text != "" {
		loop, err := a.repos.Loops.GetByID(ctx, a.loopID)
		if err != nil {
			return nil, err
		}
		if loop == nil {
			return nil, fmt.Errorf("loop not found: %s", a.loopID)
		}
		meta, err := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: a.at, Text: a.text})
		if err != nil {
			return nil, err
		}
		loop.MetadataJSON = &meta
		if err := a.repos.Loops.Upsert(ctx, *loop); err != nil {
			return nil, err
		}
	}
	if a.ask != "" {
		askPath := filepath.Join(input.WorkingDirectory, hitlSentinelRelPath)
		if err := os.MkdirAll(filepath.Dir(askPath), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(askPath, []byte(a.ask), 0o644); err != nil {
			return nil, err
		}
	}
	return a.inner.Start(ctx, input)
}

func TestWorkerDoesNotAttachOldVendorSessionsAfterVendorChange(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}

	sessionID := "codex-session-123"
	metadata, err := loops.WriteHITLAsk(loop.MetadataJSON, loops.HITLAsk{
		Question:  "Which datastore?",
		SessionID: sessionID,
		Vendor:    "codex",
		Answer:    "postgres",
		Status:    "answered",
	})
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	metadata, err = loops.WriteTakeoverResume(&metadata, loops.TakeoverResume{SessionID: sessionID})
	if err != nil {
		t.Fatalf("WriteTakeoverResume() error = %v", err)
	}
	loop.MetadataJSON = &metadata
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := loop.ProjectID
	loopID := loop.ID
	if err := fixture.repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{
		ID:              "agent_old_vendor",
		ProjectID:       &projectID,
		LoopID:          &loopID,
		Vendor:          "codex",
		Status:          "completed",
		NativeSessionID: &sessionID,
		StartedAt:       fixture.nowISO(),
		CreatedAt:       fixture.nowISO(),
		UpdatedAt:       fixture.nowISO(),
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	sameVendor := New(Options{Repos: fixture.repos, AgentRuntime: "codex"})
	if prompt, gotSession := sameVendor.pendingHumanAnswer(ctx, loop, "codex"); !strings.Contains(prompt, "postgres") || gotSession != sessionID {
		t.Fatalf("same-vendor HITL resume = (%q, %q), want decision prompt + %q", prompt, gotSession, sessionID)
	}
	if prompt, gotSession := sameVendor.pendingTakeoverResume(ctx, loop, "codex"); prompt == "" || gotSession != sessionID {
		t.Fatalf("same-vendor takeover resume = (%q, %q), want prompt + %q", prompt, gotSession, sessionID)
	}
	if got := sameVendor.latestNativeSessionID(ctx, loop.ID, "codex"); got != sessionID {
		t.Fatalf("same-vendor mailbox session = %q, want %q", got, sessionID)
	}

	newVendor := New(Options{Repos: fixture.repos, AgentRuntime: "claude-code"})
	if prompt, gotSession := newVendor.pendingHumanAnswer(ctx, loop, "claude-code"); !strings.Contains(prompt, "postgres") || !strings.Contains(prompt, "vendor changed") || gotSession != "" {
		t.Fatalf("cross-vendor HITL resume = (%q, %q), want decision prompt + fresh session", prompt, gotSession)
	}
	if prompt, gotSession := newVendor.pendingTakeoverResume(ctx, loop, "claude-code"); !strings.Contains(prompt, "fresh session") || gotSession != "" {
		t.Fatalf("cross-vendor takeover resume = (%q, %q), want checkpoint prompt + fresh session", prompt, gotSession)
	}
	if got := newVendor.latestNativeSessionID(ctx, loop.ID, "claude-code"); got != "" {
		t.Fatalf("cross-vendor mailbox session = %q, want fresh session", got)
	}
}

func TestWorkerInboxAcknowledgementPreservesConcurrentMessageForNextTurn(t *testing.T) {
	for _, initialMessages := range []int{0, 20} {
		t.Run("initial_inbox_"+strconv.Itoa(initialMessages), func(t *testing.T) {
			fixture := newRunnerFixture(t)
			ctx := context.Background()
			loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
			if err != nil || loop == nil {
				t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
			}
			for i := 0; i < initialMessages; i++ {
				meta, appendErr := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: "before", Text: fmt.Sprintf("before-%d", i)})
				if appendErr != nil {
					t.Fatalf("AppendHumanMessage() error = %v", appendErr)
				}
				loop.MetadataJSON = &meta
			}
			if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}

			agent := &blockingAgentExecutor{started: make(chan struct{}), release: make(chan struct{})}
			runner := New(Options{
				DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now,
				GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: t.TempDir(), Branch: "looper/inbox", BaseBranch: "main"}},
				AgentExecutor: agent, HITLEnabled: true, AllowAutoCommit: true,
			})
			claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
			if err != nil || claim == nil {
				t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
			}
			finished := make(chan error, 1)
			go func() {
				_, runErr := runner.ProcessClaimedItem(ctx, *claim)
				finished <- runErr
			}()
			<-agent.started

			appendHumanMessageForWorkerTest(t, ctx, fixture.repos, fixture.nowISO(), "loop_worker_1", "late instruction")
			close(agent.release)
			if err := <-finished; err != nil {
				t.Fatalf("ProcessClaimedItem(first) error = %v", err)
			}

			loop, err = fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
			if err != nil || loop == nil || loop.Status != "queued" {
				t.Fatalf("loop after first turn = (%#v, %v), want queued", loop, err)
			}
			inbox := loops.ReadHumanInbox(loop.MetadataJSON)
			if len(inbox) != 1 || inbox[0].Text != "late instruction" {
				t.Fatalf("inbox after first turn = %#v, want only late instruction", inbox)
			}
			queue, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
			if err != nil || queue == nil || queue.Status != "queued" {
				t.Fatalf("queue after first turn = (%#v, %v), want queued", queue, err)
			}

			next, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-2", "worker")
			if err != nil || next == nil {
				t.Fatalf("ClaimNextOfType(next) = (%#v, %v)", next, err)
			}
			if _, err := runner.ProcessClaimedItem(ctx, *next); err != nil {
				t.Fatalf("ProcessClaimedItem(next) error = %v", err)
			}
			if len(agent.starts) != 2 || !strings.Contains(agent.starts[1].Prompt, "late instruction") {
				t.Fatalf("agent starts = %#v, want second prompt containing late instruction", agent.starts)
			}
		})
	}
}

func TestSuspendForHumanAcknowledgesOnlyPromptInboxSnapshot(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	meta, err := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: "before", Text: "prompt message"})
	if err != nil {
		t.Fatalf("AppendHumanMessage() error = %v", err)
	}
	loop.MetadataJSON = &meta
	loop.Status = "running"
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	appendHumanMessageForWorkerTest(t, ctx, fixture.repos, nowISO, loop.ID, "late message")

	queue, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || queue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", queue, err)
	}
	queue.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	run := storage.RunRecord{ID: "run_suspend_inbox", LoopID: loop.ID, Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, loop.ProjectID)
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, HITLEnabled: true, HITLAnswerTransport: "feishu"})
	if _, err := runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: *queue}, run, workerCheckpoint{}, &awaitingHumanError{question: "Continue?"}); err != nil {
		t.Fatalf("suspendForHuman() error = %v", err)
	}
	updated, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || updated == nil {
		t.Fatalf("Loops.GetByID() after suspend = (%#v, %v)", updated, err)
	}
	inbox := loops.ReadHumanInbox(updated.MetadataJSON)
	if len(inbox) != 1 || inbox[0].Text != "late message" {
		t.Fatalf("inbox after suspend = %#v, want only late message", inbox)
	}
}

func appendHumanMessageForWorkerTest(t *testing.T, ctx context.Context, repos *storage.Repositories, nowISO, loopID, text string) {
	t.Helper()
	unlock := loops.LockLoopRequeue(loopID)
	defer unlock()
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	meta, err := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: nowISO, Text: text})
	if err != nil {
		t.Fatalf("AppendHumanMessage() error = %v", err)
	}
	loop.MetadataJSON = &meta
	loop.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
}

type blockingAgentExecutor struct {
	started chan struct{}
	release chan struct{}
	starts  []AgentRunInput
}

func (a *blockingAgentExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	a.starts = append(a.starts, input)
	if len(a.starts) == 1 {
		close(a.started)
		return blockingAgentExecution{release: a.release}, nil
	}
	return fakeAgentExecution{result: AgentResult{Status: "completed", Summary: "done", ParseStatus: "parsed"}}, nil
}

type blockingAgentExecution struct{ release <-chan struct{} }

func (a blockingAgentExecution) Wait(context.Context) (AgentResult, error) {
	<-a.release
	return AgentResult{Status: "completed", Summary: "done", ParseStatus: "parsed"}, nil
}

func (blockingAgentExecution) Kill(string) error { return nil }

func TestSuspendForHumanTransitionsAndNotifies(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	// Move the seeded loop + queue item into an in-flight state.
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	loop.Status = "running"
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	queueItem.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{ID: "run_worker_1", LoopID: "loop_worker_1", Status: "running", CurrentStep: stringPtr("execute"), LastCompletedStep: stringPtr("plan"), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	var sent []HITLAskNotification
	runner := New(Options{
		DB:          fixture.coordinator.DB(),
		Repos:       fixture.repos,
		Logger:      fixture.logger,
		Now:         fixture.now,
		HITLEnabled: true,
		// This test exercises the Feishu-notify transport; github is the default and
		// is covered separately.
		HITLAnswerTransport: "feishu",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			sent = append(sent, n)
			return nil
		},
	})

	awaiting := &awaitingHumanError{question: "Which datastore?", options: []string{"redis", "postgres"}, sessionID: "sess-xyz", executionID: "agent-1", vendor: "codex"}
	result, err := runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: *queueItem}, run, workerCheckpoint{}, awaiting)
	if err != nil {
		t.Fatalf("suspendForHuman error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	// Loop suspended + ask persisted.
	got, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if got.Status != "awaiting_human" {
		t.Fatalf("loop status = %q, want awaiting_human", got.Status)
	}
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Question != "Which datastore?" || ask.SessionID != "sess-xyz" || ask.Status != "awaiting" {
		t.Fatalf("persisted ask = %#v (ok=%v), want the question + session + awaiting", ask, ok)
	}

	// Queue item cancelled so the scheduler stops retrying (resume requeues it).
	q, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || q == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if q.Status != "cancelled" {
		t.Fatalf("queue status = %q, want cancelled", q.Status)
	}

	// Run ended as interrupted (resumable from checkpoint).
	finishedRun, err := fixture.repos.Runs.GetByID(ctx, "run_worker_1")
	if err != nil || finishedRun == nil {
		t.Fatalf("Runs.GetByID error = %v", err)
	}
	if finishedRun.Status != "interrupted" {
		t.Fatalf("run status = %q, want interrupted", finishedRun.Status)
	}

	// Ask-card sent.
	if len(sent) != 1 {
		t.Fatalf("HITLNotify calls = %d, want 1", len(sent))
	}
	if sent[0].LoopSeq != 1 || sent[0].Question != "Which datastore?" || len(sent[0].Options) != 2 {
		t.Fatalf("notification = %#v, want loop seq 1 + question + 2 options", sent[0])
	}
}

func TestSuspendForHumanDeliversAskToGitHub(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	loop.Status = "running"
	pr := int64(42)
	loop.PRNumber = &pr // already has a PR, so no draft-PR git push is needed
	repo := "acme/widgets"
	loop.Repo = &repo
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	queueItem.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{ID: "run_worker_1", LoopID: "loop_worker_1", Status: "running", CurrentStep: stringPtr("execute"), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	gh := &fakeGitHubGateway{issueCommentResult: IssueCommentResult{ID: 777}}
	runner := New(Options{
		DB:          fixture.coordinator.DB(),
		Repos:       fixture.repos,
		Logger:      fixture.logger,
		Now:         fixture.now,
		HITLEnabled: true,
		// github is the default transport; be explicit for the test's intent.
		HITLAnswerTransport: "github",
		HITLGitHub:          HITLGitHubSettings{AwaitingLabel: labels.AwaitingHuman, MentionLogins: []string{"lefarcen"}},
		GitHub:              gh,
	})

	awaiting := &awaitingHumanError{question: "Redis or Postgres?", options: []string{"redis", "postgres"}, sessionID: "sess-1", vendor: "codex"}
	loopForStep, _ := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	result, err := runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loopForStep, Run: run, QueueItem: *queueItem}, run, workerCheckpoint{PullRequest: &checkpointPullPR{Number: 42}}, awaiting)
	if err != nil {
		t.Fatalf("suspendForHuman error = %v", err)
	}
	if result.Status != "awaiting_human" {
		t.Fatalf("result.Status = %q, want awaiting_human", result.Status)
	}

	// Posted the ask as a PR comment carrying the marker + question.
	if len(gh.createIssueCommentCalls) != 1 {
		t.Fatalf("CreateIssueComment calls = %d, want 1", len(gh.createIssueCommentCalls))
	}
	c := gh.createIssueCommentCalls[0]
	if c.IssueNumber != 42 || c.Repo != "acme/widgets" {
		t.Fatalf("comment target = %s#%d, want acme/widgets#42", c.Repo, c.IssueNumber)
	}
	if !containsAll(c.Body, hitlGitHubAskMarkerPrefix, "Redis or Postgres?", "@lefarcen") {
		t.Fatalf("comment body missing marker/question/mention: %s", c.Body)
	}
	// Labelled awaiting-human.
	if len(gh.addLabels) != 1 || len(gh.addLabels[0].Labels) != 1 || gh.addLabels[0].Labels[0] != labels.AwaitingHuman {
		t.Fatalf("addLabels = %#v, want one looper:awaiting-human on the PR", gh.addLabels)
	}
	// Ask metadata records the github correlation.
	got, _ := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	ask, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || ask.Transport != "github" || ask.PRNumber != 42 || ask.AskCommentID != 777 {
		t.Fatalf("ask = %#v, want github transport + pr 42 + comment 777", ask)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestEnsureDraftPRForAskValidatesAndPinsPublishedHead(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	git := &fakeGitGateway{inspectResults: []InspectHeadResult{
		{HeadSHA: "validated-head", NewCommitSHAs: []string{"validated-head"}},
		{HeadSHA: "validated-head", NewCommitSHAs: []string{"validated-head"}},
		{HeadSHA: "validated-head", NewCommitSHAs: []string{"validated-head"}},
	}}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 42, URL: "https://example/pr/42"}}
	runner := New(Options{
		Git: git, GitHub: github,
		ValidationCommands: []string{"make check"},
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
		},
	})
	checkpoint := workerCheckpoint{
		Work:     &workerInput{Repo: "acme/widgets", Branch: "looper/feature", BaseBranch: "main", Title: "Feature"},
		Worktree: &checkpointWorktree{Path: worktree, Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head"},
	}
	input := stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: worktree}, Loop: storage.LoopRecord{ID: "loop_1"}}

	number, err := runner.ensureDraftPRForAsk(context.Background(), input, &checkpoint, "acme/widgets", worktree)
	if err != nil {
		t.Fatalf("ensureDraftPRForAsk() error = %v", err)
	}
	if number != 42 || len(git.pushCalls) != 1 || git.pushCalls[0].LocalHeadSHA != "validated-head" {
		t.Fatalf("draft publish = number %d pushes %#v, want PR 42 pinned to validated-head", number, git.pushCalls)
	}
	if checkpoint.Validation == nil || !checkpoint.Validation.Passed || checkpoint.Validation.HeadSHA != "validated-head" {
		t.Fatalf("checkpoint.Validation = %#v, want validated-head", checkpoint.Validation)
	}
}

func TestEnsureDraftPRForAskDoesNotPublishFailedValidation(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	git := &fakeGitGateway{}
	github := &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 42}}
	runner := New(Options{
		Git: git, GitHub: github,
		ValidationCommands: []string{"make check"},
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			return ValidationResult{Passed: false, Summary: "Validation failed"}, nil
		},
	})
	checkpoint := workerCheckpoint{
		Work:     &workerInput{Repo: "acme/widgets", Branch: "looper/feature", BaseBranch: "main", Title: "Feature"},
		Worktree: &checkpointWorktree{Path: worktree, Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head"},
	}
	input := stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: worktree}, Loop: storage.LoopRecord{ID: "loop_1"}}

	if _, err := runner.ensureDraftPRForAsk(context.Background(), input, &checkpoint, "acme/widgets", worktree); err == nil {
		t.Fatal("ensureDraftPRForAsk() error = nil, want validation failure")
	}
	if len(git.pushCalls) != 0 || len(github.createPRCalls) != 0 {
		t.Fatalf("failed validation published draft: pushes=%d prs=%d", len(git.pushCalls), len(github.createPRCalls))
	}
}

func TestConsumeAskSentinelFailsClosedAndQuarantines(t *testing.T) {
	t.Parallel()

	writeSentinel := func(t *testing.T, content string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".looper"), 0o755); err != nil {
			t.Fatalf("MkdirAll error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, hitlSentinelRelPath), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		return dir
	}
	quarantinePathFromError := func(t *testing.T, err error) string {
		t.Helper()
		matches := regexp.MustCompile(`evidence quarantined at (\S+)`).FindStringSubmatch(err.Error())
		if len(matches) != 2 {
			t.Fatalf("error = %v, want it to name the quarantined path", err)
		}
		return matches[1]
	}
	assertQuarantined := func(t *testing.T, dir string, err error, wantContent string) string {
		t.Helper()
		quarantined := quarantinePathFromError(t, err)
		if strings.HasPrefix(quarantined, dir) {
			t.Fatalf("quarantine path %s is inside the worktree %s; auto-commit reconciliation would commit it", quarantined, dir)
		}
		raw, readErr := os.ReadFile(quarantined)
		if readErr != nil {
			t.Fatalf("quarantined evidence unreadable: %v", readErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(quarantined)) })
		if string(raw) != wantContent {
			t.Fatalf("quarantined evidence = %q, want original content preserved", raw)
		}
		if _, statErr := os.Stat(filepath.Join(dir, hitlSentinelRelPath)); !os.IsNotExist(statErr) {
			t.Fatal("original sentinel still present; quarantine must move it out of the consume path")
		}
		return quarantined
	}

	t.Run("truncated JSON fails closed with evidence quarantined", func(t *testing.T) {
		dir := writeSentinel(t, `{"question":"delete prod?`)
		ask, err := consumeAskSentinel(dir)
		if err == nil || ask != nil {
			t.Fatalf("consumeAskSentinel = (%#v, %v), want fail-closed error", ask, err)
		}
		if !strings.Contains(err.Error(), "not valid JSON") {
			t.Fatalf("error = %v, want JSON diagnostic", err)
		}
		assertQuarantined(t, dir, err, `{"question":"delete prod?`)
	})

	t.Run("schema without question fails closed", func(t *testing.T) {
		dir := writeSentinel(t, `{"options":["a","b"]}`)
		ask, err := consumeAskSentinel(dir)
		if err == nil || ask != nil {
			t.Fatalf("consumeAskSentinel = (%#v, %v), want fail-closed error", ask, err)
		}
		if !strings.Contains(err.Error(), "no question") {
			t.Fatalf("error = %v, want no-question diagnostic", err)
		}
		assertQuarantined(t, dir, err, `{"options":["a","b"]}`)
	})

	t.Run("unreadable sentinel fails closed without deleting evidence", func(t *testing.T) {
		dir := writeSentinel(t, `{"question":"q"}`)
		path := filepath.Join(dir, hitlSentinelRelPath)
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatalf("Chmod error = %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		ask, err := consumeAskSentinel(dir)
		if err == nil || ask != nil {
			t.Fatalf("consumeAskSentinel = (%#v, %v), want fail-closed error", ask, err)
		}
		if !strings.Contains(err.Error(), "cannot be read") {
			t.Fatalf("error = %v, want read diagnostic", err)
		}
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("evidence missing after read failure: %v", statErr)
		}
	})

	t.Run("oversized sentinel fails closed", func(t *testing.T) {
		dir := writeSentinel(t, `{"question":"`+strings.Repeat("x", maxAskSentinelBytes)+`"}`)
		ask, err := consumeAskSentinel(dir)
		if err == nil || ask != nil {
			t.Fatalf("consumeAskSentinel = (%#v, %v), want fail-closed error", ask, err)
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Fatalf("error = %v, want size diagnostic", err)
		}
	})

	t.Run("repeated malformed asks preserve every piece of evidence", func(t *testing.T) {
		dir := writeSentinel(t, `{"question":"first attempt`)
		_, err1 := consumeAskSentinel(dir)
		if err1 == nil {
			t.Fatal("first consumeAskSentinel error = nil, want fail-closed error")
		}
		first := assertQuarantined(t, dir, err1, `{"question":"first attempt`)

		if err := os.WriteFile(filepath.Join(dir, hitlSentinelRelPath), []byte(`{"question":"second attempt`), 0o644); err != nil {
			t.Fatalf("WriteFile(second) error = %v", err)
		}
		_, err2 := consumeAskSentinel(dir)
		if err2 == nil {
			t.Fatal("second consumeAskSentinel error = nil, want fail-closed error")
		}
		second := assertQuarantined(t, dir, err2, `{"question":"second attempt`)
		if first == second {
			t.Fatalf("both quarantines landed on %s; earlier evidence was clobbered", first)
		}
		if raw, err := os.ReadFile(first); err != nil || string(raw) != `{"question":"first attempt` {
			t.Fatalf("first evidence after second quarantine = (%q, %v), want preserved", raw, err)
		}
	})

	t.Run("valid ask still consumed, absent still no-question", func(t *testing.T) {
		dir := writeSentinel(t, `{"question":"Redis or Postgres?","options":["redis","postgres"]}`)
		ask, err := consumeAskSentinel(dir)
		if err != nil || ask == nil || ask.Question != "Redis or Postgres?" {
			t.Fatalf("consumeAskSentinel = (%#v, %v), want the parsed ask", ask, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, hitlSentinelRelPath)); !os.IsNotExist(statErr) {
			t.Fatal("valid sentinel must be consumed")
		}
		if again, err := consumeAskSentinel(dir); err != nil || again != nil {
			t.Fatalf("second consumeAskSentinel = (%#v, %v), want (nil, nil)", again, err)
		}
	})
}

func TestDetectHumanAskParksOnSentinelFailure(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, HITLEnabled: true})
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".looper"), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, hitlSentinelRelPath), []byte(`{"question":"truncated`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	_, awaitErr := runner.detectHumanAsk(context.Background(), stepInput{Loop: *loop}, dir, "exec-1")
	if awaitErr == nil {
		t.Fatal("detectHumanAsk error = nil, want parked gate failure")
	}
	var le *loopError
	if !errors.As(awaitErr, &le) || le.kind != FailureManualIntervention {
		t.Fatalf("detectHumanAsk error = %#v, want loopError with manual_intervention: a retryable kind auto-requeues and the retry, finding the sentinel quarantined, would publish the gated work", awaitErr)
	}
}

func TestSuspendForHumanAbortsOnMalformedMetadataInsteadOfStranding(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	nowISO := fixture.nowISO()

	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	malformed := `{"worktreeId":"wt-1","hitl":{`
	loop.Status = "running"
	loop.MetadataJSON = &malformed
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert error = %v", err)
	}
	queueItem, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	queueItem.Status = "running"
	if err := fixture.repos.Queue.Upsert(ctx, *queueItem); err != nil {
		t.Fatalf("Queue.Upsert error = %v", err)
	}
	run := storage.RunRecord{ID: "run_worker_1", LoopID: "loop_worker_1", Status: "running", StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatalf("Runs.Upsert error = %v", err)
	}
	project, err := fixture.repos.Projects.GetByID(ctx, "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID error = %v", err)
	}

	runner := New(Options{
		DB:                  fixture.coordinator.DB(),
		Repos:               fixture.repos,
		Logger:              fixture.logger,
		Now:                 fixture.now,
		HITLEnabled:         true,
		HITLAnswerTransport: "feishu",
		HITLNotify: func(_ context.Context, n HITLAskNotification) error {
			return nil
		},
	})

	awaiting := &awaitingHumanError{question: "Which datastore?", sessionID: "sess-xyz", executionID: "agent-1", vendor: "codex"}
	_, err = runner.suspendForHuman(ctx, stepInput{Project: *project, Loop: *loop, Run: run, QueueItem: *queueItem}, run, workerCheckpoint{}, awaiting)
	if err == nil {
		t.Fatal("suspendForHuman error = nil, want persist-ask failure: parking without a stored ask strands the loop")
	}
	if !errors.Is(err, loops.ErrMalformedLoopMetadata) {
		t.Fatalf("suspendForHuman error = %v, want ErrMalformedLoopMetadata", err)
	}

	// The loop must not have transitioned to awaiting_human, its malformed
	// metadata must be intact for diagnosis, and the queue item untouched.
	got, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID error = %v", err)
	}
	if got.Status == "awaiting_human" {
		t.Fatal("loop status = awaiting_human, want suspension aborted")
	}
	if got.MetadataJSON == nil || *got.MetadataJSON != malformed {
		t.Fatalf("loop metadata = %v, want the malformed value preserved verbatim", got.MetadataJSON)
	}
	q, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
	if err != nil || q == nil {
		t.Fatalf("Queue.GetByID error = %v", err)
	}
	if q.Status != "running" {
		t.Fatalf("queue item status = %q, want running (not cancelled)", q.Status)
	}

	// The run must be closed as failed so the retried claim resumes it instead
	// of starting a duplicate while this one stays active forever.
	gotRun, err := fixture.repos.Runs.GetByID(ctx, "run_worker_1")
	if err != nil || gotRun == nil {
		t.Fatalf("Runs.GetByID error = %v", err)
	}
	if gotRun.Status != "failed" {
		t.Fatalf("run status = %q, want failed after aborted suspension", gotRun.Status)
	}
}

func TestMergeLoopMetadataJSONRejectsMalformedCurrentValue(t *testing.T) {
	t.Parallel()

	malformed := `{"prUrl":`
	got, err := mergeLoopMetadataJSON(&malformed, map[string]any{"prUrl": "https://example.com/pr/1"})
	if err == nil {
		t.Fatalf("mergeLoopMetadataJSON(malformed) = %q, want error: merging over a corrupt value erases durable state", got)
	}
	if !errors.Is(err, loops.ErrMalformedLoopMetadata) {
		t.Fatalf("mergeLoopMetadataJSON(malformed) error = %v, want ErrMalformedLoopMetadata", err)
	}

	valid := `{"worker":{"lastRun":"r1"}}`
	out, err := mergeLoopMetadataJSON(&valid, map[string]any{"prUrl": "https://example.com/pr/1"})
	if err != nil {
		t.Fatalf("mergeLoopMetadataJSON(valid) error = %v", err)
	}
	if !strings.Contains(out, `"worker"`) || !strings.Contains(out, `"prUrl"`) {
		t.Fatalf("mergeLoopMetadataJSON(valid) = %q, want existing keys preserved and update applied", out)
	}
}

// corruptingAgentExecutor writes the ask sentinel into the worktree and
// corrupts the loop's stored metadata mid-run, simulating concurrent
// corruption that lands between claim time and suspension.
type corruptingAgentExecutor struct {
	inner   fakeAgentExecutor
	repos   *storage.Repositories
	loopID  string
	corrupt string
}

func (c *corruptingAgentExecutor) Start(ctx context.Context, input AgentRunInput) (AgentExecution, error) {
	askPath := filepath.Join(input.WorkingDirectory, hitlSentinelRelPath)
	if err := os.MkdirAll(filepath.Dir(askPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(askPath, []byte(`{"question":"Which datastore?"}`), 0o644); err != nil {
		return nil, err
	}
	loop, err := c.repos.Loops.GetByID(ctx, c.loopID)
	if err != nil || loop == nil {
		return nil, err
	}
	corrupted := c.corrupt
	loop.MetadataJSON = &corrupted
	if err := c.repos.Loops.Upsert(ctx, *loop); err != nil {
		return nil, err
	}
	return c.inner.Start(ctx, input)
}

// The aborted-suspension contract through the outer lifecycle: queue recovery
// must run and the run must end failed, so the NEXT claim resumes it instead
// of creating a second run while the first stays active.
func TestProcessClaimedQueueItemRecoversWhenSuspensionAborts(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()

	worktreePath := filepath.Join(t.TempDir(), "wt")
	git := &fakeGitGateway{
		createResult: CreateWorktreeResult{WorktreePath: worktreePath, Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head", WorktreeID: "worktree_1"},
	}
	agent := &corruptingAgentExecutor{
		repos:   fixture.repos,
		loopID:  "loop_worker_1",
		corrupt: `{"worktreeId":"wt-1","hitl":{`,
	}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git, AgentExecutor: agent,
		Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true,
		HITLEnabled: true, HITLAnswerTransport: "feishu",
		HITLNotify: func(context.Context, HITLAskNotification) error { return nil },
	})

	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	result, err := runner.ProcessClaimedQueueItem(ctx, *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v, want recovered failure result", err)
	}
	if result == nil || result.Status != "failed" {
		t.Fatalf("result = %#v, want failed after aborted suspension", result)
	}

	// The loop must not be parked awaiting a nonexistent ask.
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	if loop.Status == "awaiting_human" {
		t.Fatal("loop parked awaiting_human with no stored ask")
	}

	// Exactly one run exists and it is failed (resumable), and the queue item
	// was reconciled by recovery rather than left running.
	runs, err := fixture.repos.Runs.ListByLoop(ctx, "loop_worker_1")
	if err != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", err)
	}
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("runs = %#v, want exactly one failed (resumable) run", runs)
	}
	queueItem, err := fixture.repos.Queue.GetByID(ctx, claim.ID)
	if err != nil || queueItem == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", queueItem, err)
	}
	if queueItem.Status == "running" {
		t.Fatalf("queue item status = running, want reconciled by recovery (got %#v)", queueItem)
	}

	// Repair the metadata and reprocess: the failed run must be RESUMED, not
	// duplicated.
	repaired := `{"worktreeId":"wt-1"}`
	loop.MetadataJSON = &repaired
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert(repaired) error = %v", err)
	}
	agent.corrupt = repaired // second pass leaves metadata valid
	loop.Status = "queued"
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert(requeue loop) error = %v", err)
	}
	queueItem.Status = "queued"
	queueItem.AvailableAt = fixture.nowISO()
	queueItem.Attempts = 0
	if err := fixture.repos.Queue.Upsert(ctx, *queueItem); err != nil {
		t.Fatalf("Queue.Upsert(requeue) error = %v", err)
	}
	claim2, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim2 == nil {
		t.Fatalf("second ClaimNextOfType() = (%#v, %v)", claim2, err)
	}
	result2, err := runner.ProcessClaimedQueueItem(ctx, *claim2)
	if err != nil {
		t.Fatalf("second ProcessClaimedQueueItem() error = %v", err)
	}
	if result2 == nil || result2.Status != "awaiting_human" {
		t.Fatalf("second result = %#v, want awaiting_human once metadata is repaired", result2)
	}
	runs, err = fixture.repos.Runs.ListByLoop(ctx, "loop_worker_1")
	if err != nil {
		t.Fatalf("Runs.ListByLoop() error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs after reprocess = %d, want the failed run plus its continuation", len(runs))
	}
	// No zombie: every run is terminal-or-resumable, none left running.
	for _, run := range runs {
		if run.Status == "running" {
			t.Fatalf("run %s left running after reprocess: %#v", run.ID, run)
		}
	}
	loop, err = fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop status after repaired suspension = %q, want awaiting_human", loop.Status)
	}
	if _, ok := loops.ReadHITLAsk(loop.MetadataJSON); !ok {
		t.Fatal("no HITL ask stored after repaired suspension")
	}
}

// A message arriving after the prompt snapshot must survive acknowledgement,
// re-arm the completed item, and reach a later agent turn.
func TestMidRunHumanMessageSurvivesAndIsDeliveredNextTurn(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	meta, err := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: "2026-04-11T11:59:00.000Z", Text: "included message"})
	if err != nil {
		t.Fatalf("AppendHumanMessage() error = %v", err)
	}
	loop.MetadataJSON = &meta
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	worktreePath := filepath.Join(t.TempDir(), "wt")
	agent := &appendingAgentExecutor{repos: fixture.repos, loopID: loop.ID, text: "mid-run arrival", at: "2026-04-11T12:00:30.000Z", ask: `{"question":"Which datastore?","options":["redis","postgres"]}`}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{},
		Git:           &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: worktreePath, Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head", WorktreeID: "worktree_1"}},
		AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true,
		HITLEnabled: true, HITLAnswerTransport: "feishu", HITLNotify: func(context.Context, HITLAskNotification) error { return nil },
	})
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	first, err := runner.ProcessClaimedQueueItem(ctx, *claim)
	if err != nil || first == nil || first.Status != "awaiting_human" {
		t.Fatalf("first ProcessClaimedQueueItem() = (%#v, %v)", first, err)
	}
	if len(agent.inner.starts) != 1 || !strings.Contains(agent.inner.starts[0].Prompt, "included message") || strings.Contains(agent.inner.starts[0].Prompt, "mid-run arrival") {
		t.Fatalf("first prompt did not match snapshot: %#v", agent.inner.starts)
	}
	fresh, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", fresh, err)
	}
	if inbox := loops.ReadHumanInbox(fresh.MetadataJSON); len(inbox) != 1 || inbox[0].Text != "mid-run arrival" {
		t.Fatalf("inbox after suspension = %#v, want surviving mid-run arrival", inbox)
	}

	// Simulate /respond rearming the cancelled claim, then add another message
	// during the resumed successful turn.
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok {
		t.Fatal("missing persisted HITL ask")
	}
	ask.Answer, ask.Status = "postgres", "answered"
	meta, err = loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	fresh.MetadataJSON, fresh.Status = &meta, "queued"
	if err := fixture.repos.Loops.Upsert(ctx, *fresh); err != nil {
		t.Fatalf("Loops.Upsert(answered) error = %v", err)
	}
	queue, err := fixture.repos.Queue.GetByID(ctx, claim.ID)
	if err != nil || queue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", queue, err)
	}
	queue.Status, queue.AvailableAt, queue.Attempts = "queued", fixture.nowISO(), 0
	if err := fixture.repos.Queue.Upsert(ctx, *queue); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	agent.text, agent.at, agent.ask = "late arrival", "2026-04-11T12:01:30.000Z", ""
	claim2, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim2 == nil {
		t.Fatalf("second ClaimNextOfType() = (%#v, %v)", claim2, err)
	}
	if second, err := runner.ProcessClaimedQueueItem(ctx, *claim2); err != nil || second == nil || second.Status == "failed" {
		t.Fatalf("second ProcessClaimedQueueItem() = (%#v, %v)", second, err)
	}
	if len(agent.inner.starts) != 2 || !strings.Contains(agent.inner.starts[1].Prompt, "mid-run arrival") {
		t.Fatalf("second prompt missing survivor: %#v", agent.inner.starts)
	}
	fresh, err = fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", fresh, err)
	}
	queued, err := fixture.repos.Queue.FindActiveByLoopID(ctx, loop.ID)
	if err != nil || fresh.Status != "queued" || queued == nil || queued.Status != "queued" {
		t.Fatalf("survivor requeue = loop %#v queue %#v err %v", fresh, queued, err)
	}
	agent.text = ""
	claim3, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim3 == nil {
		t.Fatalf("third ClaimNextOfType() = (%#v, %v)", claim3, err)
	}
	if _, err := runner.ProcessClaimedQueueItem(ctx, *claim3); err != nil {
		t.Fatalf("third ProcessClaimedQueueItem() error = %v", err)
	}
	if len(agent.inner.starts) != 3 || !strings.Contains(agent.inner.starts[2].Prompt, "late arrival") {
		t.Fatalf("third prompt missing survivor: %#v", agent.inner.starts)
	}
}

func TestFinalizeCompletedHumanInboxTurnDoesNotReviveTerminalOrDisabledLoop(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, status, wantStatus string
		hitlEnabled              bool
	}{
		{name: "terminal loop", status: "terminated", hitlEnabled: true, wantStatus: "terminated"},
		{name: "HITL disabled", status: "running", hitlEnabled: false, wantStatus: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRunnerFixture(t)
			loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
			if err != nil || loop == nil {
				t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
			}
			meta, err := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: fixture.nowISO(), Text: "unread"})
			if err != nil {
				t.Fatalf("AppendHumanMessage() error = %v", err)
			}
			loop.MetadataJSON, loop.Status = &meta, tc.status
			if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			queue, err := fixture.repos.Queue.GetByID(ctx, "queue_worker_1")
			if err != nil || queue == nil {
				t.Fatalf("Queue.GetByID() = (%#v, %v)", queue, err)
			}
			if err := fixture.repos.Queue.Complete(ctx, queue.ID, fixture.nowISO()); err != nil {
				t.Fatalf("Queue.Complete() error = %v", err)
			}
			runner := New(Options{Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now, HITLEnabled: tc.hitlEnabled})
			if requeued, err := runner.finalizeCompletedHumanInboxTurn(ctx, loop.ID, queue.ID); err != nil || requeued {
				t.Fatalf("finalizeCompletedHumanInboxTurn() = (%t, %v)", requeued, err)
			}
			fresh, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
			if err != nil || fresh == nil || fresh.Status != tc.wantStatus {
				t.Fatalf("loop after finalize = (%#v, %v), want %q", fresh, err, tc.wantStatus)
			}
			active, err := fixture.repos.Queue.FindActiveByLoopID(ctx, loop.ID)
			if err != nil || active != nil {
				t.Fatalf("FindActiveByLoopID() = (%#v, %v)", active, err)
			}
		})
	}
}
