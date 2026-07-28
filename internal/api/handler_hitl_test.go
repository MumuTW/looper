package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
	"github.com/nexu-io/looper/internal/storage"
)

type runtimeHITLAnswerProbe struct {
	RuntimeState
	onMark func(context.Context, string, string)
}

func (r *runtimeHITLAnswerProbe) MarkHITLAskAnswered(ctx context.Context, loopID, answer string) {
	if r.onMark != nil {
		r.onMark(ctx, loopID, answer)
	}
}

// setupAwaitingCardLoop seeds a project + awaiting_human loop and returns the
// handler + services for card-action security tests. Parks are stamped
// transport=feishu so Feishu answers are the configured authority.

func setupAwaitingCardLoop(t *testing.T, cfg config.Config, rt *looperdruntime.Runtime, projectID, loopID string, seq int64) *Handler {
	t.Helper()
	cfg.HITL.AnswerTransport = "feishu"
	h := NewHandler(Context{Config: cfg, Runtime: runtimeWithConfig(rt, cfg)})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	targetID := projectID
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting","transport":"feishu"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: seq, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return h
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/71/respond", strings.NewReader(`{"answer":"continue with the redis approach","executionId":"agent-1","askedAt":"2026-04-11T11:59:00.000Z"}`))
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

// Dashboard /respond must clear the awaiting-human PR label for transport=github
// asks — the poll lane skips now-running loops so labels would otherwise stick.

func TestHandlerRespondClearsGitHubAwaitingLabel(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_label"
	loopID := "loop_hitl_label"
	repo := "acme/looper"
	pr := int64(42)
	metadata := `{"hitl":{"question":"Pick?","options":["a","b"],"status":"awaiting","transport":"github","provider":"github","prNumber":42,"askCommentId":99,"sessionId":"sess-label","vendor":"codex"}}`

	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{
		ID: loopID, Seq: 74, ProjectID: projectID, Type: "fixer", TargetType: "pull_request",
		Repo: &repo, PRNumber: &pr, Status: "awaiting_human", MetadataJSON: &metadata,
		CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	cancelReason := "loop suspended awaiting human"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: "queue_hitl_label", ProjectID: &projectID, LoopID: &loopID, Type: "fixer",
		TargetType: "pull_request", Repo: &repo, PRNumber: &pr, DedupeKey: "fixer:hitl-label",
		Priority: storage.QueuePriorityFixer, Status: "cancelled", AvailableAt: nowISO,
		Attempts: 0, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	var clearedRepos []struct {
		Repo  string
		PR    int64
		Label string
	}
	prev := hitlAwaitingLabelClearer
	t.Cleanup(func() { hitlAwaitingLabelClearer = prev })
	hitlAwaitingLabelClearer = func(_ context.Context, _ *config.Config, _ *githubinfra.Gateway, repo string, prNumber int64, _ string, label string) error {
		clearedRepos = append(clearedRepos, struct {
			Repo  string
			PR    int64
			Label string
		}{Repo: repo, PR: prNumber, Label: label})
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/74/respond", strings.NewReader(`{"answer":"option a"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(clearedRepos) != 1 {
		t.Fatalf("label cleanup calls = %d, want 1", len(clearedRepos))
	}
	if clearedRepos[0].Repo != "acme/looper" || clearedRepos[0].PR != 42 {
		t.Fatalf("cleanup target = %#v, want acme/looper#42", clearedRepos[0])
	}
	if clearedRepos[0].Label != "looper:awaiting-human" {
		t.Fatalf("cleanup label = %q, want looper:awaiting-human", clearedRepos[0].Label)
	}

	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("loop after respond = %#v err=%v, want running", loop, err)
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/79/respond", strings.NewReader(`{"answer":"yes continue","askedAt":"2026-04-11T11:59:00.000Z"}`))
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

func TestHandlerRespondRejectsStaleAskGeneration(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_stale_gen"
	loopID := "loop_hitl_stale_gen"
	targetID := projectID
	// Current park is gen-2; a stale dashboard card still shows gen-1.
	metadata := `{"hitl":{"question":"Which direction?","options":["a","b"],"sessionId":"sess","executionId":"agent-2","vendor":"codex","status":"awaiting","askedAt":"2026-04-11T12:00:00.000Z"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 94, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/94/respond", strings.NewReader(`{"answer":"a","executionId":"agent-1","askedAt":"2026-04-11T11:00:00.000Z"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 stale generation; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "awaiting_human" {
		t.Fatalf("loop = %#v err=%v, want still awaiting_human", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "awaiting" || strings.TrimSpace(ask.Answer) != "" {
		t.Fatalf("ask = %#v, want unanswered", ask)
	}
}

// Answer-only /respond remains usable when the park carries generation tokens
// (answerTransport=respond documented contract).

func TestHandlerRespondAnswerOnlyAcceptsCurrentPark(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})
	services := rt.Services()
	nowISO := "2026-04-11T12:00:00.000Z"
	projectID := "project_hitl_answer_only"
	loopID := "loop_hitl_answer_only"
	targetID := projectID
	metadata := `{"hitl":{"question":"Which direction?","options":["a","b"],"sessionId":"sess","executionId":"agent-cur","vendor":"codex","status":"awaiting","askedAt":"2026-04-11T12:00:00.000Z","transport":"respond"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 95, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	cancelReason := "loop suspended awaiting human"
	if err := services.Repositories.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_hitl_answer_only", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:hitl-answer-only", Priority: storage.QueuePriorityWorker, Status: "cancelled", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LastError: &cancelReason, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/loops/95/respond", strings.NewReader(`{"answer":"option a"}`))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for answer-only; body=%s", recorder.Code, recorder.Body.String())
	}
	loop, err := services.Repositories.Loops.GetByID(context.Background(), loopID)
	if err != nil || loop == nil || loop.Status != "running" {
		t.Fatalf("loop = %#v err=%v, want running", loop, err)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "answered" || ask.Answer != "option a" {
		t.Fatalf("ask = %#v, want answered option a", ask)
	}
}

// Feishu card buttons must not authorize answerTransport=respond parks.

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
