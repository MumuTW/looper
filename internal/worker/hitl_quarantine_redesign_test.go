package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// TestAskSentinelQuarantineNeverFollowsSymlink proves the daemon never reads an
// agent-controlled ask.json symlink target with its own privileges. The symlink
// targets a daemon-readable file outside the worktree whose bytes must never be
// copied into quarantine; only the link itself is preserved/removed, and the
// target file is left untouched.
func TestAskSentinelQuarantineNeverFollowsSymlink(t *testing.T) {
	worktree := t.TempDir()
	quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
	target := filepath.Join(t.TempDir(), "secret-ask.json")
	secret := `{"question":"truncated`
	if err := os.WriteFile(target, []byte(secret), 0o644); err != nil {
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
	if incident.evidenceKind != "symlink" || incident.evidenceHash == "" {
		t.Fatalf("incident = %#v, want symlink kind + evidence hash", incident)
	}

	// The link itself is gone from the worktree.
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree symlink still present: %v", err)
	}
	// The target file is untouched.
	if raw, err := os.ReadFile(target); err != nil || string(raw) != secret {
		t.Fatalf("symlink target = (%q, %v), want untouched", raw, err)
	}
	// Quarantine holds a metadata descriptor, NOT the target's bytes.
	if incident.evidencePath == "" {
		t.Fatalf("incident = %#v, want an evidence path", incident)
	}
	raw, readErr := os.ReadFile(incident.evidencePath)
	if readErr != nil {
		t.Fatalf("ReadFile(evidence) error = %v", readErr)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("quarantined evidence leaked the symlink target's bytes: %q", raw)
	}
	var descriptor map[string]any
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		t.Fatalf("quarantined evidence = %q, want a JSON descriptor: %v", raw, err)
	}
	if descriptor["kind"] != "symlink" {
		t.Fatalf("descriptor kind = %v, want symlink", descriptor["kind"])
	}
	if got, _ := descriptor["target"].(string); !strings.HasSuffix(got, "secret-ask.json") {
		t.Fatalf("descriptor target = %v, want the link target path", descriptor["target"])
	}
	// Sidecar metadata records the hash + target for diagnosis.
	sidecar, err := os.ReadFile(incident.evidencePath + ".meta.json")
	if err != nil {
		t.Fatalf("ReadFile(sidecar) error = %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(sidecar, &meta); err != nil {
		t.Fatalf("sidecar = %q, want JSON: %v", sidecar, err)
	}
	if meta["sha256"] != incident.evidenceHash {
		t.Fatalf("sidecar sha256 = %v, want %s", meta["sha256"], incident.evidenceHash)
	}
	// Evidence is private.
	if info, err := os.Stat(incident.evidencePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence permissions = (%v, %o), want 600", info, info.Mode().Perm())
	}
	if rootInfo, err := os.Stat(quarantineRoot); err != nil || rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("quarantine root = (%v, %o), want 700", rootInfo, rootInfo.Mode().Perm())
	}
}

