package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
)

func TestAskSentinelQuarantineResolvesSymlinkEvidence(t *testing.T) {
	worktree := t.TempDir()
	quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
	target := filepath.Join(t.TempDir(), "external-ask.json")
	want := `{"question":"truncated`
	if err := os.WriteFile(target, []byte(want), 0o644); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	path := filepath.Join(worktree, hitlSentinelRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("Symlink error = %v", err)
	}

	ask, err := consumeAskSentinel(worktree, quarantineRoot, "loop-symlink", "run-symlink")
	if err == nil || ask != nil {
		t.Fatalf("consumeAskSentinel() = (%#v, %v), want quarantined protocol error", ask, err)
	}
	incident := requireAskSentinelIncident(t, err)
	info, err := os.Lstat(incident.evidencePath)
	if err != nil {
		t.Fatalf("Lstat(quarantined) error = %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("quarantined evidence mode = %v, want regular copied content", info.Mode())
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("quarantined evidence permissions = %o, want 600", info.Mode().Perm())
	}
	rootInfo, err := os.Stat(quarantineRoot)
	if err != nil || rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("quarantine root = (%v, %v), want permissions 700", rootInfo, err)
	}
	if raw, err := os.ReadFile(incident.evidencePath); err != nil || string(raw) != want {
		t.Fatalf("quarantined evidence = (%q, %v), want resolved target content", raw, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree symlink still present: %v", err)
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != want {
		t.Fatalf("symlink target = (%q, %v), want untouched", raw, err)
	}
}

func TestAskSentinelQuarantineFallbackSemantics(t *testing.T) {
	t.Run("streams oversized evidence after cross-device rename", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "ask.json")
		want := strings.Repeat("x", maxAskSentinelBytes*2+17)
		if err := os.WriteFile(source, []byte(want), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		err := quarantineAskSentinel(source, filepath.Join(t.TempDir(), "quarantine"), "loop-large", "run-large", errors.New("oversized"), askSentinelFileOps{
			rename: func(string, string) error { return syscall.EXDEV },
		})
		incident := requireAskSentinelIncident(t, err)
		if incident.quarantineErr != nil || !incident.originalRemoved {
			t.Fatalf("incident = %#v, want complete fallback quarantine", incident)
		}
		info, statErr := os.Stat(incident.evidencePath)
		if statErr != nil || info.Size() != int64(len(want)) {
			t.Fatalf("quarantined size = (%v, %v), want %d", info, statErr, len(want))
		}
	})

	t.Run("copy success and remove failure is explicit", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "ask.json")
		if err := os.WriteFile(source, []byte("evidence"), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		err := quarantineAskSentinel(source, filepath.Join(t.TempDir(), "quarantine"), "loop-remove", "run-remove", errors.New("invalid"), askSentinelFileOps{
			rename: func(string, string) error { return syscall.EXDEV },
			remove: func(string) error { return errors.New("remove denied") },
		})
		incident := requireAskSentinelIncident(t, err)
		if incident.quarantineErr == nil || incident.originalRemoved || !strings.Contains(err.Error(), "quarantine incomplete") || !strings.Contains(err.Error(), "remove denied") {
			t.Fatalf("incident = %#v error = %v, want loud incomplete quarantine", incident, err)
		}
		if _, statErr := os.Stat(source); statErr != nil {
			t.Fatalf("original missing after injected remove failure: %v", statErr)
		}
		if raw, readErr := os.ReadFile(incident.evidencePath); readErr != nil || string(raw) != "evidence" {
			t.Fatalf("fallback copy = (%q, %v), want evidence", raw, readErr)
		}
	})

	t.Run("fallback read failure is the reported failure", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "ask.json")
		if err := os.WriteFile(source, []byte("evidence"), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		err := quarantineAskSentinel(source, filepath.Join(t.TempDir(), "quarantine"), "loop-read", "run-read", errors.New("invalid"), askSentinelFileOps{
			rename: func(path, _ string) error {
				if removeErr := os.Remove(path); removeErr != nil {
					t.Fatalf("Remove(source) error = %v", removeErr)
				}
				return errors.New("rename failed")
			},
		})
		incident := requireAskSentinelIncident(t, err)
		if incident.quarantineErr == nil || !strings.Contains(err.Error(), "open sentinel for fallback copy") || strings.Contains(err.Error(), "rename failed") {
			t.Fatalf("error = %v, want fallback read authority rather than rename error", err)
		}
	})
}

type malformedAskAgentExecutor struct {
	inner   fakeAgentExecutor
	content string
}

func (e *malformedAskAgentExecutor) Start(ctx context.Context, input AgentRunInput) (AgentExecution, error) {
	path := filepath.Join(input.WorkingDirectory, hitlSentinelRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(e.content), 0o644); err != nil {
		return nil, err
	}
	return e.inner.Start(ctx, input)
}

func TestProcessClaimedItemParksMalformedAskInAnswerableState(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	worktreePath := filepath.Join(t.TempDir(), "wt")
	quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: worktreePath, Branch: "looper/feature", BaseBranch: "main", HeadSHA: "base-head", WorktreeID: "worktree_1"}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: git,
		AgentExecutor: &malformedAskAgentExecutor{content: `{"question":"truncated`},
		Logger:        fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true,
		HITLEnabled: true, HITLQuarantineRoot: quarantineRoot, HITLAnswerTransport: "respond",
	})

	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "worker-1", "worker")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	result, err := runner.ProcessClaimedQueueItem(ctx, *claim)
	if err != nil || result == nil || result.Status != "awaiting_human" {
		t.Fatalf("ProcessClaimedQueueItem() = (%#v, %v), want awaiting_human", result, err)
	}
	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil || loop.Status != "awaiting_human" {
		t.Fatalf("loop = (%#v, %v), want awaiting_human", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || !strings.Contains(ask.Question, "regenerate the decision brief") || !strings.Contains(ask.Recommendation, quarantineRoot) {
		t.Fatalf("HITL ask = %#v, want durable answerable quarantine diagnostic", ask)
	}
	queue, err := fixture.repos.Queue.GetByID(ctx, claim.ID)
	if err != nil || queue == nil || queue.Status != "cancelled" {
		t.Fatalf("queue = (%#v, %v), want cancelled while waiting; no auto-requeue", queue, err)
	}
	runs, err := fixture.repos.Runs.ListByLoop(ctx, loop.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != "interrupted" {
		t.Fatalf("runs = (%#v, %v), want one resumable interrupted run", runs, err)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("push calls = %#v, want no publication after malformed ask", git.pushCalls)
	}
	evidence, err := filepath.Glob(filepath.Join(quarantineRoot, loop.ID, runs[0].ID, "ask-*", "ask.json"))
	if err != nil || len(evidence) != 1 {
		t.Fatalf("quarantined evidence = (%v, %v), want one durable file", evidence, err)
	}
}

func requireAskSentinelIncident(t *testing.T, err error) *askSentinelProtocolError {
	t.Helper()
	var incident *askSentinelProtocolError
	if !errors.As(err, &incident) {
		t.Fatalf("error = %#v, want askSentinelProtocolError", err)
	}
	return incident
}
