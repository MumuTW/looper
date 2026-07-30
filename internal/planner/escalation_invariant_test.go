package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
)

// requeueForResume mirrors what POST /loops/{seq}/respond does after it has
// persisted an answer: the loop goes back to running and the cancelled queue
// item is requeued.
func (h *escalationHarness) requeueForResume(t *testing.T) {
	t.Helper()
	loop := h.loop(t)
	loop.Status = "running"
	loop.UpdatedAt = h.fixture.nowISO()
	if err := h.fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if _, err := h.fixture.repos.Queue.RequeueLatestCancelledByLoop(context.Background(), h.loopID, h.fixture.nowISO()); err != nil {
		t.Fatalf("RequeueLatestCancelledByLoop() error = %v", err)
	}
}

func (h *escalationHarness) latestCheckpointScope(t *testing.T) *checkpointScope {
	t.Helper()
	run, err := h.fixture.repos.Runs.GetLatestByLoopID(context.Background(), h.loopID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetLatestByLoopID() = (%#v, %v)", run, err)
	}
	return parseCheckpoint(run.CheckpointJSON).Scope
}

func (h *escalationHarness) checkpointScopeForRun(t *testing.T, runID string) *checkpointScope {
	t.Helper()
	run, err := h.fixture.repos.Runs.GetByID(context.Background(), runID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID(%s) = (%#v, %v)", runID, run, err)
	}
	return parseCheckpoint(run.CheckpointJSON).Scope
}

// TestPlannerNeverAdvancesOnAnEscalationWithNoRecordedDecision covers the crash
// window the review named: the answer was consumed but the checkpoint carrying
// the decision never landed. Advancing from the surviving Assessed/Escalated
// scope would turn a human `stop` into a proceed.
func TestPlannerNeverAdvancesOnAnEscalationWithNoRecordedDecision(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true,
		AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON},
		AgentResult{Status: "completed", Summary: "wrote spec"},
	)
	if processed := harness.process(t); processed.Status != "awaiting_human" {
		t.Fatalf("first process = %#v", processed)
	}

	// Reproduce the half-applied state: the durable HITL record says the human
	// answered "stop" and the answer was consumed, while the run checkpoint
	// still holds only the escalated scope.
	loop := harness.loop(t)
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		t.Fatal("loop carries no HITL ask")
	}
	ask.Answer = "stop — this needs a design discussion first"
	ask.Status = "consumed"
	ask.AnsweredAt = harness.fixture.nowISO()
	metadataJSON, err := loops.WriteHITLAsk(loop.MetadataJSON, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk() error = %v", err)
	}
	loop.MetadataJSON = &metadataJSON
	if err := harness.fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if scope := harness.latestCheckpointScope(t); scope == nil || scope.HumanDecision != "" {
		t.Fatalf("checkpoint scope = %#v, want the escalated scope with no decision", scope)
	}
	harness.requeueForResume(t)

	processed := harness.process(t)
	if processed.Status != "awaiting_human" {
		t.Fatalf("resumed process = %#v, want the loop to park again rather than proceed", processed)
	}
	if len(harness.executor.starts) != 1 {
		t.Fatalf("agent turns = %d, want no spec authoring — the human's stop must not become a proceed", len(harness.executor.starts))
	}
	if len(harness.github.createPRCalls) != 0 || len(harness.git.pushCalls) != 0 {
		t.Fatal("a lost stop decision published a spec PR")
	}
	if harness.loop(t).Status != "awaiting_human" {
		t.Fatalf("loop status = %s, want awaiting_human", harness.loop(t).Status)
	}
}

