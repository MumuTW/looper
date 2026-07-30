package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
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

	current, err := h.buildLoopLogsResponse(r.Context(), loop)
	if err != nil {
		return err
	}

	w.Header().Set(requestIDHeaderName, requestID)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := writeSSEEvent(w, flusher, "snapshot", current); err != nil {
		return nil
	}

	observedRunID := ""
	if current.Run != nil {
		observedRunID = current.Run.RunID
	}
	previousExecutionID, previousContent := loopLogsStreamState(current, stderr)
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

		current, err = h.buildLoopLogsResponse(r.Context(), loop)
		if err != nil {
			_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
			return nil
		}
		if observedRunID == "" && current.Run != nil {
			observedRunID = current.Run.RunID
		}
		if shouldTerminateLoopLogsFollowBeforeChunk(current, observedRunID) {
			_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
			return nil
		}

		nextExecutionID, nextContent := loopLogsStreamState(current, stderr)
		chunk := appendedLogChunk(previousExecutionID, previousContent, nextExecutionID, nextContent)
		if chunk != "" {
			event := loopLogsFollowChunkEvent{Content: chunk}
			if current.Run != nil {
				event.RunID = &current.Run.RunID
				event.CurrentStep = current.Run.CurrentStep
			}
			if current.Agent != nil {
				event.ExecutionID = &current.Agent.ExecutionID
				event.Vendor = &current.Agent.Vendor
				event.PID = current.Agent.PID
				event.Status = &current.Agent.Status
			}
			if err := writeSSEEvent(w, flusher, "chunk", event); err != nil {
				return nil
			}
		}

		previousExecutionID, previousContent = nextExecutionID, nextContent
		if shouldTerminateLoopLogsFollow(current, observedRunID) {
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

func loopLogsStreamState(resp loopLogsResponse, stderr bool) (string, string) {
	if resp.Agent == nil {
		return "", ""
	}
	content := resp.Agent.Stdout
	if stderr || shouldDefaultLoopLogsStreamToStderr(resp) {
		content = resp.Agent.Stderr
	}
	return resp.Agent.ExecutionID, content
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
