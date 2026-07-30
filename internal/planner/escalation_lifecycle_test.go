package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/loops"
)

func TestPlannerEscalationParksLoopAwaitingHumanBeforeSpecAuthoring(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true, AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON})

	processed := harness.process(t)
	if processed.Status != "awaiting_human" {
		t.Fatalf("processed = %#v, want awaiting_human", processed)
	}
	if len(harness.executor.starts) != 1 {
		t.Fatalf("agent turns = %d, want only the scope assessment", len(harness.executor.starts))
	}
	if len(harness.github.createPRCalls) != 0 || len(harness.git.pushCalls) != 0 {
		t.Fatal("escalation published work; it must stop before spec authoring")
	}
	loop := harness.loop(t)
	if loop.Status != "awaiting_human" || loop.NextRunAt != nil {
		t.Fatalf("loop = (status %s, nextRunAt %v), want a stable awaiting_human state", loop.Status, loop.NextRunAt)
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || ask.Status != "awaiting" {
		t.Fatalf("hitl ask = (%#v, %v), want an awaiting ask", ask, ok)
	}
	if !strings.Contains(ask.Question, CriterionBlastRadiusFiles) || !strings.Contains(ask.Question, CriterionPublicSurfaceChange) {
		t.Fatalf("ask question does not state the decision requested:\n%s", ask.Question)
	}
	if len(ask.Options) != 2 || ask.Options[0] != EscalationOptionProceed || ask.Options[1] != EscalationOptionStop {
		t.Fatalf("ask options = %v", ask.Options)
	}
}

func TestPlannerEscalationPersistsReplayableStructuredRecord(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true, AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON})
	harness.process(t)

	events, err := harness.fixture.repos.Events.ListByEntity(context.Background(), "loop", harness.loopID)
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	var payloads []string
	for _, event := range events {
		if event.EventType == EscalationEventType {
			payloads = append(payloads, event.PayloadJSON)
		}
	}
	if len(payloads) != 1 {
		t.Fatalf("planner.escalation events = %d, want 1", len(payloads))
	}
	var record EscalationRecord
	if err := json.Unmarshal([]byte(payloads[0]), &record); err != nil {
		t.Fatalf("unmarshal escalation record error = %v", err)
	}
	if record.Version != escalationRecordVersion || record.Repo != "acme/looper" || record.IssueNumber != 114 || record.LoopID != harness.loopID {
		t.Fatalf("record identity = %#v", record)
	}
	wantCriteria := []string{CriterionBlastRadiusFiles, CriterionBlastRadiusPackages, CriterionPublicSurfaceChange}
	if strings.Join(firedNames(record.Criteria), ",") != strings.Join(wantCriteria, ",") {
		t.Fatalf("record criteria = %v, want %v", firedNames(record.Criteria), wantCriteria)
	}
	for _, criterion := range record.Criteria {
		if criterion.Threshold == "" || criterion.Observed == "" || len(criterion.Evidence) == 0 {
			t.Fatalf("criterion %#v lacks threshold/observed/evidence", criterion)
		}
	}
	if record.DecisionRequested == "" || len(record.Options) != 2 {
		t.Fatalf("record does not state the requested decision: %#v", record)
	}
	if !record.Policy.Enabled || record.Assessment.EstimatedFilesTouched != 24 {
		t.Fatalf("record omits the policy/assessment it was derived from: %#v", record)
	}
	if record.CreatedAt == "" {
		t.Fatal("record has no createdAt")
	}
}

func TestPlannerEscalationIsNotARunFailure(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true, AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON})
	processed := harness.process(t)

	if processed.FailureKind != "" {
		t.Fatalf("failureKind = %q, want empty — an escalation is not a failure", processed.FailureKind)
	}
	queue, err := harness.fixture.repos.Queue.GetByID(context.Background(), harness.queueID)
	if err != nil || queue == nil {
		t.Fatalf("Queue.GetByID() = (%#v, %v)", queue, err)
	}
	if queue.Status != "cancelled" {
		t.Fatalf("queue status = %s, want cancelled (no retry scheduled)", queue.Status)
	}
	if queue.Attempts > 1 {
		t.Fatalf("queue attempts = %d, want the claim's single attempt — an escalation must not count as a failed attempt", queue.Attempts)
	}
	if queue.LastErrorKind != nil {
		t.Fatalf("queue carries a failure classification: %v", *queue.LastErrorKind)
	}
	run, err := harness.fixture.repos.Runs.GetLatestByLoopID(context.Background(), harness.loopID)
	if err != nil || run == nil {
		t.Fatalf("Runs.GetLatestByLoopID() = (%#v, %v)", run, err)
	}
	if run.Status != "interrupted" || run.ErrorMessage != nil {
		t.Fatalf("run = (status %s, error %v), want interrupted with no error", run.Status, run.ErrorMessage)
	}
	loop := harness.loop(t)
	if loop.Status == "paused" || loop.Status == "queued" || loop.Status == "failed" {
		t.Fatalf("loop status = %s, want awaiting_human rather than a failure/retry state", loop.Status)
	}
}

