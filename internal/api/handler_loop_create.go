package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

type createLoopRequest struct {
	ProjectID   *string         `json:"projectId"`
	Type        *string         `json:"type"`
	TargetType  *string         `json:"targetType"`
	TargetID    *string         `json:"targetId"`
	Repo        *string         `json:"repo"`
	PRNumber    *int64          `json:"prNumber"`
	IssueNumber *int64          `json:"issueNumber"`
	Status      *string         `json:"status"`
	Force       *bool           `json:"force"`
	Metadata    json.RawMessage `json:"metadata"`
}

type createWorkerRequest struct {
	ProjectID   *string `json:"projectId"`
	Title       *string `json:"title"`
	Prompt      *string `json:"prompt"`
	SpecPath    *string `json:"specPath"`
	Repo        *string `json:"repo"`
	BaseBranch  *string `json:"baseBranch"`
	PRNumber    *int64  `json:"prNumber"`
	IssueNumber *int64  `json:"issueNumber"`
	Force       *bool   `json:"force"`
}

type createPlannerRequest struct {
	ProjectID   *string `json:"projectId"`
	IssueNumber *int64  `json:"issueNumber"`
	Force       *bool   `json:"force"`
}

type workerCreateResponse struct {
	loopResponse
	Title       string  `json:"title"`
	Prompt      *string `json:"prompt"`
	SpecPath    *string `json:"specPath"`
	BaseBranch  string  `json:"baseBranch"`
	IssueNumber *int64  `json:"issueNumber,omitempty"`
	Reused      bool    `json:"reused,omitempty"`
}

type plannerCreateResponse struct {
	loopResponse
	IssueNumber int64 `json:"issueNumber"`
}

