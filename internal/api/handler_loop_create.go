package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/eventlog"
	githubinfra "github.com/MumuTW/looper/internal/infra/github"
	"github.com/MumuTW/looper/internal/infra/shell"
	"github.com/MumuTW/looper/internal/loops"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
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
	if domain.LoopType(loopType) == domain.LoopTypeWorker && target.TargetType == domain.LoopTargetTypeIssue {
		metadataJSON, err = issueWorkerMetadataJSON(metadataJSON, target, derefBool(body.Force))
		if err != nil {
			return loopResponse{}, err
		}
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
	if err := h.validateCodingProjectRunnable(*project, domain.LoopType(loopType)); err != nil {
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

		candidateStatus := domain.LoopStatus(status)
		if (domain.LoopType(loopType) == domain.LoopTypeReviewer || domain.LoopType(loopType) == domain.LoopTypeFixer || domain.LoopType(loopType) == domain.LoopTypeWorker) && candidateStatus == domain.LoopStatusRunning {
			candidateStatus = domain.LoopStatusQueued
		}
		candidate := storage.LoopRecord{ProjectID: projectID, Type: loopType, TargetType: targetType, TargetID: loopTargetIDCompat(target), Repo: repoFromTargetCompat(target), PRNumber: prNumberFromTargetCompat(target), Status: string(candidateStatus), MetadataJSON: metadataJSON}
		if err := assertIssueClaimAdmission(r.Context(), transactionRepos, candidate, derefBool(body.Force)); err != nil {
			return storage.LoopRecord{}, err
		}

		existing, err := transactionRepos.Loops.List(r.Context())
		if err != nil {
			return storage.LoopRecord{}, err
		}
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
		record.Status = string(candidateStatus)
		if candidateStatus == domain.LoopStatusRunning {
			record.NextRunAt = &nowISO
		} else if candidateStatus == domain.LoopStatusQueued {
			record.NextRunAt = &nowISO
		}

		if err := upsertLoopAfterIssueClaimAdmission(r.Context(), transactionRepos.Loops, record, derefBool(body.Force)); err != nil {
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
	if err := h.validateCodingProjectRunnable(project, domain.LoopTypeWorker); err != nil {
		return workerCreateResponse{}, err
	}

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
		Title              string  `json:"title"`
		Prompt             *string `json:"prompt"`
		SpecPath           *string `json:"specPath"`
		Repo               string  `json:"repo"`
		BaseBranch         string  `json:"baseBranch"`
		IssueNumber        *int64  `json:"issueNumber,omitempty"`
		PRNumber           *int64  `json:"prNumber,omitempty"`
		IssueClaimOverride bool    `json:"issueClaimOverride,omitempty"`
	}{
		Title:              title,
		Prompt:             prompt,
		SpecPath:           effectiveSpecPath,
		Repo:               *repo,
		BaseBranch:         *baseBranch,
		IssueNumber:        issueNumber,
		PRNumber:           effectivePRNumber,
		IssueClaimOverride: issueNumber != nil && derefBool(body.Force),
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
	issueClaimCandidate := func(reusedLoopID string) storage.LoopRecord {
		return storage.LoopRecord{
			ID:           reusedLoopID,
			ProjectID:    projectID,
			Type:         string(domain.LoopTypeWorker),
			TargetType:   targetType,
			TargetID:     &targetID,
			Repo:         repo,
			PRNumber:     effectivePRNumber,
			Status:       string(domain.LoopStatusQueued),
			MetadataJSON: &metadataJSON,
		}
	}

	// Keep the eager 409 before publication, but model a reusable worker as its
	// existing lifecycle. A planner may retarget a new worker to a PR, while an
	// earlier issue worker is still the one that this request will resume.
	// Admission skips only that loop ID; independent source-issue lifecycles
	// remain conflicts. The transaction below is the authoritative recheck.
	if requestedIssueTarget != nil && !derefBool(body.Force) {
		existing, listErr := services.Repositories.Loops.List(r.Context())
		if listErr != nil {
			return workerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: listErr.Error()}
		}
		reusedLoopID := ""
		if existingLoop, _, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr != nil {
			return workerCreateResponse{}, reuseErr
		} else if ok {
			reusedLoopID = existingLoop.ID
		}
		if preflightErr := storage.WithTransaction(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) error {
			return assertIssueClaimAdmission(r.Context(), storage.NewRepositories(tx), issueClaimCandidate(reusedLoopID), false)
		}); preflightErr != nil {
			return workerCreateResponse{}, preflightErr
		}
	}

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
	if reuseStopGateLoopID != "" && h.workerReuseAfterClearStopGateHook != nil {
		h.workerReuseAfterClearStopGateHook(reuseStopGateLoopID)
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
		var reusableLoop *storage.LoopRecord
		if issueNumber != nil {
			if existingLoop, _, ok, reuseErr := reusableWorkerLoopForIssueRequestCompat(existing, projectID, *requestedIssueTarget, target); reuseErr != nil {
				return storage.LoopRecord{}, reuseErr
			} else if ok {
				loop := existingLoop
				reusableLoop = &loop
			}
		}
		if requestedIssueTarget != nil {
			reusedLoopID := ""
			if reusableLoop != nil {
				reusedLoopID = reusableLoop.ID
			}
			if admissionErr := assertIssueClaimAdmission(r.Context(), repos, issueClaimCandidate(reusedLoopID), derefBool(body.Force)); admissionErr != nil {
				return storage.LoopRecord{}, admissionErr
			}
		}
		if reusableLoop != nil {
			reusedWorkerLoop = true
			existingLoop := *reusableLoop
			existingTarget, targetErr := loopTargetFromRecordCompat(existingLoop)
			if targetErr != nil {
				return storage.LoopRecord{}, targetErr
			}
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
		if upsertErr := upsertLoopAfterIssueClaimAdmission(r.Context(), repos.Loops, record, derefBool(body.Force)); upsertErr != nil {
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

func (h *Handler) validateManualHoldBypassForLoopTarget(ctx context.Context, projectID string, loopType domain.LoopType, target domain.LoopTarget, force bool) error {
	if force || (loopType != domain.LoopTypePlanner && loopType != domain.LoopTypeWorker && loopType != domain.LoopTypeReviewer && loopType != domain.LoopTypeFixer) {
		return nil
	}
	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Repositories.Projects == nil {
		return nil
	}
	project, err := requireActiveProjectRecord(ctx, services.Repositories.Projects, projectID)
	if err != nil {
		return err
	}
	if target.TargetType != domain.LoopTargetTypeIssue && target.TargetType != domain.LoopTargetTypePullRequest {
		return nil
	}
	// An injected refresher is already the caller's explicit freshness authority;
	// unlike the default GitHub gateway, it does not require a local checkout or
	// a configured gh binary.
	if h.context.RefreshTargetLabels != nil {
		labels, refreshErr := h.refreshTargetLabels(ctx, target, project.RepoPath, "")
		if refreshErr != nil {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("refresh target before manual loop create: %v", refreshErr)}
		}
		if domain.IsAutoLaneHeld(loopType, labels) {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("target is currently held for %s; rerun with --force to bypass hold", loopType)}
		}
		return nil
	}
	// Hold preflight is best-effort at create time: when we cannot reliably talk to
	// GitHub from this handler context (missing repo path, missing gh path, etc.) we
	// skip validation rather than blocking manual creation for unrelated local setup.
	if strings.TrimSpace(project.RepoPath) == "" {
		return nil
	}
	if _, err := os.Stat(project.RepoPath); err != nil {
		return nil
	}
	ghPath := strings.TrimSpace(derefString(h.context.Config.Tools.GHPath))
	if ghPath == "" {
		return nil
	}
	labels, refreshErr := h.refreshTargetLabels(ctx, target, project.RepoPath, ghPath)
	if refreshErr != nil {
		return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("refresh target before manual loop create: %v", refreshErr)}
	}
	if !domain.IsAutoLaneHeld(loopType, labels) {
		return nil
	}
	return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("target is currently held for %s; rerun with --force to bypass hold", loopType)}
}

