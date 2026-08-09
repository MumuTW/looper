package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/loops"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func TestHandlerRespondConsumesPersistedMalformedGateIdentity(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	ctx := context.Background()
	nowISO := "2026-04-11T12:00:00.000Z"
	worktree := t.TempDir()
	if err := os.Mkdir(filepath.Join(worktree, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".looper", "ask.json"), []byte(`{"question":"truncated`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, evidence, err := loops.StageHITLGateEvidence(worktree, 4096)
	if err != nil || evidence == nil {
		t.Fatalf("StageHITLGateEvidence() = (%#v, %v)", evidence, err)
	}
	metadata, err := loops.WriteHITLAsk(nil, loops.HITLAsk{Question: "Discard malformed request?", Status: "awaiting", GateEvidence: evidence})
	if err != nil {
		t.Fatal(err)
	}
	projectID, loopID, targetID := "project_hitl_evidence", "loop_hitl_evidence", "project_hitl_evidence"
	if err := services.Repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: worktree, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := services.Repositories.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 701, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	reason := "worker suspended awaiting human decision"
	if err := services.Repositories.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_hitl_evidence", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl-evidence", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3, LastError: &reason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/701/respond", strings.NewReader(`{"answer":"discard and regenerate"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(worktree, ".looper", "ask.pending")); !os.IsNotExist(err) {
		t.Fatalf("staged evidence still exists after authorized response: %v", err)
	}
	loop, err := services.Repositories.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("loop = (%#v, %v), want running", loop, err)
	}
}

