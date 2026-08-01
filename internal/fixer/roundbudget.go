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
//
// Once the recorded count reaches maxFixerRounds the loop is parked rather than
// queued for one more push: the initial creation is not charged (it has no prior
// head to move from), so the first charged round is the first rediscovery after a
// push, and parking at the limit bounds the total pushes to the configured maximum.
//
// An unchanged head is not another round, but a budget left exhausted by a partial
// write — the charge persisted but the pause did not, or the daemon exited between
// the two writes — is reasserted here so that stale intermediate state cannot be
// queued past the limit on the next discovery of the same head.
func (r *Runner) chargeFixerRound(ctx context.Context, loop storage.LoopRecord, headSHA string) (storage.LoopRecord, bool, error) {
	metadata := parseJSONObject(loop.MetadataJSON)
	previous, hasPrevious := parseRoundBudgetState(metadata)
	state, charged := nextRoundBudget(previous, hasPrevious, headSHA, r.nowISO())
	if charged {
		updated, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"fixerRoundBudget": state})
		if err != nil {
			return loop, false, err
		}
		if state.Rounds < r.maxFixerRounds {
			return updated, false, nil
		}
		return r.parkLoopForRoundBudget(ctx, updated, state)
	}
	if hasPrevious && previous.Rounds >= r.maxFixerRounds {
		if pauseReason, _ := stringFromAny(metadata["pauseReason"]); pauseReason == roundBudgetPauseReason && loop.Status == "paused" {
			return loop, true, nil
		}
		return r.parkLoopForRoundBudget(ctx, loop, previous)
	}
	return loop, false, nil
}

// parkLoopForRoundBudget persists the pause reason and paused status for an
// exhausted budget, emits the loop.paused event, and reports the loop as parked
// so the caller skips enqueueing.
func (r *Runner) parkLoopForRoundBudget(ctx context.Context, loop storage.LoopRecord, state roundBudgetState) (storage.LoopRecord, bool, error) {
	updated, err := r.mergeLoopMetadata(ctx, loop, map[string]any{"pauseReason": roundBudgetPauseReason})
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

// reviewerHasReviewedHead reports whether an authoritative review result for
// headSHA exists for this pull request.
//
// The reviewer loop records the head it last completed a review on under
// loop.lastReviewedHeadSha. The default scheduler runs reviewer discovery before
// fixer discovery and dispatches claimed runs asynchronously, so right after a
// fixer push the next fixer discovery commonly sees zero fix items while the
// reviewer for that head is still running; resetting the budget then lets any
// findings the reviewer publishes later start from an empty budget, recreating
// the unbounded productive loop this gate stops. Only a review result for the
// current head is evidence of convergence.
//
// When no reviewer loop exists there is no asynchronous reviewer whose findings
// could still arrive, so zero fix items is authoritative and the reset proceeds.
func (r *Runner) reviewerHasReviewedHead(ctx context.Context, projectID, repo string, prNumber int64, headSHA string) bool {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return true
	}
	loops, err := r.repos.Loops.List(ctx)
	if err != nil {
		return false
	}
	hasReviewerLoop := false
	for _, loop := range loops {
		if loop.Type != "reviewer" || loop.ProjectID != projectID || derefString(loop.Repo) != repo || derefInt64(loop.PRNumber) != prNumber {
			continue
		}
		hasReviewerLoop = true
		loopMeta, _ := parseJSONObject(loop.MetadataJSON)["loop"].(map[string]any)
		if reviewed, _ := stringFromAny(loopMeta["lastReviewedHeadSha"]); reviewed == headSHA {
			return true
		}
	}
	return !hasReviewerLoop
}

// clearRoundBudgetMetadata drops the budget and, only when the loop is parked for
// this reason, the pause with it. Another gate's pause is not ours to lift.
//
// A loop that converged has no executable work left (the caller reached zero fix
// items), so it is marked completed with no next run rather than stranded as
// queued with no queue item — clearFixerFollowupStateForPR already cancelled any
// queued items, and discoverPullRequestFromDetail enqueues no replacement. A
// foreign pause is left untouched.
func (r *Runner) clearRoundBudgetMetadata(ctx context.Context, loop storage.LoopRecord) (storage.LoopRecord, error) {
	apply := func(updated *storage.LoopRecord) error {
		meta, err := loops.DecodeMetadataObjectForWrite(updated.MetadataJSON)
		if err != nil {
			return err
		}
		delete(meta, "fixerRoundBudget")
		pauseReason, _ := stringFromAny(meta["pauseReason"])
		ownedPause := pauseReason == roundBudgetPauseReason
		if ownedPause {
			delete(meta, "pauseReason")
		}
		// Completing applies only when the loop was parked for this gate, or when
		// it was queued with no foreign pause holding it. A running loop or a loop
		// paused by another gate keeps its status.
		if ownedPause && updated.Status == "paused" {
			updated.Status = "completed"
			updated.NextRunAt = nil
		} else if pauseReason == "" && updated.Status == "queued" {
			updated.Status = "completed"
			updated.NextRunAt = nil
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