func TestAskSentinelQuarantineBoundedCopyAndRetention(t *testing.T) {
	t.Run("oversized evidence is bounded with size/hash metadata", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "ask.json")
		want := strings.Repeat("x", maxAskSentinelBytes*2+17)
		if err := os.WriteFile(source, []byte(want), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
		err := quarantineAskSentinel(quarantineRequest{
			path: source, root: quarantineRoot, loopID: "loop-large", runID: "run-large",
			cause: errors.New("oversized"), evidenceKind: "regular",
			evidenceBytes: []byte(want[:maxAskSentinelBytes]), originalSize: int64(len(want)),
		})
		incident := requireAskSentinelIncident(t, err)
		if incident.quarantineErr != nil || !incident.originalRemoved {
			t.Fatalf("incident = %#v, want complete bounded quarantine", incident)
		}
		info, statErr := os.Stat(incident.evidencePath)
		if statErr != nil || info.Size() != int64(maxAskSentinelBytes) {
			t.Fatalf("quarantined size = (%v, %d), want bounded %d", info, info.Size(), maxAskSentinelBytes)
		}
		if raw, _ := os.ReadFile(incident.evidencePath); string(raw) != want[:maxAskSentinelBytes] {
			t.Fatalf("quarantined prefix mismatch")
		}
		sidecar, err := os.ReadFile(incident.evidencePath + ".meta.json")
		if err != nil {
			t.Fatalf("ReadFile(sidecar) error = %v", err)
		}
		var meta map[string]any
		if err := json.Unmarshal(sidecar, &meta); err != nil {
			t.Fatalf("sidecar = %q: %v", sidecar, err)
		}
		if meta["truncated"] != true {
			t.Fatalf("sidecar truncated = %v, want true", meta["truncated"])
		}
		if int64(meta["originalSize"].(float64)) != int64(len(want)) {
			t.Fatalf("sidecar originalSize = %v, want %d", meta["originalSize"], len(want))
		}
		if meta["sha256"] != incident.evidenceHash {
			t.Fatalf("sidecar sha256 = %v, want %s", meta["sha256"], incident.evidenceHash)
		}
		if _, err := os.Stat(source); !os.IsNotExist(err) {
			t.Fatalf("original sentinel still present after quarantine: %v", err)
		}
	})

	t.Run("copy success and remove failure is explicit", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "ask.json")
		if err := os.WriteFile(source, []byte("evidence"), 0o644); err != nil {
			t.Fatalf("WriteFile error = %v", err)
		}
		quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
		err := quarantineAskSentinel(quarantineRequest{
			path: source, root: quarantineRoot, loopID: "loop-remove", runID: "run-remove",
			cause: errors.New("invalid"), evidenceKind: "regular",
			evidenceBytes: []byte("evidence"), originalSize: int64(len("evidence")),
			ops: askSentinelFileOps{remove: func(string) error { return errors.New("remove denied") }},
		})
		incident := requireAskSentinelIncident(t, err)
		if incident.quarantineErr == nil || incident.originalRemoved ||
			!strings.Contains(err.Error(), "quarantine incomplete") || !strings.Contains(err.Error(), "remove denied") {
			t.Fatalf("incident = %#v error = %v, want loud incomplete quarantine", incident, err)
		}
		if _, statErr := os.Stat(source); statErr != nil {
			t.Fatalf("original missing after injected remove failure: %v", statErr)
		}
		if raw, readErr := os.ReadFile(incident.evidencePath); readErr != nil || string(raw) != "evidence" {
			t.Fatalf("quarantined evidence = (%q, %v), want evidence", raw, readErr)
		}
	})

	t.Run("repeated oversized incidents are pruned to the retention budget", func(t *testing.T) {
		quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
		loopID := "loop-budget"
		for i := 0; i < maxQuarantineIncidentsPerLoop+4; i++ {
			source := filepath.Join(t.TempDir(), "ask.json")
			content := strings.Repeat("y", maxAskSentinelBytes*2)
			if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
				t.Fatalf("WriteFile(%d) error = %v", i, err)
			}
			if err := quarantineAskSentinel(quarantineRequest{
				path: source, root: quarantineRoot, loopID: loopID, runID: "run-budget",
				cause: errors.New("oversized"), evidenceKind: "regular",
				evidenceBytes: []byte(content[:maxAskSentinelBytes]), originalSize: int64(len(content)),
			}); err == nil {
				t.Fatalf("quarantineAskSentinel(%d) error = nil, want incident", i)
			}
		}
		loopDir := filepath.Join(quarantineRoot, loopID)
		count := 0
		_ = filepath.WalkDir(loopDir, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if strings.HasPrefix(d.Name(), "ask-") {
				count++
			}
			return nil
		})
		if count > maxQuarantineIncidentsPerLoop {
			t.Fatalf("quarantine incidents = %d, want at most %d (retention budget not enforced)", count, maxQuarantineIncidentsPerLoop)
		}
	})

	t.Run("quarantine records evidence without re-reading the worktree", func(t *testing.T) {
		// A regular-sentinel quarantine writes the request bytes verbatim and
		// never opens the source, so a source that vanishes mid-incident still
		// leaves the provided evidence intact.
		quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
		err := quarantineAskSentinel(quarantineRequest{
			path: filepath.Join(t.TempDir(), "missing-ask.json"), root: quarantineRoot,
			loopID: "loop-noread", runID: "run-noread", cause: errors.New("invalid"),
			evidenceKind: "regular", evidenceBytes: []byte("captured"), originalSize: 7,
			ops: askSentinelFileOps{remove: func(string) error { return nil }},
		})
		incident := requireAskSentinelIncident(t, err)
		if raw, readErr := os.ReadFile(incident.evidencePath); readErr != nil || string(raw) != "captured" {
			t.Fatalf("quarantined evidence = (%q, %v), want the request bytes", raw, readErr)
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
	if ask.EvidenceHash == "" {
		t.Fatalf("HITL ask = %#v, want a persisted evidence identity for resume recovery", ask)
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

// TestDetectHumanAskResumesAfterQuarantineFailureWithoutReasking is the
// end-to-end quarantine-failure → respond → resume contract. When quarantine
// cannot remove the original sentinel, a resumed run would otherwise observe
// the same malformed sentinel, fail quarantine again, and create another ask
// forever. The persisted evidence hash lets the human's response authorize
// consuming the original under human authority: the resumed detectHumanAsk
// recognizes the SAME evidence and proceeds without raising a new ask.
func TestDetectHumanAskResumesAfterQuarantineFailureWithoutReasking(t *testing.T) {
	fixture := newRunnerFixture(t)
	ctx := context.Background()
	quarantineRoot := filepath.Join(t.TempDir(), "quarantine")
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now,
		HITLEnabled: true, HITLQuarantineRoot: quarantineRoot,
	})

	worktree := t.TempDir()
	looperDir := filepath.Join(worktree, ".looper")
	if err := os.MkdirAll(looperDir, 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}
	sentinelPath := filepath.Join(looperDir, "ask.json")
	if err := os.WriteFile(sentinelPath, []byte(`{"question":"truncated`), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}
	// Make the worktree .looper dir read-only so quarantine cannot remove the
	// original — the exact "quarantine incomplete, original remains" state.
	if err := os.Chmod(looperDir, 0o555); err != nil {
		t.Fatalf("Chmod(looperDir) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(looperDir, 0o755) })

	loop, err := fixture.repos.Loops.GetByID(ctx, "loop_worker_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = (%#v, %v)", loop, err)
	}
	input := stepInput{Loop: *loop, Run: storage.RunRecord{ID: "run-quarantine-resume"}}

	// First probe: sentinel is malformed and cannot be removed → synthetic ask
	// carrying the evidence hash.
	first, err := runner.detectHumanAsk(ctx, input, worktree, "exec-1")
	if err != nil || first == nil {
		t.Fatalf("first detectHumanAsk = (%#v, %v), want a synthetic ask", first, err)
	}
	if first.evidenceHash == "" {
		t.Fatalf("first synthetic ask = %#v, want an evidence hash", first)
	}
	if _, statErr := os.Lstat(sentinelPath); statErr != nil {
		t.Fatalf("sentinel removed during first probe; test needs it to persist: %v", statErr)
	}

	// Simulate the human answering "continue without the original request".
	answered := loops.HITLAsk{
		Question:     first.question,
		Answer:       "continue without the original request",
		Status:       "answered",
		EvidenceHash: first.evidenceHash,
	}
	meta, err := loops.WriteHITLAsk(loop.MetadataJSON, answered)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	loop.MetadataJSON = &meta
	if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	// Resume: the same sentinel is still present, but the human already
	// authorized this evidence. detectHumanAsk must NOT raise a new ask.
	second, err := runner.detectHumanAsk(ctx, input, worktree, "exec-2")
	if err != nil {
		t.Fatalf("resume detectHumanAsk error = %v, want no error", err)
	}
	if second != nil {
		t.Fatalf("resume detectHumanAsk = %#v, want nil (no new ask for already-answered evidence)", second)
	}

	// A genuinely new malformed sentinel (different content → different hash)
	// must still raise a fresh ask, so the recovery does not mask new incidents.
	if err := os.Chmod(looperDir, 0o755); err != nil {
		t.Fatalf("Chmod(looperDir, writable) error = %v", err)
	}
	if err := os.Remove(sentinelPath); err != nil {
		t.Fatalf("Remove(sentinel) error = %v", err)
	}
	if err := os.WriteFile(sentinelPath, []byte(`{"options":["a"]`), 0o644); err != nil {
		t.Fatalf("WriteFile(new sentinel) error = %v", err)
	}
	if err := os.Chmod(looperDir, 0o555); err != nil {
		t.Fatalf("Chmod(looperDir, readonly) error = %v", err)
	}
	third, err := runner.detectHumanAsk(ctx, input, worktree, "exec-3")
	if err != nil || third == nil {
		t.Fatalf("third detectHumanAsk = (%#v, %v), want a fresh ask for new evidence", third, err)
	}
	if third.evidenceHash == first.evidenceHash {
		t.Fatalf("third ask evidence hash matches the first; new evidence must produce a new hash")
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
