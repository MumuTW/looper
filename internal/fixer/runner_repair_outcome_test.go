package fixer

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/agent"
	"github.com/MumuTW/looper/internal/loops/failureclass"
	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/storage"
)

// The fixer treats the agent's declared `outcome` as the authority for whether a
// repair completed. These tests pin that authority: a parseable marker is not
// enough, the outcome must be declared and recognized, and a blocked repair must
// name a failure kind from the set the prompt offers.

func TestFixerRepairTaskOutcomeUsesStructuredAuthority(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		result      AgentResult
		wantBlocked bool
		wantKind    runpipe.QueueFailureKind
		wantErr     string
	}{
		{
			name:   "completed outcome authorizes advancing",
			result: AgentResult{Status: "completed", CompletionPayload: `{"outcome":"completed","summary":"applied fixes"}`},
		},
		{
			name:        "blocked outcome carries the declared kind",
			result:      AgentResult{Status: "completed", CompletionPayload: `{"outcome":"blocked","failure_kind":"manual_intervention","summary":"needs a human"}`},
			wantBlocked: true,
			wantKind:    runpipe.FailureManualIntervention,
		},
		{
			name:    "no payload at all",
			result:  AgentResult{Status: "completed"},
			wantErr: "Fixer agent completed without required structured outcome",
		},
		{
			name:    "unparseable payload",
			result:  AgentResult{Status: "completed", CompletionPayload: `{not json`},
			wantErr: "Fixer agent completed with invalid structured outcome",
		},
		{
			name:    "summary only, no outcome declared",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"summary":"applied fixes"}`},
			wantErr: "Fixer agent completed with missing or unrecognized structured outcome",
		},
		{
			name:    "unrecognized outcome",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"outcome":"mostly-done","summary":"applied fixes"}`},
			wantErr: "Fixer agent completed with missing or unrecognized structured outcome",
		},
		{
			name:    "blocked without a failure kind",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"outcome":"blocked","summary":"gave up"}`},
			wantErr: "Fixer blocked outcome requires a valid failure_kind",
		},
		{
			name:    "blocked with an unsupported failure kind",
			result:  AgentResult{Status: "completed", CompletionPayload: `{"outcome":"blocked","failure_kind":"non_retryable","summary":"gave up"}`},
			wantErr: "Fixer blocked outcome requires a valid failure_kind",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			blocked, message, kind, err := fixerRepairTaskOutcome(testCase.result)
			if testCase.wantErr != "" {
				if err == nil || err.Message != testCase.wantErr {
					t.Fatalf("err = %v, want %q", err, testCase.wantErr)
				}
				if err.Kind != runpipe.FailureRetryableTransient {
					t.Fatalf("err.Kind = %q, want retryable_transient", err.Kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if blocked != testCase.wantBlocked {
				t.Fatalf("blocked = %v, want %v", blocked, testCase.wantBlocked)
			}
			if testCase.wantBlocked {
				if kind != testCase.wantKind {
					t.Fatalf("kind = %q, want %q", kind, testCase.wantKind)
				}
				if message != "needs a human" {
					t.Fatalf("message = %q, want the agent's summary", message)
				}
			}
		})
	}
}

// TestFixerRepairTaskOutcomeFallsBackToTranscript covers the adapters and the
// checkpoint fallback path, which do not carry the parsed payload. The outcome has
// to be recoverable from the transcript, including the Codex --json form where the
// marker is embedded in a JSON event rather than printed on a stdout line.
func TestFixerRepairTaskOutcomeFallsBackToTranscript(t *testing.T) {
	t.Parallel()

	t.Run("plain stdout marker", func(t *testing.T) {
		t.Parallel()
		result := AgentResult{Status: "completed", Stdout: "working...\n" + agent.CompletionMarkerPrefix + `{"outcome":"completed","summary":"applied fixes"}`}
		if _, _, _, err := fixerRepairTaskOutcome(result); err != nil {
			t.Fatalf("err = %v, want the stdout marker to satisfy the contract", err)
		}
	})

	t.Run("codex jsonl embedded marker", func(t *testing.T) {
		t.Parallel()
		jsonl := `{"type":"item.completed","item":{"type":"agent_message","text":"__LOOPER_RESULT__={\"outcome\":\"blocked\",\"failure_kind\":\"retryable_after_resume\",\"summary\":\"remote head moved\"}"}}`
		blocked, message, kind, err := fixerRepairTaskOutcome(AgentResult{Status: "completed", Stdout: jsonl})
		if err != nil {
			t.Fatalf("err = %v, want the JSONL-embedded marker to be translated", err)
		}
		if !blocked || kind != runpipe.FailureRetryableAfterResume || message != "remote head moved" {
			t.Fatalf("(blocked, kind, message) = (%v, %q, %q), want the declared block", blocked, kind, message)
		}
	})
}

// TestIsTemplateCompletionPayloadSkipsEchoedFixerTemplate guards the interaction
// between the fixer prompt and template detection. The fixer template carries
// outcome/failure_kind alongside the placeholder summary, so a shape check keyed on
// a single "summary" key would treat an echoed template as a real completion.
func TestIsTemplateCompletionPayloadSkipsEchoedFixerTemplate(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		`{"summary":"<one-sentence summary>"}`,
		`{"outcome":"completed","summary":"<one-sentence summary>"}`,
		`{"outcome":"blocked","failure_kind":"manual_intervention","summary":"<one-sentence summary>"}`,
	} {
		if !isTemplateCompletionPayload(payload) {
			t.Fatalf("isTemplateCompletionPayload(%s) = false, want the echoed template skipped", payload)
		}
	}
	for _, payload := range []string{
		`{"outcome":"completed","summary":"applied fixes"}`,
		`{"summary":"applied fixes"}`,
	} {
		if isTemplateCompletionPayload(payload) {
			t.Fatalf("isTemplateCompletionPayload(%s) = true, want a real completion kept", payload)
		}
	}
}

// TestRunRepairStepReplaysAfterRejectedOutcome is the regression guard for the
// replay hole. The guard at the top of runRepairStep treats any stored repair
// record whose ParseStatus is "parsed" as a finished repair. Recording one for a
// blocked or unauthorized attempt therefore let the next automatic retry skip the
// agent and advance to reconcile/validate/push, publishing whatever the blocked
// attempt left in the worktree.
func TestRunRepairStepReplaysAfterRejectedOutcome(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name    string
		payload string
	}{
		{name: "declared block", payload: `{"outcome":"blocked","failure_kind":"retryable_transient","summary":"rate limited"}`},
		{name: "no outcome declared", payload: `{"summary":"applied fixes"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			detail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
			github := &fakeGitHubGateway{
				listOpen:      []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
				viewResponses: []PullRequestDetail{detail, detail},
			}
			git := &fakeGitGateway{
				createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
				prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
			}
			// Two results: the rejected attempt, then a valid completion for the
			// replay so a second agent start can be asserted as a real repair.
			agentExec := &fakeAgentExecutor{results: []AgentResult{
				{Status: "completed", ParseStatus: "parsed", Summary: "attempted", CompletionPayload: testCase.payload},
				{Status: "completed", ParseStatus: "parsed", Summary: "fixed on replay", CompletionPayload: `{"outcome":"completed","summary":"fixed on replay"}`},
			}}
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
			if result.Status != "failed" {
				t.Fatalf("result = %#v, want the rejected outcome to fail the run", result)
			}

			run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
			if err != nil || run == nil {
				t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
			}
			stored := parseCheckpoint(run.CheckpointJSON)
			// Errorf, not Fatalf: the replay assertions below are the behavioral half
			// of this guard and must still run when the record leaks through.
			if stored.Repair != nil {
				t.Errorf("stored checkpoint.Repair = %#v, want no repair record so the step stays replayable", stored.Repair)
			}
			// The replay guard must not treat this checkpoint as a finished repair:
			// re-running the step has to start the agent again, not return early.
			project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
			if err != nil || project == nil {
				t.Fatalf("Projects.GetByID() = (%#v, %v)", project, err)
			}
			startsBefore := len(agentExec.starts)
			replayed, replayErr := runner.runRepairStep(context.Background(), stepInput{
				Project: *project, Loop: storage.LoopRecord{ID: result.LoopID},
				Run: *run, Repo: "acme/looper", PRNumber: 42, Checkpoint: stored,
			})
			if replayErr != nil {
				t.Fatalf("replayed runRepairStep() error = %v, want the repair to run again", replayErr)
			}
			if len(agentExec.starts) != startsBefore+1 {
				t.Fatalf("agent starts = %d, want %d: the stored checkpoint short-circuited the replay", len(agentExec.starts), startsBefore+1)
			}
			if replayed.Repair == nil || replayed.Repair.Summary != "fixed on replay" {
				t.Fatalf("replayed.Repair = %#v, want the second attempt's repair record", replayed.Repair)
			}
		})
	}
}

