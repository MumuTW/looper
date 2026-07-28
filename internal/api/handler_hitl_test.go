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
	metadata := `{"hitl":{"question":"q","sessionId":"sess-1","status":"awaiting","transport":"feishu"}}`

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
	metadata := `{"hitl":{"question":"q","sessionId":"sess-mark","status":"awaiting","transport":"feishu"}}`
	if err := services.Repositories.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/repos/looper", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := services.Repositories.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 88, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	body := `{"token":"verify-tok-mark","action":{"tag":"button","value":{"loopSeq":"88","answer":"postgres"}}}`
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

	body := `{"token":"verify-tok-123","action":{"tag":"button","value":{"loopSeq":"94","answer":"keep"}}}`
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