func (h *Handler) refreshTargetLabels(ctx context.Context, target domain.LoopTarget, cwd, ghPath string) ([]string, error) {
	if h.context.RefreshTargetLabels != nil {
		return h.context.RefreshTargetLabels(ctx, target, cwd)
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: ghPath, CWD: cwd, Env: config.DaemonGitHubCredentialEnv(h.context.Config), GHRun: shell.Run})
	switch target.TargetType {
	case domain.LoopTargetTypeIssue:
		return gh.GetIssueLabels(ctx, githubinfra.ViewIssueInput{Repo: target.Repo, IssueNumber: target.IssueNumber, CWD: cwd})
	case domain.LoopTargetTypePullRequest:
		detail, err := gh.ViewPullRequest(ctx, githubinfra.ViewPullRequestInput{Repo: target.Repo, PRNumber: target.PRNumber, CWD: cwd})
		return detail.Labels, err
	default:
		return nil, nil
	}
}

// refuseOccupiedIssueTarget asks the forge whether the issue is still open and
// whether any open pull request already references it, and refuses dispatch
// when either is true. The caller gates this on force=false and a configured
// LookupIssueOccupancy, so a force override or a forge-unreachable process
// keeps the local-only occupancy check from #319 working unchanged.
func (h *Handler) refuseOccupiedIssueTarget(ctx context.Context, project storage.ProjectRecord, repo string, issueNumber int64) error {
	occupancy, err := h.context.LookupIssueOccupancy(ctx, repo, issueNumber, project.RepoPath)
	if err != nil {
		if IsIssueLookupNotFound(err) {
			return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Issue %s#%d not found; refresh target before manual loop create", repo, issueNumber)}
		}
		// A transient forge outage must not hard-fail dispatch: the local check
		// still prevents self-collision, and prepare-work re-validates on claim.
		return nil
	}
	if !occupancy.Occupied() {
		return nil
	}
	reference := fmt.Sprintf("%s#%d", repo, issueNumber)
	if occupancy.IsPullRequest {
		return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s is a pull request, not an issue; rerun with --force to bypass", reference)}
	}
	if strings.TrimSpace(occupancy.State) != "" && !strings.EqualFold(strings.TrimSpace(occupancy.State), "open") {
		return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s is %s; rerun with --force to bypass", reference, strings.ToLower(occupancy.State))}
	}
	if len(occupancy.OpenPullRequests) > 0 {
		pr := occupancy.OpenPullRequests[0]
		prReference := fmt.Sprintf("#%d", pr.Number)
		if strings.TrimSpace(pr.Repo) != "" {
			prReference = fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
		}
		if strings.TrimSpace(pr.URL) != "" {
			prReference += " (" + pr.URL + ")"
		}
		return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("%s already has an open pull request %s; rerun with --force to bypass", reference, prReference)}
	}
	return nil
}

