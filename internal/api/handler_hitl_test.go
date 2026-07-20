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
	cancelReason := "loop suspended awaiting human"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/71/respond", strings.NewReader(`{"answer":"continue with the redis approach"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("resumed loop = %#v, %v", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Answer != "continue with the redis approach" || ask.Status != "answered" || ask.SessionID != "sess-abc" {
		t.Fatalf("answer metadata = %#v, %v", ask, ok)
	}
	active, err := services.Repositories.Queue.FindActiveByLoopID(context.Background(), loopID)
	if err != nil || active == nil || active.Status != "queued" {
		t.Fatalf("resumed queue = %#v, %v", active, err)
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

func TestHandlerFeishuInboundRouteIsRemoved(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hitl/feishu", strings.NewReader(`{"type":"url_verification","challenge":"must-not-echo"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 after removing Feishu inbound collaboration", recorder.Code)
	}
}
