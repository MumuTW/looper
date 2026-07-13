package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// newEnqueueTestRepos opens an in-memory storage coordinator with migrations
// applied, for exercising enqueueHumanMessageToLoop end to end.
func newEnqueueTestRepos(t *testing.T) *storage.Repositories {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "hitl.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner().RunPending() error = %v", err)
	}
	return storage.NewRepositories(coordinator.DB())
}

func strptr(s string) *string { return &s }

// TestEnqueueHumanMessageReactivatesCompletedWorkerLoop asserts the core of
// 线程永远可追问: a reply to a COMPLETED worker loop that has a captured native agent
// session is recorded, the loop is flipped back to queued, and a fresh active queue
// item is created so the scheduler re-dispatches it (RequeueLatestCancelledByLoop
// alone can't — the loop's only queue item is 'completed', not 'cancelled').
func TestEnqueueHumanMessageReactivatesCompletedWorkerLoop(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	nowISO := "2026-07-13T00:00:00.000Z"
	projectID := "project_1"
	loopID := "loop_worker_done"
	target := "issue:acme/looper:27"

	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "issue", TargetID: &target, Repo: strptr("acme/looper"), Status: "completed", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// A captured native session is what makes reactivation safe (native resume, no re-impl).
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "agent_1", ProjectID: &projectID, LoopID: &loopID, Vendor: "opencode", Status: "completed", NativeSessionID: strptr("sess_abc"), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	// The loop's only queue item is 'completed' — there is nothing cancelled to revive.
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_done", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "issue", TargetID: target, Repo: strptr("acme/looper"), DedupeKey: "worker:project_1:acme/looper:27", Priority: 1, Status: "completed", AvailableAt: nowISO, MaxAttempts: 3, FinishedAt: strptr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	ackClosed, err := enqueueHumanMessageToLoop(ctx, repos, nowISO, loopID, "还能再改改这个 resize handle 吗?")
	if err != nil {
		t.Fatalf("enqueueHumanMessageToLoop() error = %v", err)
	}
	if ackClosed {
		t.Fatal("ackClosed = true, want false (a resumable completed worker is reactivated, not acked)")
	}

	got, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID() error = %v, loop = %v", err, got)
	}
	if got.Status != "queued" {
		t.Fatalf("loop.Status = %q, want queued", got.Status)
	}
	inbox := loops.ReadHumanInbox(got.MetadataJSON)
	if len(inbox) != 1 || inbox[0].Text != "还能再改改这个 resize handle 吗?" {
		t.Fatalf("human inbox = %+v, want the recorded message", inbox)
	}
	active, err := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active == nil {
		t.Fatal("no active queue item after reactivation; the loop would never be dispatched")
	}
	if active.Status != "queued" {
		t.Fatalf("active queue item status = %q, want queued", active.Status)
	}
	if active.ID == "queue_done" {
		t.Fatal("expected a NEW queue item, not the reused completed one")
	}
}

// TestEnqueueHumanMessageAcksUnresumableFinishedLoop asserts that a finished loop
// that cannot be safely resumed (here: no captured native session) is NOT
// reactivated into a dangerous full re-run — the message is recorded and the caller
// is told to post the honest closed-task ack.
func TestEnqueueHumanMessageAcksUnresumableFinishedLoop(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	nowISO := "2026-07-13T00:00:00.000Z"
	projectID := "project_1"
	loopID := "loop_planner_done"

	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	// Planner loop with NO captured session — native resume is impossible, so reviving
	// it would re-author the spec from scratch. Must fall back to the honest ack.
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "completed", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_planner_done", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: "issue:acme/looper:9", DedupeKey: "planner:project_1:acme/looper:9", Priority: 1, Status: "completed", AvailableAt: nowISO, MaxAttempts: 3, FinishedAt: strptr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	ackClosed, err := enqueueHumanMessageToLoop(ctx, repos, nowISO, loopID, "再补一版?")
	if err != nil {
		t.Fatalf("enqueueHumanMessageToLoop() error = %v", err)
	}
	if !ackClosed {
		t.Fatal("ackClosed = false, want true (unresumable finished loop should get the honest ack)")
	}

	got, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID() error = %v, loop = %v", err, got)
	}
	if got.Status != "completed" {
		t.Fatalf("loop.Status = %q, want completed (not reactivated)", got.Status)
	}
	if inbox := loops.ReadHumanInbox(got.MetadataJSON); len(inbox) != 1 {
		t.Fatalf("human inbox = %+v, want the message still recorded (never silently dropped)", inbox)
	}
	active, err := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active != nil {
		t.Fatalf("active queue item = %+v, want none (unresumable loop must not be re-dispatched)", active)
	}
}

