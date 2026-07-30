package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nexu-io/looper/internal/storage"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

type loopLogsFollowObservation struct {
	Kind  string
	Bytes int
}

type loopLogsCombinedState struct {
	response loopLogsResponse
	output   agentOutputPayload
}

type loopLogsFileCursor struct {
	path       string
	offset     int64
	fileInfo   os.FileInfo
	lastInline string
	snapshot   string
}

type loopLogsCombinedCursor struct {
	executionID string
	stdout      loopLogsFileCursor
	stderr      loopLogsFileCursor
}

// streamLoopLogsCombined is the dashboard's follow contract. One connection
// owns both stdout and stderr, so one state refresh serves both streams. Log
// files are tailed by offset every 200ms; durable state is refreshed only once
// per second and never rereads persisted log history.
func (h *Handler) streamLoopLogsCombined(w http.ResponseWriter, r *http.Request, requestID string, loop storage.LoopRecord) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: "Streaming is not supported by this response writer"}
	}

	current, err := h.buildLoopLogsCombinedState(r.Context(), loop)
	if err != nil {
		return err
	}
	cursor, err := h.newLoopLogsCombinedCursor(current)
	if err != nil {
		return err
	}
	cursor.applySnapshot(&current.response)

	w.Header().Set(requestIDHeaderName, requestID)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := writeSSEEvent(w, flusher, "snapshot", current.response); err != nil {
		return nil
	}
	h.observeLoopLogsFollow("snapshot_delivered", 0)

	observedRunID := ""
	if current.response.Run != nil {
		observedRunID = current.response.Run.RunID
	}
	if shouldTerminateLoopLogsFollow(current.response, observedRunID) {
		_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
		return nil
	}

	readTicker := time.NewTicker(loopLogsFollowPollInterval)
	defer readTicker.Stop()
	stateTicker := time.NewTicker(loopLogsFollowStatePollInterval)
	defer stateTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-readTicker.C:
			if err := h.emitLoopLogsFileChunks(w, flusher, current.response, &cursor, false); err != nil {
				if errors.Is(err, errLoopLogsClientWrite) {
					return nil
				}
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
				return nil
			}
		case <-stateTicker.C:
			next, stateErr := h.buildLoopLogsCombinedState(r.Context(), loop)
			if stateErr != nil {
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(stateErr))
				return nil
			}
			if observedRunID == "" && next.response.Run != nil {
				observedRunID = next.response.Run.RunID
			}
			if shouldTerminateLoopLogsFollowBeforeChunk(next.response, observedRunID) {
				_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
				return nil
			}

			if err := h.updateLoopLogsCombinedCursor(w, flusher, next, &cursor); err != nil {
				if errors.Is(err, errLoopLogsClientWrite) {
					return nil
				}
				_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
				return nil
			}
			current = next
			terminal := shouldTerminateLoopLogsFollow(current.response, observedRunID)
			if terminal {
				if err := h.emitLoopLogsFileChunks(w, flusher, current.response, &cursor, true); err != nil {
					if errors.Is(err, errLoopLogsClientWrite) {
						return nil
					}
					_ = writeSSEEvent(w, flusher, "error", newLoopLogsFollowErrorEvent(err))
					return nil
				}
			}
			if terminal {
				_ = writeSSEEvent(w, flusher, "end", map[string]string{"reason": "run_completed"})
				return nil
			}
		}
	}
}

var errLoopLogsClientWrite = errors.New("loop logs client write failed")

