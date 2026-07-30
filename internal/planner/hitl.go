package planner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// escalationError is returned from the assess-scope step when the configured
// policy fired on the agent's scope assessment. The step loop catches it and
// parks the loop as awaiting_human instead of treating it as a failure — an
// escalation is a decision, not a crash, so it must not take the retry /
// failure-classification path.
type escalationError struct {
	record EscalationRecord
}

func (e *escalationError) Error() string {
	return "planner escalated to human review before spec authoring"
}

func asEscalationError(err error) (*escalationError, bool) {
	var typed *escalationError
	if errors.As(err, &typed) {
		return typed, true
	}
	return nil, false
}

// suspendForHuman parks a planner run as awaiting_human: it persists the
// structured escalation record on the event log and the loop's HITL metadata,
// transitions the loop to awaiting_human, cancels the claimed queue item (so
// POST /loops/{seq}/respond can requeue it), and ends the run as interrupted so
// the resume starts at assess-scope with the human's answer in hand.
//
// It deliberately does not call failQueueItem: the queue item is cancelled, not
// failed, so attempts are never incremented and no retry is scheduled.
func (r *Runner) suspendForHuman(ctx context.Context, input stepInput, run storage.RunRecord, checkpoint plannerCheckpoint, escalation *escalationError) (ProcessResult, error) {
	nowISO := r.nowISO()
	record := escalation.record
	record.CreatedAt = nowISO

	question := record.DecisionRequested
	ask := loops.HITLAsk{
		Question: question,
		Options:  cloneStrings(record.Options),
		Status:   "awaiting",
		AskedAt:  nowISO,
		// The assessor reports observations. The configured policy, not the
		// assessor, decided that a human is needed, so do not present its
		// rationale as an agent recommendation in the dashboard.
		Consequences: escalationConsequences(),
		Transport:    "respond",
	}

	// The planner.escalation record is the authority for this suspension: it is
	// what the dashboard, the audit trail and any replay read to learn why the
	// loop is parked and what decision was requested. Parking without it would
	// leave awaiting_human with no explanation, so the write is checked and the
	// suspension is abandoned if it fails.
	if err := r.appendEventChecked(ctx, eventInput{
		eventType: EscalationEventType, projectID: input.Loop.ProjectID, loopID: input.Loop.ID, runID: run.ID,
		entityType: "loop", entityID: input.Loop.ID, payload: record,
	}); err != nil {
		return ProcessResult{}, err
	}

	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		if meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask); werr == nil {
			updated.MetadataJSON = &meta
		}
		updated.Status = "awaiting_human"
		updated.LastRunAt = stringPtr(nowISO)
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	reason := "planner suspended awaiting human decision"
	if _, err := r.repos.Queue.CancelByLoop(ctx, input.Loop.ID, nowISO, &reason); err != nil {
		return ProcessResult{}, err
	}
	summary := fmt.Sprintf("Awaiting human decision before spec authoring for %s#%d: %s", record.Repo, record.IssueNumber, firedCriteriaSummary(record.Criteria))
	if _, err := r.completeRun(ctx, run, "interrupted", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{LoopID: input.Loop.ID, RunID: run.ID, QueueItemID: input.QueueItem.ID, Status: "awaiting_human", Summary: summary}, nil
}

func firedCriteriaSummary(criteria []FiredCriterion) string {
	names := make([]string, 0, len(criteria))
	for _, criterion := range criteria {
		names = append(names, criterion.Criterion)
	}
	return strings.Join(names, ", ")
}

// pendingEscalationAnswer reads the human's answer to a prior escalation from
// the freshest loop record. The second return is false when no answered ask is
// waiting. It is read-only: the answer is flipped to "consumed" only once the
// resumed step has acted on it.
func (r *Runner) pendingEscalationAnswer(ctx context.Context, loop storage.LoopRecord) (loops.HITLAsk, bool) {
	meta := loop.MetadataJSON
	if r.repos != nil && r.repos.Loops != nil {
		if fresh, err := r.repos.Loops.GetByID(ctx, loop.ID); err == nil && fresh != nil {
			meta = fresh.MetadataJSON
		}
	}
	ask, ok := loops.ReadHITLAsk(meta)
	if !ok || ask.Status != "answered" || strings.TrimSpace(ask.Answer) == "" {
		return loops.HITLAsk{}, false
	}
	return ask, true
}

// markEscalationAnswerConsumed flips a delivered answer from "answered" to
// "consumed" so a later run of the same loop cannot re-apply it. Persisted
// against the freshest record so it does not clobber concurrent metadata writes.
func (r *Runner) markEscalationAnswerConsumed(ctx context.Context, loop storage.LoopRecord) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "answered" {
		return
	}
	ask.Status = "consumed"
	meta, werr := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	_ = r.repos.Loops.Upsert(ctx, *fresh)
}
