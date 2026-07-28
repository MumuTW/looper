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

func TestHandlerFeishuThreadReplyQueuesConversationalText(t *testing.T) {
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

	// Free-text in the shared thread is conversational — must not resolve the ask
	// (generation-bound card buttons / dashboard /respond remain the answer path).
	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_reply","root_id":"om_root_91","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"用 A 改 resize handle\"}"},"sender":{"sender_type":"user","sender_id":{"open_id":"ou_user"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"queued":true`) {
		t.Fatalf("body = %s, want queued:true for free-text", recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), "loop_thread")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want still awaiting_human (free-text is not a final answer)", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || strings.TrimSpace(ask.Answer) != "" {
		t.Fatalf("ask = %#v (ok=%v), want unanswered awaiting park", ask, ok)
	}
	inbox := loops.ReadHumanInbox(loop.MetadataJSON)
	if len(inbox) != 1 || inbox[0].Text != "用 A 改 resize handle" {
		t.Fatalf("human inbox = %#v, want queued free-text", inbox)
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

// Notify-only Feishu cards (answerTransport=github) must not accept free-text
// thread replies: that would bypass GitHub answerAuthors authority.

func TestHandlerFeishuThreadReplyRejectsGitHubTransport(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.HITL.Enabled = true
	t.Setenv("LOOPER_TEST_FEISHU_VTOKEN5", "verify-tok-123")
	cfg.Notifications.Webhook.VerificationTokenEnv = "LOOPER_TEST_FEISHU_VTOKEN5"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_notify_only"
	loopID := "loop_notify_only"
	targetID := projectID
	// transport=github marks the parked ask as notify-only on Feishu.
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting","transport":"github","prNumber":42,"askCommentId":9001}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 93, ProjectID: projectID, Type: "fixer", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := services.Repositories.FeishuThreads.Upsert(context.Background(), "om_root_93", loopID, "oc_group", nowISO); err != nil {
		t.Fatalf("FeishuThreads.Upsert() error = %v", err)
	}

	body := `{"schema":"2.0","header":{"event_type":"im.message.receive_v1","token":"verify-tok-123"},"event":{"message":{"message_id":"om_reply","root_id":"om_root_93","chat_id":"oc_group","message_type":"text","content":"{\"text\":\"bypass via feishu\"}"},"sender":{"sender_type":"user","sender_id":{"open_id":"ou_user"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"delivered":false`) {
		t.Fatalf("body = %s, want delivered:false for github transport", recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "awaiting_human" {
		t.Fatalf("loop.Status = %q, want still awaiting_human", loop.Status)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || strings.TrimSpace(ask.Answer) != "" {
		t.Fatalf("ask = %#v, want unanswered awaiting state", ask)
	}
}