func (h *Handler) buildLoopLogsCombinedState(ctx context.Context, loop storage.LoopRecord) (loopLogsCombinedState, error) {
	h.observeLoopLogsFollow("state_refresh", 0)
	services := h.context.Runtime.Services()
	if latestLoop, err := services.Repositories.Loops.GetByID(ctx, loop.ID); err != nil {
		return loopLogsCombinedState{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	} else if latestLoop != nil {
		loop = *latestLoop
	}
	latestRun, err := services.Repositories.Runs.GetLatestByLoopID(ctx, loop.ID)
	if err != nil {
		return loopLogsCombinedState{}, apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
	}
	response, output, err := h.buildLogsStateForRun(ctx, loop, latestRun, false)
	if err != nil {
		return loopLogsCombinedState{}, err
	}
	return loopLogsCombinedState{response: response, output: output}, nil
}

func (h *Handler) newLoopLogsCombinedCursor(state loopLogsCombinedState) (loopLogsCombinedCursor, error) {
	executionID := ""
	if state.response.Agent != nil {
		executionID = state.response.Agent.ExecutionID
	}
	stdout, err := h.newLoopLogsFileCursor(state.output.StdoutLogPath, state.output.Stdout)
	if err != nil {
		return loopLogsCombinedCursor{}, err
	}
	stderr, err := h.newLoopLogsFileCursor(state.output.StderrLogPath, state.output.Stderr)
	if err != nil {
		return loopLogsCombinedCursor{}, err
	}
	return loopLogsCombinedCursor{executionID: executionID, stdout: stdout, stderr: stderr}, nil
}

func (h *Handler) newLoopLogsFileCursor(path, inline string) (loopLogsFileCursor, error) {
	cursor := loopLogsFileCursor{lastInline: inline}
	if strings.TrimSpace(path) == "" || !isPathWithinDirectory(path, h.context.Config.Daemon.LogDir) {
		return cursor, nil
	}
	content, offset, info, found, err := readLoopLogsSnapshot(path, loopLogsFollowSnapshotBytes)
	if err != nil {
		return loopLogsFileCursor{}, err
	}
	if found {
		cursor.path = path
		cursor.offset = offset
		cursor.fileInfo = info
		cursor.snapshot = content
	}
	return cursor, nil
}

func (cursor loopLogsCombinedCursor) applySnapshot(response *loopLogsResponse) {
	if response.Agent == nil {
		return
	}
	response.Agent.Stdout = tailLogBytes(cursor.stdout.snapshotContent(), loopLogsFollowSnapshotBytes)
	response.Agent.Stderr = tailLogBytes(cursor.stderr.snapshotContent(), loopLogsFollowSnapshotBytes)
}

func (cursor loopLogsFileCursor) snapshotContent() string {
	if cursor.snapshot != "" || (cursor.path != "" && cursor.offset == 0) {
		return cursor.snapshot
	}
	return cursor.lastInline
}

func (h *Handler) updateLoopLogsCombinedCursor(w io.Writer, flusher http.Flusher, state loopLogsCombinedState, cursor *loopLogsCombinedCursor) error {
	nextExecutionID := ""
	if state.response.Agent != nil {
		nextExecutionID = state.response.Agent.ExecutionID
	}
	if nextExecutionID != cursor.executionID {
		next, err := h.newLoopLogsCombinedCursor(state)
		if err != nil {
			return err
		}
		cursor.executionID = next.executionID
		cursor.stdout = next.stdout
		cursor.stderr = next.stderr
		if state.response.Agent != nil {
			if err := writeLoopLogsChunk(w, flusher, state.response, "stdout", cursor.stdout.snapshotContent()); err != nil {
				return errLoopLogsClientWrite
			}
			if err := writeLoopLogsChunk(w, flusher, state.response, "stderr", cursor.stderr.snapshotContent()); err != nil {
				return errLoopLogsClientWrite
			}
		}
		return nil
	}

	if state.response.Agent == nil {
		return nil
	}
	for _, update := range []struct {
		stream string
		cursor *loopLogsFileCursor
		path   string
		inline string
	}{
		{stream: "stdout", cursor: &cursor.stdout, path: state.output.StdoutLogPath, inline: state.output.Stdout},
		{stream: "stderr", cursor: &cursor.stderr, path: state.output.StderrLogPath, inline: state.output.Stderr},
	} {
		if update.cursor.path == "" && strings.TrimSpace(update.path) != "" && isPathWithinDirectory(update.path, h.context.Config.Daemon.LogDir) {
			known := update.cursor.lastInline
			next, err := h.newLoopLogsFileCursor(update.path, update.inline)
			if err != nil {
				return err
			}
			*update.cursor = next
			gap := logContentAfterKnown(next.snapshotContent(), known)
			if err := writeLoopLogsChunk(w, flusher, state.response, update.stream, gap); err != nil {
				return errLoopLogsClientWrite
			}
			continue
		}
		if update.cursor.path == "" {
			chunk := appendedLogChunk(cursor.executionID, update.cursor.lastInline, cursor.executionID, update.inline)
			update.cursor.lastInline = update.inline
			if err := writeLoopLogsChunk(w, flusher, state.response, update.stream, chunk); err != nil {
				return errLoopLogsClientWrite
			}
		} else {
			update.cursor.lastInline = update.inline
		}
	}
	return nil
}

func (h *Handler) emitLoopLogsFileChunks(w io.Writer, flusher http.Flusher, response loopLogsResponse, cursor *loopLogsCombinedCursor, drain bool) error {
	for _, item := range []struct {
		stream string
		cursor *loopLogsFileCursor
	}{
		{stream: "stdout", cursor: &cursor.stdout},
		{stream: "stderr", cursor: &cursor.stderr},
	} {
		for {
			chunk, attempted, err := item.cursor.readNext(loopLogsFollowMaxChunkBytes)
			if attempted {
				h.observeLoopLogsFollow("file_read", len(chunk))
			}
			if err != nil {
				return apiError{code: pkgapi.ErrorCodeInternalError, status: http.StatusInternalServerError, message: err.Error()}
			}
			if chunk == "" {
				break
			}
			if err := writeLoopLogsChunk(w, flusher, response, item.stream, chunk); err != nil {
				return errLoopLogsClientWrite
			}
			if !drain {
				break
			}
		}
	}
	return nil
}

func writeLoopLogsChunk(w io.Writer, flusher http.Flusher, response loopLogsResponse, stream, content string) error {
	for content != "" {
		chunk, remaining := splitLoopLogsChunk(content, loopLogsFollowMaxChunkBytes)
		event := loopLogsFollowChunkEvent{Stream: stream, Content: chunk}
		if response.Run != nil {
			event.RunID = &response.Run.RunID
			event.CurrentStep = response.Run.CurrentStep
		}
		if response.Agent != nil {
			event.ExecutionID = &response.Agent.ExecutionID
			event.Vendor = &response.Agent.Vendor
			event.PID = response.Agent.PID
			event.Status = &response.Agent.Status
		}
		if err := writeSSEEvent(w, flusher, "chunk", event); err != nil {
			return err
		}
		content = remaining
	}
	return nil
}

// splitLoopLogsChunk keeps the wire chunk bound in bytes while never splitting
// a valid UTF-8 rune. File cursors use the same boundary rule before converting
// raw bytes to strings.
func splitLoopLogsChunk(content string, maxBytes int) (string, string) {
	if len(content) <= maxBytes {
		return content, ""
	}
	cut := utf8ChunkBoundary([]byte(content), maxBytes)
	if cut == 0 {
		// maxBytes is much larger than utf8.UTFMax in production. Retain a
		// deterministic escape hatch for malformed input and defensive tests.
		cut = maxBytes
	}
	return content[:cut], content[cut:]
}

func (cursor *loopLogsFileCursor) readNext(maxBytes int) (string, bool, error) {
	if cursor.path == "" {
		return "", false, nil
	}
	file, err := os.Open(cursor.path)
	if errors.Is(err, os.ErrNotExist) {
		// A disappeared file must not suppress the durable inline fallback.
		// The next state refresh will either reopen a replacement or resume
		// incremental inline delivery.
		cursor.path = ""
		cursor.offset = 0
		cursor.fileInfo = nil
		return "", true, nil
	}
	if err != nil {
		return "", true, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", true, err
	}
	if cursor.fileInfo != nil && !os.SameFile(cursor.fileInfo, info) {
		// Rename-and-recreate rotation can leave the replacement at the same
		// size (or larger) than the old file, so size alone is insufficient.
		cursor.offset = 0
	}
	cursor.fileInfo = info
	if info.Size() < cursor.offset {
		cursor.offset = 0
	}
	if info.Size() == cursor.offset {
		return "", true, nil
	}
	remaining := info.Size() - cursor.offset
	if remaining > int64(maxBytes) {
		remaining = int64(maxBytes)
	}
	if _, err := file.Seek(cursor.offset, io.SeekStart); err != nil {
		return "", true, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, remaining))
	if err != nil {
		return "", true, err
	}
	if cut := utf8ChunkBoundary(raw, len(raw)); cut < len(raw) {
		raw = raw[:cut]
	}
	cursor.offset += int64(len(raw))
	return string(raw), true, nil
}