func derefBool(value *bool) bool {
	return value != nil && *value
}

func reusableWorkerLoopForIssueRequestCompat(existing []storage.LoopRecord, projectID string, issueTarget, effectiveTarget domain.LoopTarget) (storage.LoopRecord, domain.LoopTarget, bool, error) {
	for _, loop := range existing {
		if loop.ProjectID != projectID || loop.Type != string(domain.LoopTypeWorker) {
			continue
		}
		status := domain.LoopStatus(loop.Status)
		if !domain.IsConflictingActiveLoopStatus(status) {
			continue
		}
		loopTarget, err := loopTargetFromRecordCompat(loop)
		if err != nil {
			return storage.LoopRecord{}, domain.LoopTarget{}, false, err
		}
		key := loopTargetKeyFromRecordCompat(loop)
		if key != loopTargetKeyCompat(issueTarget) && key != loopTargetKeyCompat(effectiveTarget) {
			continue
		}
		return loop, loopTarget, true, nil
	}

	return storage.LoopRecord{}, domain.LoopTarget{}, false, nil
}

func (h *Handler) resumeReusableWorkerLoopCompat(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, target domain.LoopTarget, nowISO string, force bool) (storage.LoopRecord, error) {
	status := domain.LoopStatus(loop.Status)
	if force && status == domain.LoopStatusRunning {
		return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeLoopConflict, status: http.StatusConflict, message: fmt.Sprintf("Cannot force reuse running worker loop %s", loop.ID)}
	}
	if force {
		normalized, err := forceManualWorkerLoopStateCompat(ctx, repos, loop, nowISO)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		loop = normalized
	}
	shouldQueue := status == domain.LoopStatusIdle || status == domain.LoopStatusPaused || status == domain.LoopStatusQueued
	if status == domain.LoopStatusIdle || status == domain.LoopStatusPaused {
		if err := domain.AssertLoopStatusTransition(status, domain.LoopStatusQueued); err != nil {
			return storage.LoopRecord{}, err
		}
		loop.Status = string(domain.LoopStatusQueued)
		loop.NextRunAt = &nowISO
		loop.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			return storage.LoopRecord{}, err
		}
	}

	if shouldQueue {
		requeued, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, loop.ID, nowISO)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if requeued == 0 {
			activeQueue, findErr := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
			if findErr != nil {
				return storage.LoopRecord{}, findErr
			}
			if activeQueue == nil {
				latestQueue, latestErr := repos.Queue.GetLatestByLoopID(ctx, loop.ID)
				if latestErr != nil {
					return storage.LoopRecord{}, latestErr
				}
				if latestQueue != nil {
					if latestQueue.DedupeKey != "" {
						activeDedupe, dedupeErr := repos.Queue.FindActiveByDedupe(ctx, latestQueue.DedupeKey)
						if dedupeErr != nil {
							return storage.LoopRecord{}, dedupeErr
						}
						if activeDedupe != nil {
							return loop, nil
						}
					}
					replacement := *latestQueue
					replacement.ID = generateRequestID()
					replacement.Status = "queued"
					replacement.AvailableAt = nowISO
					replacement.Attempts = 0
					replacement.ClaimedBy = nil
					replacement.ClaimedAt = nil
					replacement.StartedAt = nil
					replacement.FinishedAt = nil
					replacement.LastError = nil
					replacement.LastErrorKind = nil
					replacement.CreatedAt = nowISO
					replacement.UpdatedAt = nowISO
					if force {
						replacement.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(replacement.PayloadJSON)
					}
					if _, _, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, replacement); err != nil {
						return storage.LoopRecord{}, err
					}
				} else {
					queueRecord, ok, queueErr := buildQueuedLoopQueueRecordCompat(loop, target, nowISO, loop.MetadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
					if queueErr != nil {
						return storage.LoopRecord{}, queueErr
					}
					if ok {
						if force {
							queueRecord.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(queueRecord.PayloadJSON)
						}
						if _, _, upsertQueueErr := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, queueRecord); upsertQueueErr != nil {
							return storage.LoopRecord{}, upsertQueueErr
						}
					}
				}
			} else if force {
				activeQueue.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(activeQueue.PayloadJSON)
				activeQueue.UpdatedAt = nowISO
				if err := repos.Queue.Upsert(ctx, *activeQueue); err != nil {
					return storage.LoopRecord{}, err
				}
			}
		} else if force {
			activeQueue, findErr := repos.Queue.FindActiveByLoopID(ctx, loop.ID)
			if findErr != nil {
				return storage.LoopRecord{}, findErr
			}
			if activeQueue != nil {
				activeQueue.PayloadJSON = forcedManualWorkerQueuePayloadJSONCompat(activeQueue.PayloadJSON)
				activeQueue.UpdatedAt = nowISO
				if err := repos.Queue.Upsert(ctx, *activeQueue); err != nil {
					return storage.LoopRecord{}, err
				}
			}
		}
	}

	return loop, nil
}

