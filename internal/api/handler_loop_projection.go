package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/MumuTW/looper/internal/fixer"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func (h *Handler) logsRepositories() (*storage.Repositories, error) {
	if h.context.Runtime == nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	repos := h.context.Runtime.Services().Repositories
	if repos == nil || repos.Loops == nil || repos.Runs == nil || repos.AgentExecutions == nil {
		return nil, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Storage is not configured"}
	}
	return repos, nil
}

func (h *Handler) buildLoopLogsResponse(ctx context.Context, loop storage.LoopRecord) (loopLogsResponse, error) {
	repos, err := h.logsRepositories()
	if err != nil {
		return loopLogsResponse{}, err
	}
	if latestLoop, err := repos.Loops.GetByID(ctx, loop.ID); err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	} else if latestLoop != nil {
		loop = *latestLoop
	}

	latestRun, err := repos.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}

	return h.buildLogsResponseForRun(ctx, loop, latestRun)
}

func (h *Handler) buildRunLogsResponse(ctx context.Context, runID string) (loopLogsResponse, error) {
	repos, err := h.logsRepositories()
	if err != nil {
		return loopLogsResponse{}, err
	}
	run, err := repos.Runs.GetByID(ctx, runID)
	if err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if run == nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeRunNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Run not found: %s", runID)}
	}

	loop, err := repos.Loops.GetByID(ctx, run.LoopID)
	if err != nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	if loop == nil {
		return loopLogsResponse{}, apiError{code: pkgapi.ErrorCodeLoopNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Loop not found for run: %s", runID)}
	}

	return h.buildLogsResponseForRun(ctx, *loop, run)
}

func (h *Handler) buildLogsResponseForRun(ctx context.Context, loop storage.LoopRecord, run *storage.RunRecord) (loopLogsResponse, error) {
	response, _, err := h.buildLogsStateForRun(ctx, loop, run, true)
	return response, err
}

// buildLogsStateForRun is the single query/projection path for log snapshots
// and incremental follow metadata. Followers pass readPersisted=false so a
// state refresh never rereads the bounded log history; their file cursors own
// incremental bytes instead.
func (h *Handler) buildLogsStateForRun(ctx context.Context, loop storage.LoopRecord, run *storage.RunRecord, readPersisted bool) (loopLogsResponse, agentOutputPayload, error) {
	repos, err := h.logsRepositories()
	if err != nil {
		return loopLogsResponse{}, agentOutputPayload{}, err
	}
	var runPayload *loopLogsRunResponse
	var agentPayload *loopLogsAgentPayload
	var output agentOutputPayload
	if run != nil {
		runPayload = &loopLogsRunResponse{
			RunID:        run.ID,
			Status:       run.Status,
			CurrentStep:  run.CurrentStep,
			StartedAt:    run.StartedAt,
			EndedAt:      run.EndedAt,
			Summary:      run.Summary,
			ErrorMessage: run.ErrorMessage,
		}

		latestAgent, agentErr := repos.AgentExecutions.GetLatestByRunID(ctx, run.ID)
		if agentErr != nil {
			return loopLogsResponse{}, agentOutputPayload{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: agentErr.Error()}
		}
		if latestAgent != nil {
			output = decodeAgentOutput(latestAgent.OutputJSON)
			stdout, stderr := output.Stdout, output.Stderr
			if readPersisted {
				stdout, stderr = materializeAgentOutput(h.context.Config.Daemon.LogDir, output)
			}
			agentPayload = &loopLogsAgentPayload{
				ExecutionID:     latestAgent.ID,
				Vendor:          latestAgent.Vendor,
				Status:          latestAgent.Status,
				PID:             latestAgent.PID,
				StartedAt:       latestAgent.StartedAt,
				EndedAt:         latestAgent.EndedAt,
				HeartbeatCount:  latestAgent.HeartbeatCount,
				LastHeartbeatAt: latestAgent.LastHeartbeatAt,
				Summary:         latestAgent.Summary,
				ParseStatus:     latestAgent.ParseStatus,
				ErrorMessage:    latestAgent.ErrorMessage,
				Stdout:          stdout,
				Stderr:          stderr,
			}
		}
	}

	return loopLogsResponse{Seq: loop.Seq, LoopID: loop.ID, LoopType: loop.Type, LoopStatus: loop.Status, Run: runPayload, Agent: agentPayload}, output, nil
}

func serializeLoop(loop storage.LoopRecord) loopResponse {
	return loopResponse{
		ID:           loop.ID,
		Seq:          loop.Seq,
		ProjectID:    loop.ProjectID,
		Type:         loop.Type,
		TargetType:   loop.TargetType,
		TargetID:     loop.TargetID,
		Repo:         loop.Repo,
		PRNumber:     loop.PRNumber,
		Status:       loop.Status,
		ConfigJSON:   loop.ConfigJSON,
		MetadataJSON: loop.MetadataJSON,
		LastRunAt:    loop.LastRunAt,
		NextRunAt:    loop.NextRunAt,
		CreatedAt:    loop.CreatedAt,
		UpdatedAt:    loop.UpdatedAt,
	}
}

// serializeLoopWithDiagnostics loads latest queue/run and attaches attempt/error fields.
func (h *Handler) serializeLoopWithDiagnostics(ctx context.Context, loop storage.LoopRecord) (loopResponse, error) {
	view := serializeLoop(loop)
	services := h.context.Runtime.Services()
	var latestQueue *storage.QueueItemRecord
	var latestRun *storage.RunRecord
	if services.Repositories != nil && services.Repositories.Queue != nil {
		queue, err := services.Repositories.Queue.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		latestQueue = queue
	}
	if services.Repositories != nil && services.Repositories.Runs != nil {
		run, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
		if err != nil {
			return loopResponse{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
		}
		latestRun = run
	}
	decorateLoopDiagnostics(&view, latestQueue, latestRun)
	return view, nil
}

func serializeRun(run storage.RunRecord) runResponse {
	return runResponse{
		ID:                run.ID,
		LoopID:            run.LoopID,
		Status:            run.Status,
		CurrentStep:       run.CurrentStep,
		LastCompletedStep: run.LastCompletedStep,
		CheckpointJSON:    run.CheckpointJSON,
		Summary:           run.Summary,
		ErrorMessage:      run.ErrorMessage,
		StartedAt:         run.StartedAt,
		LastHeartbeatAt:   run.LastHeartbeatAt,
		EndedAt:           run.EndedAt,
		CreatedAt:         run.CreatedAt,
		UpdatedAt:         run.UpdatedAt,
		Outcome:           fixer.DeriveRunOutcome(run),
	}
}