func readLoopLogsSnapshot(path string, maxBytes int) (string, int64, os.FileInfo, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil, false, nil
	}
	if err != nil {
		return "", 0, nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", 0, nil, false, err
	}
	start := info.Size() - int64(maxBytes)
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", 0, nil, false, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, info.Size()-start))
	if err != nil {
		return "", 0, nil, false, err
	}
	if start > 0 {
		raw = trimIncompleteUTF8Prefix(raw)
	}
	return string(raw), info.Size(), info, true, nil
}

func trimIncompleteUTF8Prefix(raw []byte) []byte {
	for len(raw) > 0 && raw[0]&0xc0 == 0x80 {
		raw = raw[1:]
	}
	return raw
}

// utf8ChunkBoundary returns a complete-rune prefix no longer than maxBytes.
// It assumes the input starts at a rune boundary; callers that take a tail
// first discard leading continuation bytes with trimIncompleteUTF8Prefix.
func utf8ChunkBoundary(raw []byte, maxBytes int) int {
	if maxBytes <= 0 {
		return 0
	}
	if len(raw) < maxBytes {
		return len(raw)
	}
	start := maxBytes - 1
	for start > 0 && raw[start]&0xc0 == 0x80 {
		start--
	}
	if !utf8.FullRune(raw[start:maxBytes]) {
		return start
	}
	return maxBytes
}

func tailLogBytes(content string, maxBytes int) string {
	if len(content) <= maxBytes {
		return content
	}
	return string(trimIncompleteUTF8Prefix([]byte(content[len(content)-maxBytes:])))
}

func logContentAfterKnown(content, known string) string {
	if known == "" {
		return content
	}
	if content == known {
		return ""
	}
	if index := strings.LastIndex(content, known); index >= 0 {
		return content[index+len(known):]
	}
	return content
}

func (h *Handler) observeLoopLogsFollow(kind string, bytes int) {
	if h.loopLogsFollowObserve != nil {
		h.loopLogsFollowObserve(loopLogsFollowObservation{Kind: kind, Bytes: bytes})
	}
}
