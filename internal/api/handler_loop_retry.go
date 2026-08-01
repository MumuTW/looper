package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/eventlog"
	"github.com/MumuTW/looper/internal/fixer"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

// retryLoop re-arms a loop for another scheduler pass. fromHandback is true when
// invoked via /loops/{id}/handback so discardWorktreeChanges is rejected: that
// path preserves human interactive edits in the worktree for the resumed session.
func (h *Handler) retryLoop(ctx context.Context, r *http.Request, loopID string, fromHandback bool) (retryLoopResponse, error) {
	var body retryLoopRequest
	if aerr := decodeJSONMutationBody(r, &body, false); aerr != nil {
		return retryLoopResponse{}, *aerr
	}
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "resume" && mode != "rediscover" {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Unsupported retry mode: %s", mode)}
	}
	if mode != "auto" {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusNotImplemented, message: fmt.Sprintf("Retry mode %s is not implemented safely yet; use mode auto", mode)}
	}
	resetAttempts := true
	if body.ResetAttempts != nil {
		resetAttempts = *body.ResetAttempts
	}
	if !resetAttempts {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "resetAttempts=false is not supported for explicit operator retry"}
	}
	discardWorktreeChanges := body.DiscardWorktreeChanges != nil && *body.DiscardWorktreeChanges
	if discardWorktreeChanges && fromHandback {
		return retryLoopResponse{}, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: "discardWorktreeChanges is not allowed on handback; human interactive worktree edits must be preserved (retry with --discard-worktree-changes after handback if needed)",
		}
	}

	// Serialize per-loop retry with start/requeue so discard cannot race another
	// retry or /loops/{id}/start that enqueues replacement work between preflight
	// and reset (or a scheduler-started run for that replacement).
	unlock := h.lockLoopRetry(loopID)
	defer unlock()

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())

	// Always resolve the loop target and hold the same-target lock for the whole
	// retry window — not only when discarding. Regular retry and /start for a
	// *different* failed loop on this target would otherwise create an active
	// queue item after discard preflight and before git reset, then the discard
	// TX would conflict after the worktree was already wiped.
	preflightLoop, err := services.Repositories.Loops.GetByID(ctx, loopID)
	if err != nil {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if preflightLoop == nil {
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
	}
	target, targetErr := loopTargetFromRecordCompat(*preflightLoop)
	if targetErr != nil {
		return retryLoopResponse{}, targetErr
	}
	unlockTarget := h.lockLoopTarget(preflightLoop.ProjectID, domain.LoopType(preflightLoop.Type), target)
	defer unlockTarget()

	// Opt-in discard runs before requeue so git mutation stays outside the
	// queue transaction. Every non-mutating retry blocker must pass first so a
	// later precondition failure never leaves discarded worktree changes
	// without creating a replacement queue item.
	var worktreeDiscard *worktreeDiscardResult
	if discardWorktreeChanges {
		// Runtime HITL poll requeues awaiting_human loops without the API lock
		// (hitl_github_poll / Feishu helpers). Refuse discard so a poll-delivered
		// answer cannot requeue between preflight and git reset, wiping the
		// worktree for the answered continuation when the retry TX then conflicts.
		// human_takeover pins the same worktree for interactive human edits;
		// /handback already rejects discard, and direct /retry must match that.
		if err := rejectDiscardWhileParkedForHuman(preflightLoop.Status, loopID); err != nil {
			return retryLoopResponse{}, err
		}

		if err := h.assertLoopRetryPreconditions(ctx, services.Repositories, *preflightLoop, nowISO); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		// Same-type uniqueness is not enough for discard: PR worktrees are shared
		// across fixer/reviewer/worker. An already queued/running/waiting/
		// human_takeover sibling is not held by the target mutex (that only
		// serializes mutations), so refuse git reset/clean while any worktree-
		// owning sibling holds the PR checkout.
		if err := h.assertDiscardSharedPRWorktreeClear(ctx, services.Repositories, *preflightLoop); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}

		// Recheck immediately before git mutation as defense in depth. Runtime
		// free-text enqueue now shares LockLoopRequeue with this path, so the
		// common race is serialized; this snapshot still catches any unlocked
		// requeue injected under discardBeforeGitHook in tests (or future
		// callers that forget the shared guard).
		if h.discardBeforeGitHook != nil {
			h.discardBeforeGitHook(loopID)
		}
		freshLoop, freshErr := services.Repositories.Loops.GetByID(ctx, loopID)
		if freshErr != nil {
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: freshErr.Error()}
		}
		if freshLoop == nil {
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		if err := rejectDiscardWhileParkedForHuman(freshLoop.Status, loopID); err != nil {
			return retryLoopResponse{}, err
		}
		if err := h.assertLoopRetryPreconditions(ctx, services.Repositories, *freshLoop, nowISO); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		if err := h.assertDiscardSharedPRWorktreeClear(ctx, services.Repositories, *freshLoop); err != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		preflightLoop = freshLoop
		if preflightLoop.Type == "fixer" {
			if _, err := loops.DecodeMetadataObjectForWrite(preflightLoop.MetadataJSON); err != nil {
				return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot discard worktree changes while loop metadata is malformed: %v", err)}
			}
		}

		discardResult, discardErr := h.discardLoopWorktreeChanges(ctx, services, *preflightLoop)
		if discardErr != nil {
			var typed apiError
			if asAPIError(discardErr, &typed) {
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: discardErr.Error()}
		}
		worktreeDiscard = &discardResult
	}

	type retryResult struct {
		loop        storage.LoopRecord
		queueItemID *string
	}
	// Clear the sticky stop gate before the queue item becomes claimable.
	// Clearing after the TX commit races a concurrent scheduler tick that can
	// claim the new item, pass the parked check (loop is queued), then fail
	// AgentExecutor.Start with ErrSpawnLoopStopping and back off the retry.
	// If the TX fails (or publishes no replacement work), restore the gate so
	// a failed retry cannot reopen AdmitSpawn for stale pre-stop runners.
	gateWasActive := false
	if services.ActiveExecutions != nil {
		// Clear and report under one lock so abort restore covers any gate this
		// call removed (including one set by concurrent BeginLoopStop).
		gateWasActive = services.ActiveExecutions.ClearLoopStop(loopID)
	}
	restoreStopGate := func() error {
		if gateWasActive && services.ActiveExecutions != nil {
			return services.ActiveExecutions.RestoreLoopStop(loopID)
		}
		return nil
	}
	if h.retryAfterClearStopGateHook != nil {
		h.retryAfterClearStopGateHook(loopID)
	}
	result, err := storage.WithTransactionValue(ctx, services.Coordinator.DB(), nil, func(tx *sql.Tx) (retryResult, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return retryResult{}, err
		}
		if loop == nil {
			return retryResult{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found: %s", loopID)}
		}
		if err := h.assertLoopRetryPreconditions(ctx, repos, *loop, nowISO); err != nil {
			return retryResult{}, err
		}
		// When discard already mutated the worktree, re-check shared-PR siblings
		// inside the TX so a concurrent runtime requeue/create that raced past
		// preflight cannot leave both an active sibling and a successful retry.
		if discardWorktreeChanges {
			if err := h.assertDiscardSharedPRWorktreeClear(ctx, repos, *loop); err != nil {
				return retryResult{}, err
			}
		}

		target, targetErr := loopTargetFromRecordCompat(*loop)
		if targetErr != nil {
			return retryResult{}, targetErr
		}
		latestQueue, err := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return retryResult{}, err
		}

		queueLoop := *loop
		queueLoop.Status = string(domain.LoopStatusQueued)
		queueLoop.NextRunAt = &nowISO
		queueLoop.UpdatedAt = nowISO
		escapeFixerManualPark := false
		if queueLoop.Type == string(domain.LoopTypeReviewer) {
			metadataJSON, metadataErr := resetReviewerLoopRetryMetadata(queueLoop.MetadataJSON)
			if metadataErr != nil {
				return retryResult{}, metadataErr
			}
			queueLoop.MetadataJSON = metadataJSON
		} else if queueLoop.Type == string(domain.LoopTypeFixer) {
			metadataJSON, metadataErr := resetFixerLoopRetryMetadata(queueLoop.MetadataJSON)
			if metadataErr != nil {
				return retryResult{}, metadataErr
			}
			queueLoop.MetadataJSON = metadataJSON
			// Deferred until the queue record is known to be committable — see the
			// rewrite below. Rewriting here would destroy the park even on the
			// no-queue-record and dedupe-conflict exits.
			escapeFixerManualPark = true
		}
		var queueRecord storage.QueueItemRecord
		var ok bool
		if latestQueue != nil {
			queueRecord = *latestQueue
			queueRecord.ID = generateRequestID()
			queueRecord.Status = "queued"
			queueRecord.AvailableAt = nowISO
			if resetAttempts {
				queueRecord.Attempts = 0
			}
			queueRecord.ClaimedBy = nil
			queueRecord.ClaimedAt = nil
			queueRecord.StartedAt = nil
			queueRecord.FinishedAt = nil
			queueRecord.LastError = nil
			queueRecord.LastErrorKind = nil
			queueRecord.CreatedAt = nowISO
			queueRecord.UpdatedAt = nowISO
			ok = true
		} else {
			built, builtOK, queueErr := buildQueuedLoopQueueRecordCompat(queueLoop, target, nowISO, queueLoop.MetadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
			if queueErr != nil {
				return retryResult{}, queueErr
			}
			queueRecord = built
			ok = builtOK
		}
		if !ok {
			return retryResult{loop: *loop}, nil
		}
		// Dedupe is already asserted by assertLoopRetryPreconditions; re-check
		// inside the transaction for races between preflight and commit.
		if queueRecord.DedupeKey != "" {
			activeDedupe, err := repos.Queue.FindActiveByDedupe(ctx, queueRecord.DedupeKey)
			if err != nil {
				return retryResult{}, err
			}
			if activeDedupe != nil {
				return retryResult{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusConflict, message: fmt.Sprintf("Cannot retry loop %s while dedupe queue item %s is active", loop.ID, activeDedupe.ID)}
			}
		}

		// An explicit operator retry must escape a fixer run parked because the
		// repair completion contract was missing or invalid. Without rewriting the
		// checkpoint, createRunContext resumes at the same downstream step and
		// validateFixerResumeCheckpoint parks it again, so retry can never reach
		// repair or discovery.
		//
		// Rewritten only here, after the no-queue-record and dedupe-conflict exits:
		// the first commits the transaction with a nil error, so an earlier rewrite
		// would clear the park without queueing the retry that justified it.
		if escapeFixerManualPark {
			if _, err := fixer.MarkInvalidCompletionRunRestartFromDiscover(ctx, repos, loop.ID, nowISO); err != nil {
				return retryResult{}, err
			}
		}

		updated := queueLoop
		writeLoop := repos.Loops.Upsert
		if loop.Status == string(domain.LoopStatusHumanTakeover) {
			writeLoop = repos.Loops.UpsertChangingHumanHold
		}
		if err := writeLoop(ctx, updated); err != nil {
			return retryResult{}, err
		}
		persisted, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, queueRecord)
		if err != nil {
			return retryResult{}, err
		}
		return retryResult{loop: updated, queueItemID: &persisted.ID}, nil
	})
	if err != nil {
		err = mapIssueClaimAdmissionError(err)
		if restoreErr := restoreStopGate(); restoreErr != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				typed.message = errors.Join(err, restoreErr).Error()
				return retryLoopResponse{}, typed
			}
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: errors.Join(err, restoreErr).Error()}
		}
		var typed apiError
		if asAPIError(err, &typed) {
			return retryLoopResponse{}, typed
		}
		return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if result.queueItemID == nil {
		// No replacement work published; keep sticky stop closed if it was.
		if restoreErr := restoreStopGate(); restoreErr != nil {
			return retryLoopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: restoreErr.Error()}
		}
	}
	if h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}
	return retryLoopResponse{
		Loop:                   serializeLoop(result.loop),
		QueueItemID:            result.queueItemID,
		Mode:                   mode,
		ResetAttempts:          resetAttempts,
		DiscardWorktreeChanges: discardWorktreeChanges,
		WorktreeDiscard:        worktreeDiscard,
	}, nil
}