func TestPlannerEscalationResumesToSpecAuthoringAfterHumanAuthorization(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true,
		AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON},
		AgentResult{Status: "completed", Summary: "wrote spec"},
	)
	if processed := harness.process(t); processed.Status != "awaiting_human" {
		t.Fatalf("first process = %#v", processed)
	}
	harness.answerEscalation(t, "proceed, but keep the change inside internal/planner")

	processed := harness.process(t)
	if processed.Status != "success" || processed.PullRequestNumber != 501 {
		t.Fatalf("resumed process = %#v, want a published spec PR", processed)
	}
	if len(harness.executor.starts) != 2 {
		t.Fatalf("agent turns = %d, want assessment + spec (the assessment must not re-run)", len(harness.executor.starts))
	}
	specPrompt := harness.executor.starts[1].Prompt
	if !strings.Contains(specPrompt, "keep the change inside internal/planner") {
		t.Fatalf("spec prompt dropped the human's authorization:\n%s", specPrompt)
	}
	loop := harness.loop(t)
	if loop.Status != "completed" {
		t.Fatalf("loop status = %s, want completed", loop.Status)
	}
	ask, _ := loops.ReadHITLAsk(loop.MetadataJSON)
	if ask.Status != "consumed" {
		t.Fatalf("hitl ask status = %s, want consumed so it cannot be re-applied", ask.Status)
	}
}

func TestPlannerEscalationRejectionSettlesIssueWithoutSpec(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true, AgentResult{Status: "completed", Stdout: escalatingAssessmentJSON})
	harness.process(t)
	harness.answerEscalation(t, "stop — this needs a design discussion first")

	processed := harness.process(t)
	if processed.Status != "skipped" {
		t.Fatalf("resumed process = %#v, want the issue settled", processed)
	}
	if !strings.Contains(processed.Summary, "settled by human") {
		t.Fatalf("summary = %q, want it to name the human settlement", processed.Summary)
	}
	if len(harness.executor.starts) != 1 {
		t.Fatalf("agent turns = %d, want no spec authoring after a rejection", len(harness.executor.starts))
	}
	if len(harness.github.createPRCalls) != 0 {
		t.Fatal("rejected escalation still opened a spec PR")
	}
	loop := harness.loop(t)
	if loop.Status != "completed" || loop.NextRunAt != nil {
		t.Fatalf("loop = (status %s, nextRunAt %v), want a settled loop", loop.Status, loop.NextRunAt)
	}
}

func TestPlannerProceedsNormallyWhenNoCriterionFires(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, true,
		AgentResult{Status: "completed", Stdout: benignAssessmentJSON},
		AgentResult{Status: "completed", Summary: "wrote spec"},
	)

	processed := harness.process(t)
	if processed.Status != "success" || processed.PullRequestNumber != 501 {
		t.Fatalf("processed = %#v, want normal planning", processed)
	}
	if len(harness.executor.starts) != 2 {
		t.Fatalf("agent turns = %d, want assessment + spec", len(harness.executor.starts))
	}
	if strings.Contains(harness.executor.starts[1].Prompt, "HUMAN AUTHORIZATION") {
		t.Fatal("spec prompt carries an authorization that was never requested")
	}
	if harness.loop(t).Status != "completed" {
		t.Fatalf("loop status = %s, want completed", harness.loop(t).Status)
	}
}

func TestPlannerSkipsScopeAssessmentWhenEscalationDisabled(t *testing.T) {
	t.Parallel()
	harness := newEscalationHarness(t, false, AgentResult{Status: "completed", Summary: "wrote spec"})

	processed := harness.process(t)
	if processed.Status != "success" || processed.PullRequestNumber != 501 {
		t.Fatalf("processed = %#v, want unchanged planner behaviour", processed)
	}
	if len(harness.executor.starts) != 1 {
		t.Fatalf("agent turns = %d, want spec authoring only when escalation is disabled", len(harness.executor.starts))
	}
}
