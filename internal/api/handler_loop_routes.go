package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MumuTW/looper/internal/domain"
	"github.com/MumuTW/looper/internal/storage"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func (h *Handler) buildLoopRouteResponse(r *http.Request, path string) (any, error) {
	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/loops/"), "/")
	selector, err := urlPathSegment(parts, 0)
	if err != nil {
		return nil, err
	}
	if len(parts) > 2 && strings.TrimSpace(parts[2]) != "" {
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}

	loop, err := h.resolveLoop(r.Context(), selector)
	if err != nil {
		return nil, err
	}

	if len(parts) == 1 || strings.TrimSpace(parts[1]) == "" {
		if r.Method != http.MethodGet {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.serializeLoopWithDiagnostics(r.Context(), loop)
	}

	subresource := parts[1]
	switch subresource {
	case "logs":
		if r.Method != http.MethodGet {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.buildLoopLogsResponse(r.Context(), loop)
	case "worktree":
		if r.Method != http.MethodGet {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.loopWorktreeStatus(r.Context(), loop)
	case "start":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.mutateLoopStatus(r.Context(), loop.ID, domain.LoopStatusRunning)
	case "pause":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.mutateLoopStatus(r.Context(), loop.ID, domain.LoopStatusPaused)
	case "retry":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.retryLoop(r.Context(), r, loop.ID, false)
	case "respond":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.respondToLoop(r.Context(), r, loop.ID)
	case "takeover":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.takeoverLoop(r.Context(), loop.ID)
	case "handback":
		if r.Method != http.MethodPost {
			return nil, apiError{code: pkgapi.ErrorCodeMethodNotAllowed, status: http.StatusMethodNotAllowed, message: fmt.Sprintf("Unsupported method for %s", path)}
		}
		return h.handbackLoop(r.Context(), r, loop.ID)
	default:
		return nil, apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}
}

func isFollowLoopLogsRequest(r *http.Request, path string) bool {
	if r.Method != http.MethodGet || !strings.HasSuffix(path, "/logs") {
		return false
	}
	value := strings.TrimSpace(r.URL.Query().Get("follow"))
	return value == "1" || strings.EqualFold(value, "true")
}

func (h *Handler) streamLoopLogsRoute(w http.ResponseWriter, r *http.Request, path string, requestID string) error {
	parts := strings.Split(strings.TrimPrefix(path, apiBasePath+"/loops/"), "/")
	selector, err := urlPathSegment(parts, 0)
	if err != nil {
		return err
	}
	if len(parts) != 2 || strings.TrimSpace(parts[1]) != "logs" {
		return apiError{code: pkgapi.ErrorCodeRouteNotFound, status: http.StatusNotFound, message: fmt.Sprintf("Unknown route: %s", path)}
	}

	loop, err := h.resolveLoop(r.Context(), selector)
	if err != nil {
		return err
	}

	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("streams")), "both") {
		return h.streamLoopLogsCombined(w, r, requestID, loop)
	}
	return h.streamLoopLogs(w, r, requestID, loop, queryBool(r.URL.Query(), "stderr"))
}

func (h *Handler) streamLoopLogs(w http.ResponseWriter, r *http.Request, requestID string, loop storage.LoopRecord, stderr bool) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Streaming is not supported by this response writer"}
	}

	state, err := h.buildLoopLogsCombinedState(r.Context(), loop)
	if err != nil {
		return err
	}
	requestedStderr := stderr
	streamStderr := requestedStderr || shouldDefaultLoopLogsStreamToStderr(state.response)
	cursor, err := h.newLoopLogsSingleCursor(state, streamStderr)
	if err != nil {
		return err
	}
	current := state.response
	applyLoopLogsSingleSnapshot(&current, cursor, streamStderr)

	w.Header().Set(requestIDHeaderName, requestID)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := writeSSEEvent(w, flusher, "snapshot", current); err != nil {
		return nil
	}
	h.observeLoopLogsFollow("snapshot_delivered", 0)

	observedRunID := ""
	if current.Run != nil {
		observedRunID = current.Run.RunID
	}
	executionID := ""
	if current.Agent != nil {
		executionID = current.Agent.ExecutionID
	}
	if shouldTerminateLoopLogsFollow(current, observedRunID) {
		_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
		return nil
	}

	ticker := time.NewTicker(loopLogsFollowPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-ticker.C:
		}

		nextState, stateErr := h.buildLoopLogsCombinedState(r.Context(), loop)
		if stateErr != nil {
			_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(stateErr))
			return nil
		}
		next := nextState.response
		if observedRunID == "" && next.Run != nil {
			observedRunID = next.Run.RunID
		}
		if shouldTerminateLoopLogsFollowBeforeChunk(next, observedRunID) {
			_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
			return nil
		}

		nextExecutionID := ""
		if next.Agent != nil {
			nextExecutionID = next.Agent.ExecutionID
		}
		nextStreamStderr := requestedStderr || shouldDefaultLoopLogsStreamToStderr(next)
		if nextExecutionID != executionID || nextStreamStderr != streamStderr {
			if err := h.emitLoopLogsSingleChunks(w, flusher, current, &cursor, true); err != nil {
				if errors.Is(err, errLoopLogsClientWrite) {
					return nil
				}
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
				return nil
			}
			nextCursor, cursorErr := h.newLoopLogsSingleCursor(nextState, nextStreamStderr)
			if cursorErr != nil {
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(cursorErr))
				return nil
			}
			cursor = nextCursor
			executionID = nextExecutionID
			streamStderr = nextStreamStderr
			if next.Agent != nil {
				if err := writeLoopLogsChunk(w, flusher, next, "", cursor.snapshotContent()); err != nil {
					return nil
				}
			}
		} else {
			if err := h.updateLoopLogsSingleCursor(w, flusher, next, nextState.output, &cursor, streamStderr, executionID); err != nil {
				if errors.Is(err, errLoopLogsClientWrite) {
					return nil
				}
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
				return nil
			}
			if err := h.emitLoopLogsSingleChunks(w, flusher, next, &cursor, false); err != nil {
				if errors.Is(err, errLoopLogsClientWrite) {
					return nil
				}
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
				return nil
			}
		}
		current = next

		if shouldTerminateLoopLogsFollow(current, observedRunID) {
			// Drain any remaining file bytes before ending. A burst exceeding
			// loopLogsFollowMaxChunkBytes between polls leaves more than one
			// chunk unread; the non-draining emit above only delivers one, and
			// without a drain here the terminal end event would close the stream
			// and silently omit the rest.
			if err := h.emitLoopLogsSingleChunks(w, flusher, current, &cursor, true); err != nil {
				if errors.Is(err, errLoopLogsClientWrite) {
					return nil
				}
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
				return nil
			}
			_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
			return nil
		}
	}
}