func forcedManualWorkerQueuePayloadJSONCompat(payloadJSON *string) *string {
	payload := parseJSONObject(payloadJSON)
	if len(payload) == 0 {
		return payloadJSON
	}
	delete(payload, "autoDiscovered")
	payload["issueClaimOverride"] = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		return payloadJSON
	}
	text := string(encoded)
	return &text
}

func forceManualWorkerLoopStateCompat(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) (storage.LoopRecord, error) {
	metadataJSON, err := forcedManualWorkerMetadataJSONCompat(loop.MetadataJSON)
	if err != nil {
		return storage.LoopRecord{}, err
	}
	if !stringPtrEqual(metadataJSON, loop.MetadataJSON) {
		loop.MetadataJSON = metadataJSON
		loop.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			return storage.LoopRecord{}, err
		}
	}
	if repos.Runs != nil {
		latestRun, err := repos.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if latestRun != nil {
			checkpointJSON := forcedManualWorkerCheckpointJSONCompat(latestRun.CheckpointJSON)
			if !stringPtrEqual(checkpointJSON, latestRun.CheckpointJSON) {
				latestRun.CheckpointJSON = checkpointJSON
				latestRun.UpdatedAt = nowISO
				if err := repos.Runs.Upsert(ctx, *latestRun); err != nil {
					return storage.LoopRecord{}, err
				}
			}
		}
	}
	return loop, nil
}

// issueWorkerMetadataJSON records the source identity used after a worker
// retargets to a pull request. Only force=true may create the durable override:
// callers control generic metadata, so an incoming override is not authority.
func issueWorkerMetadataJSON(metadataJSON *string, target domain.LoopTarget, force bool) (*string, error) {
	metadata := parseJSONObject(metadataJSON)
	if metadata == nil {
		metadata = map[string]any{}
	}
	worker, _ := metadata["worker"].(map[string]any)
	if worker == nil {
		worker = map[string]any{}
	}
	delete(worker, "issueClaimOverride")
	if force {
		worker["issueClaimOverride"] = true
	}
	worker["repo"] = target.Repo
	worker["issueNumber"] = target.IssueNumber
	metadata["worker"] = worker
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	text := string(encoded)
	return &text, nil
}

