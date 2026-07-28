package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestHandlerFeishuCardActionDeliversAnswer(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	cfg.HITL.AnswerTransport = "feishu"
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
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting","transport":"feishu","executionId":"agent-card","askedAt":"2026-04-11T12:00:00.000Z"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 81, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-123","action":{"tag":"button","value":{"loopSeq":"81","answer":"redis","executionId":"agent-card","askedAt":"2026-04-11T12:00:00.000Z"}}}`
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

// Pre-upgrade Feishu cards lack executionId/askedAt; they must not bind via
// /respond omission semantics after the loop re-escalates.
func TestHandlerFeishuCardActionRejectsMissingGenerationTokens(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	cfg.HITL.AnswerTransport = "feishu"
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN_NOGEN", "verify-tok-nogen")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN_NOGEN"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_card_nogen"
	loopID := "loop_card_nogen"
	targetID := projectID
	// Park has a generation (post-escalation); old card omits tokens.
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting","transport":"feishu","executionId":"agent-new","askedAt":"2026-04-11T13:00:00.000Z"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 97, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-nogen","action":{"tag":"button","value":{"loopSeq":"97","answer":"stale-option"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"delivered":false`) || !strings.Contains(recorder.Body.String(), "missing ask generation tokens") {
		t.Fatalf("body = %s, want delivered:false missing generation tokens", recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want still awaiting_human", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || ask.Answer != "" {
		t.Fatalf("ask = %#v, want still awaiting with empty answer", ask)
	}
}

// TestHandlerFeishuCardActionMarksAskAnswered covers both Feishu delivery paths:
// the API card-action callback must invoke the same MarkHITLAskAnswered hook the
// inbox poll uses so interactive cards leave the clickable "awaiting" state.

func TestHandlerFeishuCardActionMarksAskAnswered(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	cfg.HITL.AnswerTransport = "feishu"
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN_MARK", "verify-tok-mark")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN_MARK"

	var marked [][2]string
	runtimeWithHook := &runtimeHITLAnswerProbe{
		RuntimeState: rt,
		onMark: func(_ context.Context, loopID, answer string) {
			marked = append(marked, [2]string{loopID, answer})
		},
	}
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithHook})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_card_mark"
	loopID := "loop_card_mark"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-mark","status":"awaiting","transport":"feishu","executionId":"agent-mark","askedAt":"2026-04-11T12:00:00.000Z"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 88, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-mark","action":{"tag":"button","value":{"loopSeq":"88","answer":"postgres","executionId":"agent-mark","askedAt":"2026-04-11T12:00:00.000Z"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(marked) != 1 || marked[0][0] != loopID || marked[0][1] != "postgres" {
		t.Fatalf("MarkHITLAskAnswered calls = %#v, want one card completion for loop/answer", marked)
	}
}

// runtimeHITLAnswerProbe embeds RuntimeState and adds MarkHITLAskAnswered so
// API delivery tests can assert the notification completion hook ran.

func TestHandlerFeishuCardActionRejectsWhenTokenNotConfigured(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	// No verificationTokenEnv configured -> the injection route must fail closed.
	h := setupAwaitingCardLoop(t, cfg, rt, "project_card_notok", "loop_card_notok", 82)

	body := `{"token":"anything","action":{"tag":"button","value":{"loopSeq":"82","answer":"redis","executionId":"agent-card","askedAt":"2026-04-11T12:00:00.000Z"}}}`
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

	body := `{"token":"wrong-token","action":{"tag":"button","value":{"loopSeq":"83","answer":"redis","executionId":"agent-card","askedAt":"2026-04-11T12:00:00.000Z"}}}`
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

func TestHandlerFeishuCardActionRejectsRespondTransport(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	cfg.HITL.AnswerTransport = "respond"
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN_RESPOND", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN_RESPOND"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_respond_btn"
	loopID := "loop_respond_btn"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting","transport":"respond","executionId":"agent-r","askedAt":"2026-04-11T12:00:00.000Z"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 96, ProjectID: projectID, Type: "fixer", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-123","action":{"tag":"button","value":{"loopSeq":"96","answer":"keep","executionId":"agent-r","askedAt":"2026-04-11T12:00:00.000Z"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"delivered":false`) {
		t.Fatalf("body = %s, want delivered:false for respond transport button", recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want still awaiting_human", loop.Status)
	}
}

func TestHandlerFeishuCardActionRejectsGitHubTransport(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN6", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN6"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_notify_btn"
	loopID := "loop_notify_btn"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting","transport":"github","prNumber":42}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 94, ProjectID: projectID, Type: "fixer", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-123","action":{"tag":"button","value":{"loopSeq":"94","answer":"keep","executionId":"agent-g","askedAt":"2026-04-11T12:00:00.000Z"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"delivered":false`) {
		t.Fatalf("body = %s, want delivered:false for github transport button", recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want still awaiting_human", loop.Status)
	}
}
