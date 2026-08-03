package fixer

import (
	"context"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/loops/runpipe"
	"github.com/MumuTW/looper/internal/storage"
)

// TestRegenerationWithheldReplayCompletesClaimedQueueItem drives the durable
// omitted-comment evidence through the queue runner. The first claimed replay
// fails after the event append but before Commented=true is checkpointed; the
// handoff must requeue the claim, and the next claim must consume the event,
// finish close-and-regenerate, and complete the queue row without reposting
// either body.
func TestRegenerationWithheldReplayCompletesClaimedQueueItem(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
		commentErrs:       []error{contentGateRejection(), contentGateRejection()},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	metadata := `{"sourceWorkerId":"worker_origin","fixerRegeneration":{"authority":"fixer-exhaustion:fixer_exhausted","failureSummary":"failed","failureKind":"retryable_transient","failureContext":"secret-shaped context"}}`
	loop.MetadataJSON = runpipe.StringPtr(metadata)
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert(replay state) error = %v", err)
	}
	queue.Status = "running"
	queue.Priority = storage.QueuePriorityFixer
	queue.AvailableAt = fixture.nowISO()
	queue.DedupeKey = "fixer:regeneration-withheld-replay"
	if err := fixture.repos.Queue.Upsert(context.Background(), queue); err != nil {
		t.Fatalf("Queue.Upsert(claimed replay) error = %v", err)
	}

	ctx := context.Background()
	if _, err := fixture.coordinator.DB().ExecContext(ctx, `
		CREATE TRIGGER fixer_regeneration_replay_checkpoint_failure
		BEFORE UPDATE OF metadata_json ON loops
		WHEN OLD.id = 'fixer_exhausted' AND instr(NEW.metadata_json, '"commented":true') > 0
		BEGIN SELECT RAISE(FAIL, 'injected regeneration replay checkpoint failure'); END;
	`); err != nil {
		t.Fatalf("create replay checkpoint failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fixture.coordinator.DB().ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fixer_regeneration_replay_checkpoint_failure`)
	})

	first, err := runner.ProcessClaimedQueueItem(ctx, queue)
	if err != nil {
		persisted, _ := fixture.repos.Loops.GetByID(ctx, loop.ID)
		t.Fatalf("first ProcessClaimedQueueItem() error = %v (comments=%d loopMetadata=%q queue=%#v)", err, len(gateway.createIssueComments), derefString(persisted.MetadataJSON), func() any { q, _ := fixture.repos.Queue.GetByID(ctx, queue.ID); return q }())
	}
	if first == nil || first.Status != "queued" {
		t.Fatalf("first replay result = %#v, want queued handoff", first)
	}
	persistedQueue, err := fixture.repos.Queue.GetByID(ctx, queue.ID)
	if err != nil || persistedQueue == nil || persistedQueue.Status != "queued" {
		t.Fatalf("queue after checkpoint failure = %#v err=%v, want queued", persistedQueue, err)
	}
	if got := len(gateway.createIssueComments); got != 2 {
		t.Fatalf("first replay comment attempts = %d, want rejected detailed and fallback bodies", got)
	}

	if _, err := fixture.coordinator.DB().ExecContext(ctx, `DROP TRIGGER IF EXISTS fixer_regeneration_replay_checkpoint_failure`); err != nil {
		t.Fatalf("drop replay checkpoint failure trigger: %v", err)
	}
	fixture.advance(6 * time.Second)
	claimed, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "regeneration-replay", "fixer")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = %#v err=%v, want replay claim", claimed, err)
	}
	second, err := runner.ProcessClaimedQueueItem(ctx, *claimed)
	if err != nil {
		t.Fatalf("second ProcessClaimedQueueItem() error = %v", err)
	}
	if second == nil || second.Status != "failed" {
		t.Fatalf("second replay result = %#v, want failed terminal handoff", second)
	}
	persistedQueue, err = fixture.repos.Queue.GetByID(ctx, claimed.ID)
	if err != nil || persistedQueue == nil || persistedQueue.Status != "completed" {
		t.Fatalf("queue after replay completion = %#v err=%v, want completed", persistedQueue, err)
	}
	if got := len(gateway.createIssueComments); got != 2 {
		t.Fatalf("replay comment attempts = %d, want no duplicate bodies", got)
	}
	events, err := fixture.repos.Events.ListByEntityAndEventTypes(ctx, "loop", loop.ID, []string{regenerationCommentWithheldEventType})
	if err != nil || len(events) != 1 {
		t.Fatalf("context-withheld events = %#v err=%v, want one durable marker", events, err)
	}
	durable, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || durable == nil {
		t.Fatalf("Loops.GetByID() after replay = %#v err=%v", durable, err)
	}
	state, ok := parseRegenerationState(parseJSONObject(durable.MetadataJSON))
	if !ok || !state.Commented || !state.Closed || !state.Routed {
		t.Fatalf("regeneration state after replay = %#v, want Commented/Closed/Routed", state)
	}
}

// TestHandleTerminalExhaustionIgnoresStaleRegenerationCommentEvidence ensures
// a marker with the expected deterministic ID but a different target/payload
// cannot settle a later normal regeneration comment step.
func TestHandleTerminalExhaustionIgnoresStaleRegenerationCommentEvidence(t *testing.T) {
	fixture := newRunnerFixture(t)
	gateway := &regenerationFakeGateway{
		fakeGitHubGateway: &fakeGitHubGateway{currentUser: "looper", viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadRefName: "looper/fix-42", HeadSHA: "head-42"}}},
		issue:             IssueDetail{Number: 7, State: "OPEN"},
		commits:           []PullRequestCommit{{AuthorLogin: "looper", CommitterLogin: "looper"}},
	}
	runner := newRegenerationRunner(t, fixture, gateway, func(_ context.Context, _ RegenerateIssueInput) error { return nil })
	loop, queue := setupRegenerationRecords(t, fixture)
	ctx := context.Background()
	authority := "fixer-exhaustion:" + loop.ID
	if err := eventlog.Append(ctx, fixture.repos, eventlog.AppendInput{
		ID:         regenerationCommentWithheldEventID(loop.ID, "acme/looper", 42, authority),
		EventType:  regenerationCommentWithheldEventType,
		ProjectID:  runpipe.OptionalString(loop.ProjectID),
		LoopID:     runpipe.OptionalString(loop.ID),
		EntityType: runpipe.OptionalString("loop"),
		EntityID:   runpipe.OptionalString(loop.ID),
		Payload:    map[string]any{"repo": "stale/looper", "prNumber": int64(41), "authority": "stale-authority"},
		CreatedAt:  fixture.now(),
	}); err != nil {
		t.Fatalf("append stale regeneration withheld event: %v", err)
	}

	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
	action, err := runner.handleTerminalExhaustion(ctx, *project, loop, queue, fixerCheckpoint{}, &runpipe.LoopError{Message: "failed", Kind: runpipe.FailureRetryableTransient})
	if err != nil || action != regenerationCompleted {
		t.Fatalf("handleTerminalExhaustion() = action %q err %v, want normal regeneration completion", action, err)
	}
	if got := len(gateway.createIssueComments); got != 1 {
		t.Fatalf("comment attempts = %d, want one comment despite stale evidence", got)
	}
	durable, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || durable == nil {
		t.Fatalf("Loops.GetByID() after regeneration = %#v err=%v", durable, err)
	}
	state, ok := parseRegenerationState(parseJSONObject(durable.MetadataJSON))
	if !ok || !state.Commented || !state.Closed || !state.Routed {
		t.Fatalf("regeneration state = %#v, want Commented/Closed/Routed after ignoring stale evidence", state)
	}
}