func forcedManualWorkerMetadataJSONCompat(metadataJSON *string) (*string, error) {
	metadata, err := loops.DecodeMetadataObjectForWrite(metadataJSON)
	if err != nil {
		return nil, err
	}
	worker, _ := metadata["worker"].(map[string]any)
	if worker == nil {
		worker = map[string]any{}
	}
	delete(worker, "autoDiscovered")
	worker["issueClaimOverride"] = true
	metadata["worker"] = worker
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	text := string(encoded)
	return &text, nil
}

func forcedManualWorkerCheckpointJSONCompat(checkpointJSON *string) *string {
	checkpoint := parseJSONObject(checkpointJSON)
	if len(checkpoint) == 0 {
		return checkpointJSON
	}
	work, ok := checkpoint["work"].(map[string]any)
	if !ok {
		return checkpointJSON
	}
	delete(work, "autoDiscovered")
	work["issueClaimOverride"] = true
	checkpoint["work"] = work
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return checkpointJSON
	}
	text := string(encoded)
	return &text
}

func stringPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func reusedWorkerResponseFields(loop storage.LoopRecord, fallbackTitle string, fallbackPrompt, fallbackSpecPath, fallbackBaseBranch *string, fallbackIssueNumber *int64) (string, *string, *string, *string, *int64) {
	metadata := parseJSONObject(loop.MetadataJSON)
	worker, _ := metadata["worker"].(map[string]any)
	title := fallbackTitle
	if value := readStringAny(worker["title"]); value != nil {
		title = *value
	}
	prompt := fallbackPrompt
	if value := readStringAny(worker["prompt"]); value != nil {
		prompt = value
	}
	specPath := fallbackSpecPath
	if value := readStringAny(worker["specPath"]); value != nil {
		specPath = value
	}
	baseBranch := fallbackBaseBranch
	if value := readStringAny(worker["baseBranch"]); value != nil {
		baseBranch = value
	}
	issueNumber := fallbackIssueNumber
	if value := int64MetadataPtr(worker, "issueNumber"); value != nil {
		issueNumber = value
	}
	return title, prompt, specPath, baseBranch, issueNumber
}

func (h *Handler) buildPlannersCreateResponse(r *http.Request) (plannerCreateResponse, error) {
	if r.Method != http.MethodPost {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/planners")}
	}
	if !isCodingRoleAgentConfigured(h.effectiveConfig(), config.CodingRolePlanner) {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeAgentNotConfigured, status: http.StatusBadRequest, message: "Cannot create planner loop without config.agent.vendor"}
	}

	services := h.context.Runtime.Services()
	if services.Repositories == nil || services.Coordinator == nil {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}

	body := createPlannerRequest{}
	if aerr := decodeJSONMutationBody(r, &body, true); aerr != nil {
		return plannerCreateResponse{}, *aerr
	}

	projectID := strings.TrimSpace(derefString(body.ProjectID))
	if projectID == "" {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "projectId is required"}
	}
	project, err := requireActiveProjectRecord(r.Context(), services.Repositories.Projects, projectID)
	if err != nil {
		return plannerCreateResponse{}, err
	}

	issueNumber := normalizePositiveInt64Ptr(body.IssueNumber)
	if issueNumber == nil {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "issueNumber must be a positive integer"}
	}

	repo := stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")
	if repo == nil {
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "project repo is required"}
	}
	target := domain.LoopTarget{TargetType: domain.LoopTargetTypeIssue, Repo: *repo, IssueNumber: *issueNumber}
	if err := h.validateManualHoldBypassForLoopTarget(r.Context(), projectID, domain.LoopTypePlanner, target, derefBool(body.Force)); err != nil {
		return plannerCreateResponse{}, err
	}

	// Share same-target lock with discard+retry so planner uniqueness races
	// cannot interleave with requeue while discard mutates the worktree.
	unlockPlannerTarget := h.lockLoopTargetForStatus(projectID, domain.LoopTypePlanner, target, domain.LoopStatusRunning)
	defer unlockPlannerTarget()

	nowISO := eventlog.FormatJavaScriptISOString(h.now().UTC())
	targetID := fmt.Sprintf("issue:%s:%d", *repo, *issueNumber)
	metadataJSONPtr, err := manualPlannerMetadataJSON(nil, *issueNumber)
	if err != nil {
		return plannerCreateResponse{}, err
	}
	metadataJSON := derefString(metadataJSONPtr)

	record, err := storage.WithTransactionValue(r.Context(), services.Coordinator.DB(), nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)
		seq, seqErr := repos.Loops.AllocateSeq(r.Context())
		if seqErr != nil {
			return storage.LoopRecord{}, seqErr
		}

		existing, listErr := repos.Loops.List(r.Context())
		if listErr != nil {
			return storage.LoopRecord{}, listErr
		}
		if uniqueErr := assertUniqueActiveLoopCompat(existing, "", projectID, domain.LoopTypePlanner, target, domain.LoopStatusRunning); uniqueErr != nil {
			return storage.LoopRecord{}, uniqueErr
		}

		record := storage.LoopRecord{
			ID:           generateRequestID(),
			Seq:          seq,
			ProjectID:    projectID,
			Type:         string(domain.LoopTypePlanner),
			TargetType:   string(domain.LoopTargetTypeIssue),
			TargetID:     &targetID,
			Repo:         repo,
			PRNumber:     nil,
			Status:       string(domain.LoopStatusRunning),
			ConfigJSON:   nil,
			MetadataJSON: &metadataJSON,
			NextRunAt:    &nowISO,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if upsertErr := repos.Loops.Upsert(r.Context(), record); upsertErr != nil {
			return storage.LoopRecord{}, upsertErr
		}

		queueRecord, ok, queueErr := buildQueuedLoopQueueRecordCompat(record, target, nowISO, &metadataJSON, int64(h.context.Config.Scheduler.RetryMaxAttempts))
		if queueErr != nil {
			return storage.LoopRecord{}, queueErr
		}
		if !ok {
			return storage.LoopRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "failed to build planner queue item"}
		}
		if upsertQueueErr := repos.Queue.Upsert(r.Context(), queueRecord); upsertQueueErr != nil {
			return storage.LoopRecord{}, upsertQueueErr
		}

		return record, nil
	})
	if err != nil {
		var typed apiError
		if asAPIError(err, &typed) {
			return plannerCreateResponse{}, typed
		}
		return plannerCreateResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if h.context.TriggerSchedulerTick != nil {
		h.context.TriggerSchedulerTick()
	}

	return plannerCreateResponse{loopResponse: serializeLoop(record), IssueNumber: *issueNumber}, nil
}

