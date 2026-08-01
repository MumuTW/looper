package fixer

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
)

// roundBudgetPauseReason parks a fixer loop that keeps doing real work without
// ever finishing.
//
// Unlike every other pause in this package it is not resumable by new review
// feedback. A steady arrival of new feedback is the condition being stopped, not
// evidence that continuing would terminate, so the sibling resume paths must not
// grow a case for it: only an operator clears this one.
const roundBudgetPauseReason = "fixer_non_converging"

// maxFixerRoundsPerPullRequest is the default number of pushes a fixer may make
// against one pull request before the loop is parked for a human.
//
// Every other gate in this package resets when the head SHA moves, which is why
// none of them saw PR #495: 25 rounds over 11 hours, each pushing a real commit
// and resolving real threads, against a single markdown file that grew from 302
// to 2147 lines. Progress was never zero and no step failed twice, so neither the
// zero-progress pause nor the consecutive-failure breaker could fire. Nothing was
// wrong with any individual round — what was wrong was how many there were.
// Review feedback with no objective pass condition can always ask for one more
// refinement, so something has to count the asking.
const maxFixerRoundsPerPullRequest = 8

// roundBudgetState counts the pushes this loop has drawn review on for one pull
// request. It lives in loop metadata under "fixerRoundBudget".
type roundBudgetState struct {
	Rounds       int    `json:"rounds,omitempty"`
	LastHeadSHA  string `json:"lastHeadSha,omitempty"`
	FirstRoundAt string `json:"firstRoundAt,omitempty"`
	RecordedAt   string `json:"recordedAt,omitempty"`
}

// nextRoundBudget advances the budget for a discovery that is about to queue work
// at headSHA, and reports whether this discovery was charged.
//
// Only a moved head counts. Re-discovering the same head is the retry, pending-
// rediscovery and follow-up traffic the other gates already govern; charging it
// here would park loops that are not looping. A moved head is the single event
// that means this loop pushed and drew fresh review on what it pushed.
//
// An empty head is not charged. Discovery always carries one in practice, and a
// provider that cannot supply it must not silently spend a budget it cannot
// measure — the failure-streak breaker treats head identity as diagnostic for the
// same reason.
func nextRoundBudget(previous roundBudgetState, hasPrevious bool, headSHA, nowISO string) (roundBudgetState, bool) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return previous, false
	}
	if !hasPrevious {
		return roundBudgetState{Rounds: 1, LastHeadSHA: headSHA, FirstRoundAt: nowISO, RecordedAt: nowISO}, true
	}
	if previous.LastHeadSHA == headSHA {
		return previous, false
	}
	firstRoundAt := previous.FirstRoundAt
	if firstRoundAt == "" {
		firstRoundAt = nowISO
	}
	return roundBudgetState{
		Rounds:       previous.Rounds + 1,
		LastHeadSHA:  headSHA,
		FirstRoundAt: firstRoundAt,
		RecordedAt:   nowISO,
	}, true
}

func parseRoundBudgetState(metadata map[string]any) (roundBudgetState, bool) {
	raw, ok := metadata["fixerRoundBudget"]
	if !ok || raw == nil {
		return roundBudgetState{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return roundBudgetState{}, false
	}
	var state roundBudgetState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return roundBudgetState{}, false
	}
	if state.Rounds <= 0 {
		return roundBudgetState{}, false
	}
	return state, true
}

// chargeFixerRound records that discovery is about to queue another round against
// this pull request, and reports whether the loop was parked instead of queued.
//
// A parked loop is returned so the caller can stop without enqueueing; the budget
// is persisted either way so an operator reading the loop can see how it got here.
func (r *Runner) chargeFixerRound(ctx context.Context, loop storage.LoopRecord, headSHA string) (storage.LoopRecord, bool, error) {
	metadata := parseJSONObject(loop.MetadataJSON)
	previous, hasPrevious := parseRoundBudgetState(metadata)
	state, charged := nextRoundBudget(previous, hasPrevious, headSHA, r.nowISO())
	if !charged {
		return loop, false, nil
	}
	updated, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerRoundBudget": state})
	if err != nil {
		return loop, false, err
	}
	if state.Rounds <= r.maxFixerRounds {
		return updated, false, nil
	}
	updated, err = r.mergeLoopMetadata(ctx, updated, map[string]any{"pauseReason": roundBudgetPauseReason})
	if err != nil {
		return loop, false, err
	}
	updated, err = r.updateLoop(ctx, updated, func(mutated *storage.LoopRecord) {
		mutated.Status = "paused"
		mutated.NextRunAt = nil
	})
	if err != nil {
		return loop, false, err
	}
	r.logError("fixer round budget exhausted", map[string]any{
		"projectId":    updated.ProjectID,
		"loopId":       updated.ID,
		"repo":         derefString(updated.Repo),
		"prNumber":     derefInt64(updated.PRNumber),
		"rounds":       state.Rounds,
		"maxRounds":    r.maxFixerRounds,
		"headSha":      state.LastHeadSHA,
		"firstRoundAt": state.FirstRoundAt,
		"pauseReason":  roundBudgetPauseReason,
	})
	r.appendEvent(ctx, eventInput{
		eventType:  "loop.paused",
		projectID:  updated.ProjectID,
		loopID:     updated.ID,
		entityType: "loop",
		entityID:   updated.ID,
		payload: map[string]any{
			"pauseReason":  roundBudgetPauseReason,
			"rounds":       state.Rounds,
			"maxRounds":    r.maxFixerRounds,
			"headSha":      state.LastHeadSHA,
			"firstRoundAt": state.FirstRoundAt,
		},
	})
	return updated, true, nil
}

// clearRoundBudgetForPullRequest resets the budget for a pull request that has no
// fix work left. Reaching zero fix items is the only evidence that this loop
// converged, so it is the only automatic reset: "discovery enqueued nothing" is
// not equivalent, because a parked loop enqueues nothing every tick and would
// clear its own pause.
func (r *Runner) clearRoundBudgetForPullRequest(ctx context.Context, projectID, repo string, prNumber int64) error {
	loop, err := r.findFixerLoopByPR(ctx, projectID, repo, prNumber)
	if err != nil || loop == nil {
		return err
	}
	metadata := parseJSONObject(loop.MetadataJSON)
	if _, ok := parseRoundBudgetState(metadata); !ok {
		return nil
	}
	_, err = r.clearRoundBudgetMetadata(ctx, *loop)
	return err
}

// clearRoundBudgetMetadata drops the budget and, only when the loop is parked for
// this reason, the pause with it. Another gate's pause is not ours to lift.
func (r *Runner) clearRoundBudgetMetadata(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	apply := func(updated *storage.LoopRecord) error {
		meta, err := loops.DecodeMetadataObjectForWrite(updated.MetadataJSON)
		if err != nil {
			return err
		}
		delete(meta, "fixerRoundBudget")
		if pauseReason, _ := stringFromAny(meta["pauseReason"]); pauseReason == roundBudgetPauseReason {
			delete(meta, "pauseReason")
			if updated.Status == "paused" {
				updated.Status = "queued"
				nextRunAt := r.nowISO()
				updated.NextRunAt = &nextRunAt
			}
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		metadataJSON := string(encoded)
		updated.MetadataJSON = &metadataJSON
		return nil
	}
	var mutateErr error
	updated, err := r.updateLoop(ctx, loop, func(updated *storage.LoopRecord) {
		mutateErr = apply(updated)
	})
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if mutateErr != nil {
		return storage.LoopRecord{}, mutateErr
	}
	return updated, nil
}