// TestFixerPromptOffersOnlyHonoredFailureKinds pins the prompt to the kinds this
// path actually acts on. A repair-step block resumes identically for both
// retryable kinds, so advertising retryable_after_resume would promise a
// re-prepared environment that does not happen. It stays accepted on input.
func TestFixerPromptOffersOnlyHonoredFailureKinds(t *testing.T) {
	t.Parallel()
	prompt := agent.AppendFixerCompletionInstruction("repair the pr")
	if strings.Contains(prompt, "retryable_after_resume") {
		t.Fatalf("prompt offers retryable_after_resume, which a repair-step block does not honor:\n%s", prompt)
	}
	for _, needle := range []string{"retryable_transient", "manual_intervention"} {
		if !strings.Contains(prompt, needle) {
			t.Fatalf("prompt = %q, want %q offered", prompt, needle)
		}
	}
	// Still accepted so a reporting agent is not downgraded to a contract failure.
	if kind, ok := parseFixerBlockedFailureKind("retryable_after_resume"); !ok || kind != runpipe.FailureRetryableAfterResume {
		t.Fatalf("parseFixerBlockedFailureKind(retryable_after_resume) = (%q, %v), want it still accepted", kind, ok)
	}
}

// TestRepairStepHoldsCheckpointFallbackRestrictionForOperator verifies that when
// the agent executor propagates a checkpoint-fallback tool-network restriction
// refusal (ErrStaticConfigMismatch) through Wait, the fixer classifies it as
// manual intervention — not retryable_transient — so it does not burn retry
// attempts or trip the failure-streak circuit breaker.
func TestRepairStepHoldsCheckpointFallbackRestrictionForOperator(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	detail := PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix-42", BaseRefName: "main", BaseSHA: "base-1", Comments: []map[string]any{{"id": "c1", "threadId": "t1", "body": "please fix"}}}
	github := &fakeGitHubGateway{
		listOpen:      []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1"}},
		viewResponses: []PullRequestDetail{detail},
	}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt-42"), Branch: "feature/fix-42", HeadSHA: "base-head"},
		prepareResult: PrepareWorktreeResult{HeadSHA: "base-head", Clean: true},
	}
	agentExec := &fakeAgentExecutor{
		results: []AgentResult{{Status: "completed", ParseStatus: "parsed", Summary: "attempted"}},
		waitErr: fmt.Errorf("agent vendor %q cannot deny tool network access: %w", "claude-code", failureclass.ErrStaticConfigMismatch),
	}
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
	if result.Status != "failed" || result.FailureKind != runpipe.FailureManualIntervention {
		t.Fatalf("result = %#v, want failed manual_intervention for checkpoint fallback restriction refusal", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" || queue.FinishedAt == nil {
		t.Fatalf("queue = %#v, want terminal manual_intervention (no retry)", queue)
	}
}