// TestPlannerEscalationDecisionIsDurableBeforeAnyFurtherWork asserts the write
// ordering directly: by the time the spec turn is allowed to start, both halves
// of the resume — the decision on the checkpoint and the consumed ask — are
// already persisted.
func TestPlannerEscalationDecisionIsDurableBeforeAnyFurtherWork(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true,
		AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON},
		AgentResult{Status: "completed", Summary: "wrote spec"},
	)
	harness.process(t)
	harness.answerEscalation(t, "proceed, but keep the change inside internal/planner")

	var scopeAtSpecTurn *checkpointScope
	var askAtSpecTurn loops.HITLAsk
	harness.executor.onStart = func(input AgentRunInput) {
		if !strings.HasPrefix(input.IdempotencyKey, "planner:") {
			return
		}
		scopeAtSpecTurn = harness.checkpointScopeForRun(t, input.RunID)
		askAtSpecTurn, _ = loops.ReadHITLAsk(harness.loop(t).MetadataJSON)
	}

	if processed := harness.process(t); processed.Status != "success" {
		t.Fatalf("resumed process = %#v", processed)
	}
	if scopeAtSpecTurn == nil {
		t.Fatal("spec turn never started")
	}
	if !scopeAtSpecTurn.HumanAuthorized || !strings.Contains(scopeAtSpecTurn.HumanDecision, "internal/planner") {
		t.Fatalf("checkpoint scope at spec turn = %#v, want the human decision already durable", scopeAtSpecTurn)
	}
	if askAtSpecTurn.Status != "consumed" {
		t.Fatalf("ask status at spec turn = %q, want consumed", askAtSpecTurn.Status)
	}
}

// TestPlannerScopeAssessmentRejectsAWorktreeItModified covers the injection
// path: the Issue body is untrusted, so an assessment that writes into the
// worktree would have its files committed by write-spec and pushed by publish.
func TestPlannerScopeAssessmentRejectsAWorktreeItModified(t *testing.T) {
	t.Parallel()

	cases := map[string]InspectHeadResult{
		"uncommitted changes": {HasUncommittedChanges: true, ChangedFiles: []string{"internal/planner/backdoor.go"}},
		"new commits":         {NewCommitSHAs: []string{"deadbeef"}},
	}
	for name, inspect := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			harness := newEscalationHarness(t, true,
				AgentResult{Status: "completed", Stdout: benignAssessmentJSON},
				AgentResult{Status: "completed", Summary: "wrote spec"},
			)
			harness.git.inspectResult = inspect

			processed := harness.process(t)
			if processed.Status != "failed" {
				t.Fatalf("processed = %#v, want a loud failure", processed)
			}
			if processed.FailureKind != FailureManualIntervention {
				t.Fatalf("failureKind = %q, want manual_intervention", processed.FailureKind)
			}
			if !strings.Contains(processed.Summary, "modified its worktree") {
				t.Fatalf("summary = %q, want a distinct worktree-mutation reason", processed.Summary)
			}
			if len(harness.executor.starts) != 1 {
				t.Fatalf("agent turns = %d, want the run stopped before spec authoring", len(harness.executor.starts))
			}
			if len(harness.git.commitCalls) != 0 || len(harness.git.pushCalls) != 0 || len(harness.github.createPRCalls) != 0 {
				t.Fatal("assessment-authored changes reached commit/push/PR")
			}
		})
	}
}

func TestPlannerScopeAssessmentAcceptsAnUntouchedWorktree(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true,
		AgentResult{Status: "completed", Stdout: benignAssessmentJSON},
		AgentResult{Status: "completed", Summary: "wrote spec"},
	)
	if processed := harness.process(t); processed.Status != "success" {
		t.Fatalf("processed = %#v, want a clean worktree to plan normally", processed)
	}
	if len(harness.git.inspectCalls) < 2 {
		t.Fatalf("InspectHead calls = %d, want the assessment verified separately from write-spec", len(harness.git.inspectCalls))
	}
}

// TestPlannerRechecksHoldLabelAppliedDuringScopeAssessment: a hold applied while
// the assessment turn runs must settle the Issue, not strand it in
// awaiting_human on a decision nobody should have been asked for.
func TestPlannerRechecksHoldLabelAppliedDuringScopeAssessment(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true, AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON})
	harness.executor.onStart = func(input AgentRunInput) {
		harness.github.issueDetail.Labels = []string{"looper:hold"}
	}

	processed := harness.process(t)
	if processed.Status != "skipped" {
		t.Fatalf("processed = %#v, want the held Issue settled rather than escalated", processed)
	}
	if !strings.Contains(processed.Summary, "currently held") {
		t.Fatalf("summary = %q, want it to name the hold", processed.Summary)
	}
	loop := harness.loop(t)
	if loop.Status == "awaiting_human" {
		t.Fatal("a held Issue was parked awaiting a human decision")
	}
	if _, ok := loops.ReadHITLAsk(loop.MetadataJSON); ok {
		t.Fatal("a held Issue still raised a HITL ask")
	}
}