type resolveWorkerProjectInput struct {
	ProjectID *string
	Repo      *string
	PRNumber  *int64
}

func (h *Handler) resolveWorkerProject(ctx context.Context, input resolveWorkerProjectInput) (storage.ProjectRecord, error) {
	services := h.context.Runtime.Services()
	if input.ProjectID != nil {
		project, err := requireActiveProjectRecord(ctx, services.Repositories.Projects, *input.ProjectID)
		if err != nil {
			return storage.ProjectRecord{}, err
		}
		if input.Repo != nil {
			configuredRepo := strings.TrimSpace(derefString(stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")))
			requestedRepo := strings.TrimSpace(*input.Repo)
			if configuredRepo != "" && configuredRepo != requestedRepo {
				if input.PRNumber != nil {
					return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodePullRequestProjectMismatch, status: http.StatusConflict, message: fmt.Sprintf("Pull request %s#%d does not belong to project %s", requestedRepo, *input.PRNumber, *input.ProjectID)}
				}
				return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("project %s is configured for repo %s, not %s", *input.ProjectID, configuredRepo, requestedRepo)}
			}
		}
		return *project, nil
	}

	if input.Repo != nil && input.PRNumber != nil {
		requestedRepo := strings.TrimSpace(*input.Repo)
		snapshots, err := services.Repositories.PullRequestSnapshots.List(ctx)
		if err != nil {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		matchedProjectIDs := map[string]struct{}{}
		for _, snapshot := range snapshots {
			if snapshot.Repo == requestedRepo && snapshot.PRNumber == *input.PRNumber {
				project, getErr := services.Repositories.Projects.GetByID(ctx, snapshot.ProjectID)
				if getErr != nil {
					return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: getErr.Error()}
				}
				if project != nil && !project.Archived {
					configuredRepo := strings.TrimSpace(derefString(stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")))
					if configuredRepo != "" && configuredRepo != requestedRepo {
						continue
					}
					matchedProjectIDs[snapshot.ProjectID] = struct{}{}
				}
			}
		}
		if len(matchedProjectIDs) > 1 {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: fmt.Sprintf("Multiple projects match pull request %s#%d; pass projectId explicitly", *input.Repo, *input.PRNumber)}
		}
		for projectID := range matchedProjectIDs {
			project, getErr := services.Repositories.Projects.GetByID(ctx, projectID)
			if getErr != nil {
				return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: getErr.Error()}
			}
			if project != nil {
				return *project, nil
			}
		}
	}

	if input.Repo != nil {
		projectsList, err := services.Repositories.Projects.List(ctx)
		if err != nil {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		matches := make([]storage.ProjectRecord, 0)
		for _, candidate := range projectsList {
			if candidate.Archived {
				continue
			}
			candidateRepo := stringMetadataPtr(parseProjectMetadata(candidate.MetadataJSON), "repo")
			if candidateRepo != nil && *candidateRepo == *input.Repo {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeProjectAmbiguous, status: http.StatusConflict, message: fmt.Sprintf("Multiple projects match repo %s; pass projectId explicitly", *input.Repo)}
		}
	}

	return storage.ProjectRecord{}, apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: "projectId is required unless it can be resolved from repo/prNumber"}
}

