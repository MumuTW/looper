package loops

import (
	"context"
	"fmt"
	"strings"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/storage"
)

// HITLAnswerInput describes one durable answer-metadata update. Runtime poll
// lanes require an existing ask because stale remote replies are a no-op; the
// authenticated HTTP endpoint preserves its existing ability to record an
// answer against an awaiting loop whose ask payload is incomplete.
type HITLAnswerInput struct {
	LoopID             string
	Answer             string
	NowISO             string
	RequireExistingAsk bool
	// Resume makes answer recording own the awaiting_human -> running
	// transition and requeue in the same transaction. The HTTP endpoint keeps
	// this false because it still owns its established lifecycle transition.
	Resume bool
	// ConsumeGateEvidence lets the runtime poll lane consume the exact staged
	// malformed-gate identity only when the answer is applied.
	ConsumeGateEvidence bool
}

type HITLAnswerResult struct {
	Loop    storage.LoopRecord
	Applied bool
}

// RecordHITLAnswer is transaction-local. It owns the durable answer metadata
// update and, when requested, the runtime poll lane's lifecycle transition and
// queue reactivation so an interruption cannot leave an answered ask parked in
// awaiting_human.
func RecordHITLAnswer(ctx context.Context, repos *storage.Repositories, input HITLAnswerInput) (HITLAnswerResult, error) {
	if repos == nil || repos.Loops == nil {
		return HITLAnswerResult{}, fmt.Errorf("HITL answer recording is not configured")
	}
	if input.Resume && repos.Queue == nil {
		return HITLAnswerResult{}, fmt.Errorf("HITL answer resumption is not configured")
	}
	if strings.TrimSpace(input.LoopID) == "" || strings.TrimSpace(input.NowISO) == "" {
		return HITLAnswerResult{}, fmt.Errorf("HITL answer recording requires loop id and time")
	}
	loop, err := repos.Loops.GetByID(ctx, input.LoopID)
	if err != nil {
		return HITLAnswerResult{}, err
	}
	if loop == nil {
		return HITLAnswerResult{}, fmt.Errorf("%w: %s", ErrLoopNotFound, input.LoopID)
	}
	result := HITLAnswerResult{Loop: *loop}
	if loop.Status != string(domain.LoopStatusAwaitingHuman) {
		return result, nil
	}
	ask, present := ReadHITLAsk(loop.MetadataJSON)
	if input.RequireExistingAsk && !present {
		return result, nil
	}
	if input.ConsumeGateEvidence {
		// The authenticated transport answer is the authority to consume the
		// exact staged malformed-gate identity; changed evidence remains parked.
		if err := ConsumeHITLGateEvidence(ask.GateEvidence); err != nil {
			return HITLAnswerResult{}, fmt.Errorf("consume malformed HITL gate evidence: %w", err)
		}
	}
	ask.Answer = input.Answer
	ask.Status = "answered"
	ask.AnsweredAt = input.NowISO
	metadata, err := WriteHITLAsk(loop.MetadataJSON, ask)
	if err != nil {
		return HITLAnswerResult{}, err
	}
	updated := *loop
	updated.MetadataJSON = &metadata
	updated.UpdatedAt = input.NowISO
	if input.Resume {
		updated.Status = string(domain.LoopStatusRunning)
		updated.NextRunAt = &input.NowISO
	}
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return HITLAnswerResult{}, err
	}
	if input.Resume {
		if _, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, input.LoopID, input.NowISO); err != nil {
			return HITLAnswerResult{}, err
		}
	}
	return HITLAnswerResult{Loop: updated, Applied: true}, nil
}