func (h *Handler) buildCreateLoopResponse(r *http.Request) (loopResponse, error) {
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	var body createLoopRequest
	if aerr := decodeJSONMutationBody(r, &body, true); aerr != nil {
		return loopResponse{}, *aerr
	}

	projectID := strings.TrimSpace(derefString(body.ProjectID))
	if projectID == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "projectId is required"}
	}

	loopType := strings.TrimSpace(derefString(body.Type))
	if loopType == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "type is required"}
	}
	if err := domain.AssertKnownLoopType(domain.LoopType(loopType)); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
	}

	targetType := strings.TrimSpace(derefString(body.TargetType))
	if targetType == "" {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "targetType is required"}
	}

	status := strings.TrimSpace(derefString(body.Status))
	if status == "" {
		status = string(domain.LoopStatusRunning)
	}
	if err := domain.AssertKnownLoopStatus(domain.LoopStatus(status)); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
	}

	if (loopType == string(domain.LoopTypeReviewer) || loopType == string(domain.LoopTypeFixer) || loopType == string(domain.LoopTypeWorker) || loopType == string(domain.LoopTypePlanner)) && !isCodingRoleAgentConfigured(h.effectiveConfig(), loopType) {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: fmt.Sprintf("Cannot create %s loop without config.agent.vendor", loopType)}
	}

	target, err := buildLoopTarget(targetType, body)
	if err != nil {
		return loopResponse{}, err
	}
	if err := domain.AssertLoopTypeMatchesTarget(domain.LoopType(loopType), target); err != nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: err.Error()}
	}

	metadataJSON, err := normalizeMetadataJSON(body.Metadata)
	if err != nil {
		return loopResponse{}, err
	}
	if domain.LoopType(loopType) == domain.LoopTypePlanner {
		metadataJSON, err = manualPlannerMetadataJSON(metadataJSON, target.IssueNumber)
		if err != nil {
			return loopResponse{}, err
		}
	}
	now := h.now().UTC()
	nowISO := eventlog.FormatJavaScriptISOString(now)
	if domain.LoopType(loopType) == domain.LoopTypeFixer {
		metadataJSON, err = manualFixerMetadataJSON(metadataJSON, nowISO)
		if err != nil {
			return loopResponse{}, err
		}
	}
	if domain.LoopType(loopType) == domain.LoopTypeReviewer {
		metadataJSON, err = reviewerLoopMetadataJSON(metadataJSON, h.context.Config.Roles.Reviewer.Behavior, target, nowISO, derefBool(body.Force))
		if err != nil {
			return loopResponse{}, err
		}
	}
	if services.Repositories == nil || services.Repositories.Projects == nil {
		return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	project, err := requireActiveProjectRecord(r.Context(), services.Repositories.Projects, projectID)
	if err != nil {
		return loopResponse{}, err
	}
	if err := validateLoopTargetProjectCompatibility(projectID, parseProjectMetadata(project.MetadataJSON), target); err != nil {
		return loopResponse{}, err
	}
	if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopType(loopType), target, derefBool(body.Force)); err != nil {
		return loopResponse{}, err
	}

	// Share the same-target lock with discard+retry so create cannot enqueue a
	// new active loop for this target between discard preflight and requeue.
	candidateStatusForLock := domain.LoopStatus(status)
	if (domain.LoopType(loopType) == domain.LoopTypeReviewer || domain.LoopType(loopType) == domain.LoopTypeFixer || domain.LoopType(loopType) == domain.LoopTypeWorker) && candidateStatusForLock == domain.LoopStatusRunning {
		candidateStatusForLock = domain.LoopStatusQueued
	}
	unlockTarget := h.lockLoopTargetForStatus(projectID, domain.LoopType(loopType), target, candidateStatusForLock)
	defer unlockTarget()

	record, err := storage.WithTransactionValue(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		transactionRepos := storage.NewRepositories(tx)
		_, err := requireActiveProjectRecord(r.Context(), transactionRepos.Projects, projectID)
		if err != nil {
			return storage.LoopRecord{}, err
		}

		existing, err := transactionRepos.Loops.List(r.Context())
		if err != nil {
			return storage.LoopRecord{}, err
		}
		candidateStatus := domain.LoopStatus(status)
		if err := assertUniqueActiveLoopCompat(existing, "", projectID, domain.LoopType(loopType), target, candidateStatus); err != nil {
			return storage.LoopRecord{}, err
		}

		seq, err := transactionRepos.Loops.AllocateSeq(r.Context())
		if err != nil {
			return storage.LoopRecord{}, err
		}

		record := storage.LoopRecord{
			ID:           generateRequestID(),
			Seq:          seq,
			ProjectID:    projectID,
			Type:         loopType,
			TargetType:   targetType,
			TargetID:     loopTargetIDCompat(target),
			Repo:         repoFromTargetCompat(target),
			PRNumber:     prNumberFromTargetCompat(target),
			Status:       status,
			ConfigJSON:   nil,
			MetadataJSON: metadataJSON,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if (domain.LoopType(loopType) == domain.LoopTypeReviewer || domain.LoopType(loopType) == domain.LoopTypeFixer || domain.LoopType(loopType) == domain.LoopTypeWorker) && candidateStatus == domain.LoopStatusRunning {
			record.Status = string(domain.LoopStatusQueued)
			candidateStatus = domain.LoopStatusQueued
		}
		if candidateStatus == domain.LoopStatusRunning {
			record.NextRunAt = &nowISO
		} else if candidateStatus == domain.LoopStatusQueued {
			record.NextRunAt = &nowISO
		}

		if err := transactionRepos.Loops.Upsert(r.Context(), record); err != nil {
			return storage.LoopRecord{}, err
		}

		shouldQueue := ((domain.LoopType(loopType) == domain.LoopTypeReviewer || domain.LoopType(loopType) == domain.LoopTypeFixer || domain.LoopType(loopType) == domain.LoopTypeWorker) && candidateStatus == domain.LoopStatusQueued) || (domain.LoopType(loopType) == domain.LoopTypePlanner && (candidateStatus == domain.LoopStatusRunning || candidateStatus == domain.LoopStatusQueued))
		if shouldQueue {
			queueRecord, ok, queueErr := buildQueuedLoopQueueRecordCompat(record, target, nowISO, metadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
			if queueErr != nil {
				return storage.LoopRecord{}, queueErr
			}
			if ok {
				existingQueue, findErr := transactionRepos.Queue.FindActiveByDedupe(r.Context(), queueRecord.DedupeKey)
				if findErr != nil {
					return storage.LoopRecord{}, findErr
				}
				if existingQueue == nil {
					persistedQueue, createdQueue, upsertQueueErr := transactionRepos.Queue.CreateOrGetActiveByDedupe(r.Context(), queueRecord)
					if upsertQueueErr != nil {
						return storage.LoopRecord{}, upsertQueueErr
					}
					if !createdQueue && persistedQueue.ID != queueRecord.ID {
						return storage.LoopRecord{}, fmt.Errorf("active loop already exists for dedupe key %s", queueRecord.DedupeKey)
					}
				}
			}
		}

		return record, nil
	})
	if err != nil {
		var typed apiError
		if asAPIError(err, &typed) {
			return loopResponse{}, typed
		}
		return loopResponse{}, mapLoopCreateError(err)
	}
	shouldTriggerScheduler := ((record.Type == string(domain.LoopTypeReviewer) || record.Type == string(domain.LoopTypeFixer) || record.Type == string(domain.LoopTypeWorker)) && record.Status == string(domain.LoopStatusQueued)) || (record.Type == string(domain.LoopTypePlanner) && (record.Status == string(domain.LoopStatusRunning) || record.Status == string(domain.LoopStatusQueued)))
	if shouldTriggerScheduler && h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}

	return serializeLoop(record), nil
}