// TestEnqueueHumanMessageReactivatesCompletedPlannerLoop asserts the planner side of
// 线程永远可追问: a product answer replied on a COMPLETED planner loop (审批门 loops are
// completed, not awaiting_human) that has a captured native session is recorded, the
// loop flips back to queued, and a fresh active queue item is created so the planner
// re-dispatches and native-resumes the spec session.
func TestEnqueueHumanMessageReactivatesCompletedPlannerLoop(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	nowISO := "2026-07-13T00:00:00.000Z"
	projectID := "project_1"
	loopID := "loop_planner_session"

	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Looper", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 3, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "completed", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	// A captured planner session makes the follow-up resume safe (native resume, no re-plan).
	if err := repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "agent_p1", ProjectID: &projectID, LoopID: &loopID, Vendor: "opencode", Status: "completed", NativeSessionID: strptr("sess_planner"), StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_planner_completed", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: "issue:acme/looper:42", DedupeKey: "planner:project_1:acme/looper:42", Priority: 1, Status: "completed", AvailableAt: nowISO, MaxAttempts: 3, FinishedAt: strptr(nowISO), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	ackClosed, err := enqueueHumanMessageToLoop(ctx, repos, nowISO, loopID, "产品答案:导出优先 PDF")
	if err != nil {
		t.Fatalf("enqueueHumanMessageToLoop() error = %v", err)
	}
	if ackClosed {
		t.Fatal("ackClosed = true, want false (a resumable completed planner is reactivated, not acked)")
	}
	got, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID() error = %v, loop = %v", err, got)
	}
	if got.Status != "queued" {
		t.Fatalf("loop.Status = %q, want queued", got.Status)
	}
	if inbox := loops.ReadHumanInbox(got.MetadataJSON); len(inbox) != 1 {
		t.Fatalf("human inbox = %+v, want the recorded product answer", inbox)
	}
	active, err := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil {
		t.Fatalf("FindActiveByLoopID() error = %v", err)
	}
	if active == nil || active.Status != "queued" {
		t.Fatalf("active queue item = %+v, want a fresh queued item so the scheduler re-dispatches", active)
	}
	if active.ID == "queue_planner_completed" {
		t.Fatal("expected a NEW queue item, not the reused completed one")
	}
}

func TestDetectGitHubHITLAnswer(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 100, Author: "lefarcen", Body: "<!-- looper:hitl:ask v=1 --> which one?"}, // the ask (bot marker), == askCommentID
		{ID: 101, Author: "lefarcen", Body: "<!-- looper:stamp --> still working"},     // bot marker, ignored even if same login
		{ID: 105, Author: "lefarcen", Body: "用 A,改 resize handle"},                     // human reply, no marker -> first answer
		{ID: 110, Author: "someoneelse", Body: "later comment"},
	}
	// First non-looper comment after the ask wins — even though the bot and human
	// share the "lefarcen" account, the marker distinguishes them.
	if got := detectGitHubHITLAnswer(comments, 100, nil); got != "用 A,改 resize handle" {
		t.Fatalf("answer = %q, want the first human reply", got)
	}
	// Only the ask + a marked bot note -> no answer yet.
	if got := detectGitHubHITLAnswer(comments[:2], 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty (no human reply yet)", got)
	}
	// Allowlist excludes lefarcen -> the next allowed author answers.
	if got := detectGitHubHITLAnswer(comments, 100, []string{"someoneelse"}); got != "later comment" {
		t.Fatalf("answer = %q, want the allowlisted author's comment", got)
	}
	// A looper-marked comment after the ask is never an answer.
	marked := []githubAnswerComment{{ID: 200, Author: "lefarcen", Body: "<!-- looper:decision-log --> recorded"}}
	if got := detectGitHubHITLAnswer(marked, 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty (looper's own comment)", got)
	}
}

func TestPollGitHubHITLAnswersOnce(t *testing.T) {
	commentsByPR := map[int64][]githubAnswerComment{
		42: {{ID: 500, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}, {ID: 501, Author: "lefarcen", Body: "go with A"}},
		43: {{ID: 600, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}}, // no human reply yet
	}
	var deliveredTo []string
	var cleared []int64
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, pr int64, _ string) ([]githubAnswerComment, error) {
			return commentsByPR[pr], nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			deliveredTo = append(deliveredTo, loopID+"="+answer)
			return nil
		},
		clearAwaiting: func(_ contextType, _ string, pr int64, _ string) { cleared = append(cleared, pr) },
		projectCWD:    func(string) string { return "/tmp/repo" },
	}
	loops := []githubHITLAwaitingLoop{
		{ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500},
		{ID: "loop-b", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 43, AskCommentID: 600},
		{ID: "loop-c", Repo: "acme/x", Transport: "feishu", PRNumber: 44}, // non-github, skipped
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), loops, deps)
	if n != 1 {
		t.Fatalf("delivered = %d, want 1", n)
	}
	if len(deliveredTo) != 1 || deliveredTo[0] != "loop-a=go with A" {
		t.Fatalf("deliveredTo = %v, want [loop-a=go with A]", deliveredTo)
	}
	if len(cleared) != 1 || cleared[0] != 42 {
		t.Fatalf("cleared = %v, want [42]", cleared)
	}
}