func TestHandlerRespondResumesAwaitingHumanLoop(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl"
	loopID := "loop_hitl"
	targetID := projectID
	metadata := `{"hitl":{"question":"Which direction?","options":["continue","redirect"],"sessionId":"sess-abc","executionId":"agent-1","vendor":"codex","status":"awaiting","askedAt":"2026-04-11T11:59:00.000Z"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 71, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// A cancelled queue item (as a suspend leaves behind) so the resume requeues it.
	cancelReason := "loop suspended awaiting human"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/71/respond", strings.NewReader(`{"answer":"continue with the redis approach"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "running" {
		t.Fatalf("loop.Status = %q, want running", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		t.Fatalf("HITL ask metadata missing after respond")
	}
	if ask.Answer != "continue with the redis approach" {
		t.Fatalf("ask.Answer = %q, want the posted answer", ask.Answer)
	}
	if ask.Status != "answered" {
		t.Fatalf("ask.Status = %q, want answered", ask.Status)
	}
	if ask.SessionID != "sess-abc" {
		t.Fatalf("ask.SessionID = %q, want preserved sess-abc", ask.SessionID)
	}

	// The loop must be requeued so the scheduler resumes it.
	items, err := services.Repositories.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	queued := false
	for _, item := range items {
		if item.LoopID != nil && *item.LoopID == loopID && item.Status == "queued" {
			queued = true
		}
	}
	if !queued {
		t.Fatalf("expected a queued queue item for the resumed loop; items=%#v", items)
	}
}

func TestDeliverHumanAnswerSharesLoopRequeueGuard(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_guard"
	loopID := "loop_hitl_guard"
	targetID := projectID
	metadata := `{"hitl":{"question":"Continue?","sessionId":"sess-guard","status":"awaiting"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 711, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	cancelReason := "worker suspended awaiting human"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl_guard", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl-guard", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	// The answer transaction must wait for the same guard used by worker-side
	// post-park metadata reconciliation; otherwise stale worker writes can win.
	unlock := loops.LockLoopRequeue(loopID)
	result := make(chan error, 1)
	go func() {
		_, err := h.deliverHumanAnswer(context.Background(), loopID, "continue")
		result <- err
	}()
	select {
	case err := <-result:
		unlock()
		t.Fatalf("deliverHumanAnswer completed while requeue guard was held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	unlock()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("deliverHumanAnswer() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deliverHumanAnswer did not complete after requeue guard release")
	}
}

func TestHandlerRespondRejectsNonAwaitingLoop(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_reject"
	loopID := "loop_hitl_reject"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 72, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "paused", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/72/respond", strings.NewReader(`{"answer":"continue"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "paused" {
		t.Fatalf("loop.Status = %q, want unchanged paused", loop.Status)
	}
}

// HITL resume re-enters mutateLoopStatus(...Running). When live worker/global
// vendor was removed while the loop waited, sticky agent_snapshot_json on the
// interrupted predecessor must still allow answer requeue (same rule as retry).
func TestHandlerRespondAllowsStickySnapshotWhenAgentNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Agent.Vendor = nil
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_sticky_snapshot"
	loopID := "loop_hitl_sticky_snapshot"
	targetID := projectID
	metadata := `{"hitl":{"question":"Continue?","options":["yes","no"],"sessionId":"sess-sticky","status":"awaiting","askedAt":"2026-04-11T11:59:00.000Z"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 79, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	cancelReason := "worker suspended awaiting human decision"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl_sticky", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl-sticky", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	snapshot := `{"vendor":"codex","model":"frozen-hitl","profileId":"worker-profile"}`
	if err := services.Repositories.Runs.Upsert(context.Background(), storage.RunRecord{
		ID: "run_" + loopID + "_hitl", LoopID: loopID, Status: "interrupted",
		StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO, AgentSnapshotJSON: &snapshot,
	}); err != nil {
		t.Fatalf("Runs.Upsert(snapshot) error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/79/respond", strings.NewReader(`{"answer":"yes continue"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 sticky HITL resume with predecessor snapshot; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("loop after sticky HITL respond = %#v, %v, want running", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Answer != "yes continue" || ask.Status != "answered" {
		t.Fatalf("HITL ask after sticky respond = %#v, ok=%v", ask, ok)
	}
}

func TestHandlerRespondRejectsWhenAgentNotConfiguredWithoutSnapshot(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Agent.Vendor = nil
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_no_snapshot"
	loopID := "loop_hitl_no_snapshot"
	targetID := projectID
	metadata := `{"hitl":{"question":"Continue?","sessionId":"sess-none","status":"awaiting"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 80, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/80/respond", strings.NewReader(`{"answer":"yes"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without live vendor or snapshot; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "without config.agent.vendor") {
		t.Fatalf("body = %s, want agent not configured rejection", recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	// Answer is stored before mutateLoopStatus; loop may stay awaiting_human if
	// requeue fails after metadata write — either way respond must not succeed.
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status == "running" {
		t.Fatalf("loop.Status = running, want not requeued without agent identity")
	}
}

func TestHandlerFeishuCardActionDeliversAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	// The card-action route is fail-closed: it requires a configured, matching
	// Feishu verification token before it will deliver an answer.
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_card"
	loopID := "loop_card"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 81, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-123","action":{"tag":"button","value":{"loopSeq":"81","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "running" {
		t.Fatalf("loop.Status = %q, want running", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Answer != "redis" || ask.Status != "answered" {
		t.Fatalf("ask = %#v (ok=%v), want answer redis + answered", ask, ok)
	}
}

// setupAwaitingCardLoop seeds a project + awaiting_human loop and returns the
// handler + services for card-action security tests.
func setupAwaitingCardLoop(t *testing.T, cfg config.Config, rt *looperdruntime.Runtime, projectID, loopID string, seq int64) *Handler {
	t.Helper()
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: seq, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return h
}

func TestHandlerFeishuCardActionRejectsWhenTokenNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	// No verificationTokenEnv configured -> the injection route must fail closed.
	h := setupAwaitingCardLoop(t, cfg, rt, "project_card_notok", "loop_card_notok", 82)

	body := `{"token":"anything","action":{"tag":"button","value":{"loopSeq":"82","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when verification token unconfigured; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_card_notok")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human (no answer delivered)", loop.Status)
	}
}

func TestHandlerFeishuCardActionRejectsTokenMismatch(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN2", "the-real-token")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN2"
	h := setupAwaitingCardLoop(t, cfg, rt, "project_card_bad", "loop_card_bad", 83)

	body := `{"token":"wrong-token","action":{"tag":"button","value":{"loopSeq":"83","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on token mismatch; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_card_bad")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human (no answer delivered)", loop.Status)
	}
}

func TestHandlerFeishuCardActionAnswersChallenge(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(`{"type":"url_verification","challenge":"abc123","token":"t"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `"challenge":"abc123"`) {
		t.Fatalf("challenge echo missing: %s", recorder.Body.String())
	}
}

func TestHandlerFeishuCardActionGatedWhenHITLDisabled(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	// cfg.HITL.Enabled defaults to false.
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	body := `{"action":{"tag":"button","value":{"loopSeq":"81","answer":"redis"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when hitl disabled; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerFeishuThreadReplyDeliversTypedAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN3", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN3"
	h := setupAwaitingCardLoop(t, cfg, rt, "project_thread", "loop_thread", 91)
	services := rt.Services()
	// The gateway would have recorded this when it created the thread root.
	if err := services.Repositories.FeishuThreads.Upsert(context.Background(), "om_root_91", "loop_thread", "oc_group", "2026-04-11T12:00:00.000Z"); err != nil {
		t.Fatalf("FeishuThreads.Upsert() error = %v", err)
	}

	// A human types a free-text reply in the ask thread (im.message.receive_v1).
	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_reply","root_id":"om_root_91","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"用 A 改 resize handle\"}"},"sender":{"sender_type":"user","sender_id":{"open_id":"ou_user"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), "loop_thread")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "running" {
		t.Fatalf("loop.Status = %q, want running (resumed by typed reply)", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Answer != "用 A 改 resize handle" || ask.Status != "answered" {
		t.Fatalf("ask = %#v (ok=%v), want the typed free-text answer", ask, ok)
	}
}

func TestHandlerFeishuThreadReplyIgnoresNonHumanSender(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN5", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN5"
	h := setupAwaitingCardLoop(t, cfg, rt, "project_thread_bot", "loop_thread_bot", 93)
	services := rt.Services()
	if err := services.Repositories.FeishuThreads.Upsert(context.Background(), "om_root_bot", "loop_thread_bot", "oc_group", "2026-04-11T12:00:00.000Z"); err != nil {
		t.Fatalf("FeishuThreads.Upsert() error = %v", err)
	}

	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_bot_reply","root_id":"om_root_bot","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"bot follow-up\"}"},"sender":{"sender_type":"app","sender_id":{"open_id":"ou_bot"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "non-human sender") {
		t.Fatalf("status/body = %d/%s, want ignored non-human sender", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), "loop_thread_bot")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human", loop.Status)
	}
}

func TestHandlerFeishuThreadReplyIgnoresUnknownThread(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN4", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN4"
	h := setupAwaitingCardLoop(t, cfg, rt, "project_thread2", "loop_thread2", 92)

	// A reply in a thread with no mapped loop must be ignored (200, not delivered),
	// so ordinary group chatter doesn't error or touch any loop.
	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_x","root_id":"om_unknown","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"just chatting\"}"},"sender":{"sender_type":"user","sender_id":{"open_id":"ou_user"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ignored); body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := rt.Services().Repositories.Loops.GetByID(context.Background(), "loop_thread2")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human", loop.Status)
	}
}

func TestHandlerRespondRequiresAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_empty"
	loopID := "loop_hitl_empty"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 73, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/73/respond", strings.NewReader(`{"answer":"   "}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty answer; body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestDeliverHumanAnswerRejectsWhenStorageNotConfigured verifies the HITL
// answer handler fails closed with "Storage is not configured" instead of
// panicking when the coordinator or repositories are nil.
func TestDeliverHumanAnswerRejectsWhenStorageNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{
		Config:  cfg,
		Runtime: fixedRuntimeState{services: looperdruntime.Services{}},
	})
	_, err := h.deliverHumanAnswer(context.Background(), "loop_any", "continue")
	if err == nil {
		t.Fatal("deliverHumanAnswer() error = nil, want storage-not-configured rejection")
	}
	var typed apiError
	if !asAPIError(err, &typed) || typed.status != http.StatusInternalServerError {
		t.Fatalf("error = %v, want 500 storage-not-configured apiError", err)
	}
	if !strings.Contains(typed.message, "Storage is not configured") {
		t.Fatalf("error message = %q, want 'Storage is not configured'", typed.message)
	}
	// The runtime must still be stoppable after the nil-services call.
	_ = rt
}

// TestHandbackLoopRejectsWhenStorageNotConfigured verifies the handback
// handler fails closed with "Storage is not configured" instead of panicking
// when the coordinator or repositories are nil.
func TestHandbackLoopRejectsWhenStorageNotConfigured(t *testing.T) {
	_, cfg := startTestRuntime(t)
	h := NewHandler(Context{
		Config:  cfg,
		Runtime: fixedRuntimeState{services: looperdruntime.Services{}},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/1/handback", strings.NewReader(`{}`))
	_, err := h.handbackLoop(context.Background(), req, "loop_any")
	if err == nil {
		t.Fatal("handbackLoop() error = nil, want storage-not-configured rejection")
	}
	var typed apiError
	if !asAPIError(err, &typed) || typed.status != http.StatusInternalServerError {
		t.Fatalf("error = %v, want 500 storage-not-configured apiError", err)
	}
	if !strings.Contains(typed.message, "Storage is not configured") {
		t.Fatalf("error message = %q, want 'Storage is not configured'", typed.message)
	}
}

func TestDeliverHumanAnswerMapsMissingLoopToNotFound(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	_, err := h.deliverHumanAnswer(context.Background(), "loop_missing", "continue")
	var typed apiError
	if !asAPIError(err, &typed) || typed.status != http.StatusNotFound || typed.code != pkgapi.ErrorCodeLoopNotFound {
		t.Fatalf("deliverHumanAnswer() error = %v, want LOOP_NOT_FOUND/404", err)
	}
}

// TestDeliverHumanAnswerRejectsMalformedHITLMetadata verifies that an
// awaiting_human loop with no readable HITL ask metadata is rejected instead
// of silently writing a zero-valued ask that loses the question, correlation
// fields, and gate evidence.
func TestDeliverHumanAnswerRejectsMalformedHITLMetadata(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_malformed"
	loopID := "loop_hitl_malformed"
	targetID := projectID

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	// awaiting_human but with no HITL metadata — ReadHITLAsk returns false.
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 95, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	_, err := h.deliverHumanAnswer(context.Background(), loopID, "continue")
	if err == nil {
		t.Fatal("deliverHumanAnswer() error = nil, want rejection for missing HITL ask")
	}
	var typed apiError
	if !asAPIError(err, &typed) || typed.status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 apiError for unreadable HITL metadata", err)
	}
	if !strings.Contains(typed.message, "HITL ask metadata") {
		t.Fatalf("error message = %q, want 'HITL ask metadata'", typed.message)
	}
	// The loop must stay awaiting_human — no answer was written.
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want unchanged awaiting_human", loop.Status)
	}
}

// TestHandlerFeishuThreadReplyReportsLoopNoLongerExists verifies the 404
// reason is distinct from the 400 "not awaiting a human" reason so the two
// outcomes stay separable in logs.
func TestHandlerFeishuThreadReplyReportsLoopNoLongerExists(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN_404", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN_404"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	// Thread mapping points to a loop that does not exist.
	if err := services.Repositories.FeishuThreads.Upsert(context.Background(), "om_root_ghost", "loop_ghost_404", "oc_group", "2026-04-11T12:00:00.000Z"); err != nil {
		t.Fatalf("FeishuThreads.Upsert() error = %v", err)
	}

	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_reply","root_id":"om_root_ghost","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"reply\"}"},"sender":{"sender_type":"user","sender_id":{"open_id":"ou_user"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "loop no longer exists") {
		t.Fatalf("body = %s, want 'loop no longer exists' reason for 404", recorder.Body.String())
	}
}