// TestPlannerReassessesWhenIssueChangedWhileAwaitingHuman: a decision made days
// later must not author a spec from the Issue text as it read back then.
func TestPlannerReassessesWhenIssueChangedWhileAwaitingHuman(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true,
		AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON},
		AgentResult{Status: "completed", Stdout: benignAssessmentJSON},
		AgentResult{Status: "completed", Summary: "wrote spec"},
	)
	harness.process(t)
	harness.answerEscalation(t, "proceed")

	harness.github.issueDetail.Title = "Planner needs-human exit (rescoped)"
	harness.github.issueDetail.Body = "scope cut to internal/planner only"

	processed := harness.process(t)
	if processed.Status != "success" {
		t.Fatalf("resumed process = %#v", processed)
	}
	if len(harness.executor.starts) != 3 {
		t.Fatalf("agent turns = %d, want the scope reassessed against the edited Issue before spec authoring", len(harness.executor.starts))
	}
	for index, start := range harness.executor.starts[1:] {
		if !strings.Contains(start.Prompt, "scope cut to internal/planner only") {
			t.Fatalf("turn %d ran against the stale Issue body:\n%s", index+1, start.Prompt)
		}
		if strings.Contains(start.Prompt, "give planner a durable exit") {
			t.Fatalf("turn %d carried the superseded Issue body:\n%s", index+1, start.Prompt)
		}
	}
	if strings.Contains(harness.executor.starts[2].Prompt, "HUMAN AUTHORIZATION") {
		t.Fatal("a superseded authorization was carried into the spec prompt")
	}
	if !supersededResolutionRecorded(t, harness) {
		t.Fatal("no planner.escalation.resolved event records the superseded answer")
	}
}

// TestPlannerReassessesWhenBaseChangesWhileAwaitingHuman ensures the human is
// never asked to authorize an assessment made against an older repository.
func TestPlannerReassessesWhenBaseChangesWhileAwaitingHuman(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true,
		AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON},
		AgentResult{Status: "completed", Stdout: benignAssessmentJSON},
		AgentResult{Status: "completed", Summary: "wrote spec"},
	)
	harness.git.inspectResult.HeadSHA = "base-before"
	if processed := harness.process(t); processed.Status != "awaiting_human" {
		t.Fatalf("first process = %#v", processed)
	}
	harness.answerEscalation(t, "proceed")
	harness.git.refreshResult.HeadSHA = "base-after"

	if processed := harness.process(t); processed.Status != "success" {
		t.Fatalf("resumed process = %#v", processed)
	}
	if len(harness.executor.starts) != 3 {
		t.Fatalf("agent turns = %d, want reassessment then spec after base drift", len(harness.executor.starts))
	}
	if len(harness.git.refreshCalls) != 1 {
		t.Fatalf("worktree refreshes = %d, want one before applying the authorization", len(harness.git.refreshCalls))
	}
}

func supersededResolutionRecorded(t *testing.T, harness *escalationHarness) bool {
	t.Helper()
	events, err := harness.fixture.repos.Events.ListByEntity(context.Background(), "loop", harness.loopID)
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	for _, event := range events {
		if event.EventType != EscalationResolutionEventType {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			t.Fatalf("unmarshal resolution payload error = %v", err)
		}
		if superseded, _ := payload["superseded"].(bool); superseded {
			return true
		}
	}
	return false
}

// TestPlannerRefusesToParkWhenTheEscalationRecordCannotBePersisted: the
// planner.escalation event is the authority for the suspension, so a failed
// write must fail the run rather than park awaiting_human with no record.
func TestPlannerRefusesToParkWhenTheEscalationRecordCannotBePersisted(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true, AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON})
	harness.fixture.repos.Events = nil

	claim, err := harness.fixture.repos.Queue.ClaimNextOfType(context.Background(), harness.fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
	}
	if _, err := harness.runner.ProcessClaimedItem(context.Background(), *claim); err == nil {
		t.Fatal("ProcessClaimedItem() error = nil, want the failed authority write surfaced")
	} else if !strings.Contains(err.Error(), EscalationEventType) {
		t.Fatalf("error = %v, want it to name the escalation record", err)
	}
	if loop := harness.loop(t); loop.Status == "awaiting_human" {
		t.Fatal("loop parked awaiting_human with no durable planner.escalation record")
	}
}