func newLoopLogsFollowErrorEvent(err error) loopLogsFollowErrorEvent {
	var typed apiError
	if !asAPIError(err, &typed) {
		typed = internalServerError(err)
	}
	retryable := typed.status >= http.StatusInternalServerError
	retryAfterMS := int64(0)
	if retryable {
		retryAfterMS = loopLogsFollowRetryAfter.Milliseconds()
	}
	return loopLogsFollowErrorEvent{
		Code:         typed.code,
		Message:      typed.message,
		Retryable:    retryable,
		RetryAfterMS: retryAfterMS,
	}
}

func queryBool(values url.Values, key string) bool {
	value := strings.TrimSpace(values.Get(key))
	return value == "1" || strings.EqualFold(value, "true")
}

func writeSSEEvent(w io.Writer, flusher http.Flusher, event string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func shouldDefaultLoopLogsStreamToStderr(resp loopLogsResponse) bool {
	if resp.Agent == nil {
		return false
	}
	return strings.TrimSpace(resp.Agent.Stdout) == "" && strings.TrimSpace(resp.Agent.Stderr) != ""
}

func appendedLogChunk(previousExecutionID, previousContent, currentExecutionID, currentContent string) string {
	if currentExecutionID == "" {
		return ""
	}
	if previousExecutionID == "" || currentExecutionID != previousExecutionID {
		return currentContent
	}
	if currentContent == previousContent {
		return ""
	}
	if strings.HasPrefix(currentContent, previousContent) {
		return currentContent[len(previousContent):]
	}
	return currentContent
}

func shouldTerminateLoopLogsFollow(resp loopLogsResponse, observedRunID string) bool {
	if observedRunID == "" {
		if resp.Run == nil {
			return !domain.IsActiveLoopStatus(domain.LoopStatus(resp.LoopStatus))
		}
		observedRunID = resp.Run.RunID
	}
	if resp.Run == nil {
		return true
	}
	if resp.Run.RunID != observedRunID {
		return true
	}
	return domain.IsTerminalRunStatus(domain.RunStatus(resp.Run.Status))
}

func shouldTerminateLoopLogsFollowBeforeChunk(resp loopLogsResponse, observedRunID string) bool {
	if !shouldTerminateLoopLogsFollow(resp, observedRunID) {
		return false
	}
	if observedRunID == "" {
		return resp.Run == nil
	}
	if resp.Run == nil {
		return true
	}
	return resp.Run.RunID != observedRunID
}