func (h *Handler) buildWorkersCreateResponse(r *http.Request) (workerCreateResponse, error) {
	if r.Method != http.MethodPost {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/workers")}
	}
	if !isCodingRoleAgentConfigured(h.effectiveConfig(), config.CodingRoleWorker) {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: "Cannot create worker loop without config.agent.vendor"}
	}

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	body := createWorkerRequest{}
	if aerr := decodeJSONMutationBody(r, &body, true); aerr != nil {
		return workerCreateResponse{}, *aerr
	}

	prompt := normalizeOptionalString(body.Prompt)
	specPath := normalizeOptionalString(body.SpecPath)
	prNumber := normalizePositiveInt64Ptr(body.PRNumber)
	issueNumber := normalizePositiveInt64Ptr(body.IssueNumber)
	// prNumber and issueNumber are alternative targets and stay exclusive.
	// prompt/specPath are the whole input for a project-target worker, but only a
	// refinement for a pull-request worker: resolveWorkerInput prefers an explicit
	// specPath over the one parsed out of the PR body, and a PR whose body carries
	// no spec-path marker is otherwise undispatchable. Issue mode keeps the
	// exclusivity because Planner is what supplies the spec there.
	if prNumber != nil && issueNumber != nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "worker accepts exactly one target: prNumber or issueNumber"}
	}
	if issueNumber != nil && (prompt != nil || specPath != nil) {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "worker accepts exactly one input mode: prompt/specPath, prNumber, or issueNumber"}
	}
	if prNumber == nil && issueNumber == nil && prompt == nil && specPath == nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "prompt or specPath is required unless prNumber or issueNumber is provided"}
	}

	project, err := h.resolveWorkerProject(r.Context(), resolveWorkerProjectInput{
		ProjectID: normalizeOptionalString(body.ProjectID),
		Repo:      normalizeOptionalString(body.Repo),
		PRNumber:  prNumber,
	})
	if err != nil {
		return workerCreateResponse{}, err
	}
	projectID := project.ID

	repo := normalizeOptionalString(body.Repo)
	if repo == nil {
		repo = stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")
	}
	if repo == nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "repo is required"}
	}

	baseBranch := normalizeOptionalString(body.BaseBranch)
	if baseBranch == nil {
		baseBranch = normalizeOptionalString(project.BaseBranch)
	}
	if baseBranch == nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "baseBranch is required"}
	}
	requestedIssueTarget := (*domain.LoopTarget)(nil)
	if issueNumber != nil {
		requestedIssueTarget = &domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: *repo, IssueNumber: *issueNumber}
	}

	effectivePRNumber := (*int64)(nil)
	if prNumber != nil {
		resolved, resolveErr := h.requirePullRequestTarget(r.Context(), requirePullRequestTargetInput{ProjectID: projectID, Repo: *repo, PRNumber: *prNumber})
		if resolveErr != nil {
			return workerCreateResponse{}, resolveErr
		}
		effectivePRNumber = &resolved
	}

	planner := (*workerPlannerMatch)(nil)
	if issueNumber != nil {
		planner, err = h.maybeFindPlannerLoopForIssue(r.Context(), findPlannerLoopForIssueInput{ProjectID: projectID, Repo: *repo, IssueNumber: *issueNumber})
		if err != nil {
			return workerCreateResponse{}, err
		}
	}
	if effectivePRNumber == nil && planner != nil {
		effectivePRNumber = planner.PRNumber
	}
	effectiveSpecPath := specPath
	if effectiveSpecPath == nil && planner != nil {
		effectiveSpecPath = planner.SpecPath
	}

	title := strings.TrimSpace(derefString(body.Title))
	if title == "" {
		title = deriveWorkerTitle(prompt, effectiveSpecPath, repo, effectivePRNumber, issueNumber)
	}
	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	targetType := string(domain.LoopTargetTypeProject)
	targetID := "project:" + projectID
	target := domain.LoopTarget{TargetType: domain.LoopTargetTypeProject, ProjectID: projectID}
	if effectivePRNumber != nil {
		targetType = string(domain.LoopTargetTypePullRequest)
		targetID = fmt.Sprintf("pr:%s:%d", *repo, *effectivePRNumber)
		target = domain.LoopTarget{TargetType: domain.LoopTargetTypePullRequest, Repo: *repo, PRNumber: *effectivePRNumber}
	} else if issueNumber != nil {
		targetType = string(domain.LoopTargetTypeIssue)
		targetID = fmt.Sprintf("issue:%s:%d", *repo, *issueNumber)
		target = domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: *repo, IssueNumber: *issueNumber}
	}
	if err := validateLoopTargetProjectCompatibility(projectID, parseProjectMetadata(project.MetadataJSON), target); err != nil {
		return workerCreateResponse{}, err
	}
	if requestedIssueTarget != nil {
		if err := validateLoopTargetProjectCompatibility(projectID, parseProjectMetadata(project.MetadataJSON), *requestedIssueTarget); err != nil {
			return workerCreateResponse{}, err
		}
		if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopTypeWorker, *requestedIssueTarget, derefBool(body.Force)); err != nil {
			return workerCreateResponse{}, err
		}
	}
	if requestedIssueTarget == nil || target.TargetType != requestedIssueTarget.TargetType || target.Repo != requestedIssueTarget.Repo || target.IssueNumber != requestedIssueTarget.IssueNumber {
		if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopTypeWorker, target, derefBool(body.Force)); err != nil {
			return workerCreateResponse{}, err
		}
	}

	// Forge-side occupancy for an issue target: refuse when the issue is closed
	// or an open pull request already references it, so Looper does not duplicate
	// work someone else is already doing. force overrides, matching the local
	// occupancy check from #319. When the forge lookup is not configured (tests,
	// embeddings without forge access) the local check keeps working unchanged.
	if issueNumber != nil && !derefBool(body.Force) && h.context.LookupIssueOccupancy != nil {
		if err := h.refuseOccupiedIssueTarget(r.Context(), project, *repo, *issueNumber); err != nil {
			return workerCreateResponse{}, err
		}
	}

	workerPayload := struct {
		Title       string  `json:"title"`
		Prompt      *string `json:"prompt"`
		SpecPath    *string `json:"specPath"`
		Repo        string  `json:"repo"`
		BaseBranch  string  `json:"baseBranch"`
		IssueNumber *int64  `json:"issueNumber,omitempty"`
		PRNumber    *int64  `json:"prNumber,omitempty"`
	}{
		Title:       title,
		Prompt:      prompt,
		SpecPath:    effectiveSpecPath,
		Repo:        *repo,
		BaseBranch:  *baseBranch,
		IssueNumber: issueNumber,
		PRNumber:    effectivePRNumber,
	}
	payloadJSONBytes, err := json.Marshal(struct {
		Worker any `json:"worker"`
	}{
		Worker: workerPayload,
	})
	if err != nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	queuePayloadJSONBytes, err := json.Marshal(workerPayload)
	if err != nil {
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	metadataJSON := string(payloadJSONBytes)
	reusedWorkerLoop := false

	// Issue-worker reuse enqueues the existing loop (same as start requeue).
	// Take the shared per-loop retry lock before the TX so discard+retry cannot
	// wipe the managed worktree after reuse preflight/enqueue races in.
	// Pre-scan is best-effort identity for the lock; the TX re-evaluates reuse.
	if issueNumber != nil && requestedIssueTarget != nil {
		existing, listErr := services.Repositories.Loops.List(r.Context())
		if listErr != nil {
			return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: listErr.Error()}
		}
		if existingLoop, _, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr != nil {
			return workerCreateResponse{}, reuseErr
		} else if ok {
			unlock := h.lockLoopRetry(existingLoop.ID)
			defer unlock()
		}
	}
	// New worker create (and non-reuse paths) share the same-target lock with
	// discard+retry so a concurrent create for this target cannot pass unique
	// checks after discard preflight and leave a wiped worktree.
	unlockWorkerTarget := h.lockLoopTargetForStatus(projectID, domain.LoopTypeWorker, target, domain.LoopStatusQueued)
	defer unlockWorkerTarget()

	// Issue-worker reuse publishes claimable queue work inside the TX. Clear the
	// sticky stop gate before that TX so a concurrent scheduler tick cannot claim
	// the reused worker and fail AgentExecutor.Start with ErrSpawnLoopStopping.
	// Track for restore when the TX aborts or does not queue the reused loop.
	reuseStopGateLoopID := ""
	reuseGateWasActive := false
	if issueNumber != nil && requestedIssueTarget != nil {
		existing, listErr := services.Repositories.Loops.List(r.Context())
		if listErr == nil {
			if existingLoop, _, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr == nil && ok {
				reuseStopGateLoopID = existingLoop.ID
				if services.ActiveExecutions != nil {
					// Clear and sample under one lock so a concurrent BeginLoopStop
					// cannot insert a gate that we delete without recording for restore.
					reuseGateWasActive = services.ActiveExecutions.ClearLoopStop(existingLoop.ID)
				}
			}
		}
	}
	restoreReuseStopGate := func() error {
		if reuseGateWasActive && reuseStopGateLoopID != "" && services.ActiveExecutions != nil {
			return services.ActiveExecutions.RestoreLoopStop(reuseStopGateLoopID)
		}
		return nil
	}

	record, err := storage.WithTransactionValue(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)

		existing, listErr := repos.Loops.List(r.Context())
		if listErr != nil {
			return storage.LoopRecord{}, listErr
		}
		if issueNumber != nil {
			if existingLoop, existingTarget, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr != nil {
				return storage.LoopRecord{}, reuseErr
			} else if ok {
				reusedWorkerLoop = true
				// Ensure gate is open even when the pre-TX scan missed this loop;
				// still before commit so the queue item is not yet claimable.
				if services.ActiveExecutions != nil {
					if reuseStopGateLoopID == "" {
						reuseStopGateLoopID = existingLoop.ID
					}
					// Clear+report under one lock: looper stop may establish the gate
					// after the pre-TX clear saw it inactive. Without this return
					// value, TX abort restore would skip (flag still false).
					if services.ActiveExecutions.ClearLoopStop(existingLoop.ID) {
						reuseGateWasActive = true
					}
				}
				resumed, resumeErr := h.resumeReusableWorkerLoopCompat(r.Context(), repos, existingLoop, existingTarget, nowISO, derefBool(body.Force))
				if resumeErr != nil {
					return storage.LoopRecord{}, resumeErr
				}
				return resumed, nil
			}
		}
		if uniqueErr := assertUniqueActiveLoopCompat(existing, "", projectID, domain.LoopTypeWorker, target, domain.LoopStatusQueued); uniqueErr != nil {
			return storage.LoopRecord{}, uniqueErr
		}

		seq, seqErr := repos.Loops.AllocateSeq(r.Context())
		if seqErr != nil {
			return storage.LoopRecord{}, seqErr
		}

		record := storage.LoopRecord{
			ID:           generateRequestID(),
			Seq:          seq,
			ProjectID:    projectID,
			Type:         string(domain.LoopTypeWorker),
			TargetType:   targetType,
			TargetID:     &targetID,
			Repo:         repo,
			PRNumber:     effectivePRNumber,
			Status:       string(domain.LoopStatusQueued),
			ConfigJSON:   nil,
			MetadataJSON: &metadataJSON,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if upsertErr := repos.Loops.Upsert(r.Context(), record); upsertErr != nil {
			return storage.LoopRecord{}, upsertErr
		}

		projectIDCopy := projectID
		loopID := record.ID
		dedupeKey := "worker:" + loopID
		lockKey := "worker:" + loopID
		if effectivePRNumber != nil {
			dedupeKey = fmt.Sprintf("worker:%s:%s:%d", projectID, *repo, *effectivePRNumber)
			lockKey = storage.PullRequestLockKey(projectID, *repo, *effectivePRNumber)
		} else if issueNumber != nil {
			dedupeKey = fmt.Sprintf("worker:%s:%s:%d", projectID, *repo, *issueNumber)
			lockKey = storage.IssueLockKey(projectID, *repo, *issueNumber)
		}
		payloadJSON := string(queuePayloadJSONBytes)
		queueRecord := storage.QueueItemRecord{
			ID:          generateRequestID(),
			ProjectID:   &projectIDCopy,
			LoopID:      &loopID,
			Type:        string(domain.LoopTypeWorker),
			TargetType:  targetType,
			TargetID:    targetID,
			Repo:        repo,
			PRNumber:    effectivePRNumber,
			DedupeKey:   dedupeKey,
			Priority:    storage.QueuePriorityWorker,
			Status:      "queued",
			AvailableAt: nowISO,
			Attempts:    0,
			MaxAttempts: int64(h.context.Config.Scheduler.RetryMaxAttempts),
			LockKey:     &lockKey,
			PayloadJSON: &payloadJSON,
			CreatedAt:   nowISO,
			UpdatedAt:   nowISO,
		}
		if upsertQueueErr := repos.Queue.Upsert(r.Context(), queueRecord); upsertQueueErr != nil {
			return storage.LoopRecord{}, upsertQueueErr
		}

		return record, nil
	})
	if err != nil {
		if restoreErr := restoreReuseStopGate(); restoreErr != nil {
			var typed apiError
			if asAPIError(err, &typed) {
				typed.message = errors.Join(err, restoreErr).Error()
				return workerCreateResponse{}, typed
			}
			return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: errors.Join(err, restoreErr).Error()}
		}
		var typed apiError
		if asAPIError(err, &typed) {
			return workerCreateResponse{}, typed
		}
		return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	// Pre-cleared for a reuse that did not become claimable queued work: restore.
	if !reusedWorkerLoop || record.Status != string(domain.LoopStatusQueued) {
		if restoreErr := restoreReuseStopGate(); restoreErr != nil {
			return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: restoreErr.Error()}
		}
	}
	if h.context.TriggerSchedulerTick != nil {
		if !reusedWorkerLoop || record.Status == string(domain.LoopStatusQueued) {
			h.context.TriggerSchedulerTick()
		}
	}

	if reusedWorkerLoop {
		title, prompt, effectiveSpecPath, baseBranch, issueNumber = reusedWorkerResponseFields(record, title, prompt, effectiveSpecPath, baseBranch, issueNumber)
	}

	response := workerCreateResponse{
		loopResponse: serializeLoop(record),
		Title:        title,
		Prompt:       prompt,
		SpecPath:     effectiveSpecPath,
		BaseBranch:   derefString(baseBranch),
		IssueNumber:  issueNumber,
		Reused:       reusedWorkerLoop,
	}

	return response, nil
}