type requirePullRequestTargetInput struct {
	ProjectID string
	Repo      string
	PRNumber  int64
}

func (h *Handler) requirePullRequestTarget(ctx context.Context, input requirePullRequestTargetInput) (int64, error) {
	services := h.context.Runtime.Services()
	project, err := requireActiveProjectRecord(ctx, services.Repositories.Projects, input.ProjectID)
	if err != nil {
		return 0, err
	}
	projectRepo := stringMetadataPtr(parseProjectMetadata(project.MetadataJSON), "repo")
	if projectRepo == nil || *projectRepo != input.Repo {
		return 0, apiError{code: pkgapi.ErrorCodePullRequestProjectMismatch, status: http.StatusConflict, message: fmt.Sprintf("Pull request %s#%d does not belong to project %s", input.Repo, input.PRNumber, input.ProjectID)}
	}
	snapshot, err := services.Repositories.PullRequestSnapshots.GetLatestByProject(ctx, input.ProjectID, input.Repo, input.PRNumber)
	if err != nil {
		return 0, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if snapshot == nil {
		// Snapshots are written by Reviewer and by project discovery, and neither
		// has to have run before a human decides this pull request needs Worker.
		// Missing snapshot means "not seen yet", not "does not exist", so ask the
		// forge rather than refusing work that is perfectly dispatchable.
		return h.resolveUnsnapshottedPullRequestTarget(ctx, *project, input)
	}
	if snapshot.ProjectID != input.ProjectID {
		return 0, apiError{code: pkgapi.ErrorCodePullRequestProjectMismatch, status: http.StatusConflict, message: fmt.Sprintf("Pull request %s#%d does not belong to project %s", input.Repo, input.PRNumber, input.ProjectID)}
	}
	// A snapshot captured while open does not prove the pull request is still
	// open: project discovery only writes open PRs and never refreshes a
	// snapshot after closure, so a stale-open snapshot would otherwise queue an
	// expensive Worker run that pushes commits onto a closed or merged PR. The
	// forge is the authority for current state; refresh it before accepting.
	// When no lookup is configured, fall back to the snapshot's recorded state
	// so a process without forge access keeps its previous behavior.
	if h.context.LookupPullRequest != nil {
		target, lookupErr := h.context.LookupPullRequest(ctx, input.Repo, input.PRNumber, project.RepoPath)
		return h.acceptFreshPullRequestTarget(input, target, lookupErr)
	}
	if isOpen, known, stateErr := h.getPlannerPullRequestOpenState(ctx, input.ProjectID, input.Repo, input.PRNumber); stateErr == nil && known && !isOpen {
		return 0, closedPullRequestTargetError(input.Repo, input.PRNumber)
	}
	return snapshot.PRNumber, nil
}

// resolveUnsnapshottedPullRequestTarget answers the same question as its caller
// for a pull request Looper has no snapshot of. LookupPullRequest is optional;
// without it an unsnapshotted pull request stays unresolvable, which is the
// behavior every caller had before the forge fallback existed.
func (h *Handler) resolveUnsnapshottedPullRequestTarget(ctx context.Context, project storage.ProjectRecord, input requirePullRequestTargetInput) (int64, error) {
	if h.context.LookupPullRequest == nil {
		return 0, apiError{code: pkgapi.ErrorCodePullRequestNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Pull request not found: %s#%d", input.Repo, input.PRNumber)}
	}
	target, err := h.context.LookupPullRequest(ctx, input.Repo, input.PRNumber, project.RepoPath)
	return h.acceptFreshPullRequestTarget(input, target, err)
}

// acceptFreshPullRequestTarget turns a fresh forge lookup into the resolved PR
// number or an API error. Only a classified "does not exist" result is a 404;
// any other lookup failure is a retryable server error so a transient forge
// outage is not reported as a permanently missing target. A PR the forge
// confirms closed or merged is rejected before work is queued.
func (h *Handler) acceptFreshPullRequestTarget(input requirePullRequestTargetInput, target PullRequestTarget, err error) (int64, error) {
	if err != nil {
		if IsPullRequestLookupNotFound(err) {
			return 0, apiError{code: pkgapi.ErrorCodePullRequestNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Pull request not found: %s#%d (%s)", input.Repo, input.PRNumber, err.Error())}
		}
		return 0, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: fmt.Sprintf("Pull request lookup failed: %s#%d (%s)", input.Repo, input.PRNumber, err.Error())}
	}
	if target.Number <= 0 {
		return 0, apiError{code: pkgapi.ErrorCodePullRequestNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Pull request not found: %s#%d", input.Repo, input.PRNumber)}
	}
	if target.Merged || (strings.TrimSpace(target.State) != "" && !strings.EqualFold(strings.TrimSpace(target.State), "open")) {
		return 0, closedPullRequestTargetError(input.Repo, input.PRNumber)
	}
	return target.Number, nil
}

func closedPullRequestTargetError(repo string, prNumber int64) error {
	return apiError{code: pkgapi.ErrorCodeValidationFailed, status: http.StatusBadRequest, message: fmt.Sprintf("Pull request %s#%d is not open", repo, prNumber)}
}

type findPlannerLoopForIssueInput struct {
	ProjectID   string
	Repo        string
	IssueNumber int64
}

type workerPlannerMatch struct {
	PRNumber *int64
	SpecPath *string
}

func (h *Handler) maybeFindPlannerLoopForIssue(ctx context.Context, input findPlannerLoopForIssueInput) (*workerPlannerMatch, error) {
	loopsList, err := h.context.Runtime.Services().Repositories.Loops.List(ctx)
	if err != nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	targetID := fmt.Sprintf("issue:%s:%d", input.Repo, input.IssueNumber)
	for _, loop := range loopsList {
		if loop.ProjectID != input.ProjectID || loop.Type != string(domain.LoopTypePlanner) || loop.TargetType != string(domain.LoopTargetTypeIssue) || derefString(loop.TargetID) != targetID {
			continue
		}
		metadata := parseProjectMetadata(loop.MetadataJSON)
		prNumber := loop.PRNumber
		if prNumber == nil {
			prNumber = int64MetadataPtr(metadata, "prNumber")
		}
		match := &workerPlannerMatch{PRNumber: prNumber, SpecPath: stringMetadataPtr(metadata, "specPath")}
		if prNumber == nil {
			return &workerPlannerMatch{PRNumber: nil, SpecPath: match.SpecPath}, nil
		}
		isOpen, known, err := h.getPlannerPullRequestOpenState(ctx, input.ProjectID, input.Repo, *prNumber)
		if err != nil {
			return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		if known && !isOpen {
			return &workerPlannerMatch{PRNumber: nil, SpecPath: match.SpecPath}, nil
		}
		return match, nil
	}
	return nil, nil
}

func (h *Handler) getPlannerPullRequestOpenState(ctx context.Context, projectID, repo string, prNumber int64) (bool, bool, error) {
	if prNumber <= 0 {
		return false, true, nil
	}
	snapshot, err := h.context.Runtime.Services().Repositories.PullRequestSnapshots.GetLatestByProject(ctx, projectID, repo, prNumber)
	if err != nil {
		return false, false, err
	}
	if snapshot == nil {
		return false, false, nil
	}
	payload := parseJSONObject(snapshot.PayloadJSON)
	detail, _ := payload["detail"].(map[string]any)
	state := firstNonEmptyString(readStringAny(detail["state"]), readStringAny(detail["State"]))
	if state == nil {
		return false, false, nil
	}
	return strings.EqualFold(*state, "open"), true, nil
}

func deriveWorkerTitle(prompt, specPath, repo *string, prNumber, issueNumber *int64) string {
	if prompt != nil {
		runes := []rune(*prompt)
		if len(runes) > 80 {
			return string(runes[:80])
		}
		return *prompt
	}
	if specPath != nil {
		return "Implement " + *specPath
	}
	if prNumber != nil && repo != nil {
		return fmt.Sprintf("Implement %s#%d", *repo, *prNumber)
	}
	if issueNumber != nil && repo != nil {
		return fmt.Sprintf("Implement %s#%d", *repo, *issueNumber)
	}
	return "Worker run"
}

func normalizePositiveInt64Ptr(value *int64) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	v := *value
	return &v
}

func int64MetadataPtr(metadata map[string]any, key string) *int64 {
	value, ok := metadata[key]
	if !ok {
		return nil
	}
	floatValue, ok := value.(float64)
	if !ok || floatValue <= 0 || floatValue != float64(int64(floatValue)) {
		return nil
	}
	parsed := int64(floatValue)
	return &parsed
}
