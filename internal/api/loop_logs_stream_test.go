package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MumuTW/looper/internal/storage"
)

func TestCombinedLoopLogsStreamBoundsReadsAcrossSeveralLargeTabs(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	logRoot := filepath.Join(fixture.config.Daemon.LogDir, "loops", "loop_1", "run_1")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(log root): %v", err)
	}
	stdoutPath := filepath.Join(logRoot, "exec_large.stdout.log")
	stderrPath := filepath.Join(logRoot, "exec_large.stderr.log")
	largeHistory := strings.Repeat("history-line\n", 80_000)
	if err := os.WriteFile(stdoutPath, []byte(largeHistory), 0o644); err != nil {
		t.Fatalf("write stdout history: %v", err)
	}
	if err := os.WriteFile(stderrPath, []byte(largeHistory), 0o644); err != nil {
		t.Fatalf("write stderr history: %v", err)
	}

	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	output, err := json.Marshal(agentOutputPayload{
		Stdout:        "history tail",
		Stderr:        "history tail",
		StdoutLogPath: stdoutPath,
		StderrLogPath: stderrPath,
	})
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:             "exec_large",
		ProjectID:      stringPtr("project_1"),
		LoopID:         stringPtr("loop_1"),
		RunID:          stringPtr("run_1"),
		Vendor:         "codex",
		Status:         "running",
		PID:            int64Ptr(1234),
		StartedAt:      nowISO,
		OutputJSON:     stringPtr(string(output)),
		HeartbeatCount: 1,
		CreatedAt:      nowISO,
		UpdatedAt:      nowISO,
	}); err != nil {
		t.Fatalf("upsert execution: %v", err)
	}

	const tabs = 4
	stdoutAppend := strings.Repeat("o", loopLogsFollowMaxChunkBytes*2+17)
	stderrAppend := strings.Repeat("e", loopLogsFollowMaxChunkBytes+29)
	wantBytes := tabs * (len(stdoutAppend) + len(stderrAppend))

	var observationsMu sync.Mutex
	observations := make([]loopLogsFollowObservation, 0, 128)
	snapshotReady := make(chan struct{})
	bytesReady := make(chan struct{})
	snapshotCount, incrementalBytes := 0, 0
	snapshotsSignaled, bytesSignaled := false, false
	handler := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	handler.loopLogsFollowObserve = func(observation loopLogsFollowObservation) {
		observationsMu.Lock()
		observations = append(observations, observation)
		switch observation.Kind {
		case "snapshot_delivered":
			snapshotCount++
			if !snapshotsSignaled && snapshotCount == tabs {
				close(snapshotReady)
				snapshotsSignaled = true
			}
		case "file_read":
			incrementalBytes += observation.Bytes
			if !bytesSignaled && incrementalBytes == wantBytes {
				close(bytesReady)
				bytesSignaled = true
			}
		}
		observationsMu.Unlock()
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	responses := make([]*http.Response, 0, tabs)
	bodies := make([][]byte, tabs)
	var readers sync.WaitGroup
	readers.Add(tabs)
	for index := 0; index < tabs; index++ {
		response, getErr := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1&streams=both")
		if getErr != nil {
			t.Fatalf("open tab %d: %v", index, getErr)
		}
		responses = append(responses, response)
		go func(index int, response *http.Response) {
			defer readers.Done()
			defer response.Body.Close()
			bodies[index], _ = io.ReadAll(response.Body)
		}(index, response)
	}

	select {
	case <-snapshotReady:
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for every combined stream snapshot")
	}
	appendFile(t, stdoutPath, stdoutAppend)
	appendFile(t, stderrPath, stderrAppend)
	select {
	case <-bytesReady:
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for every combined stream to read appended bytes")
	}
	markRunSuccess(t, fixture, "run_1")
	done := make(chan struct{})
	go func() {
		readers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		for _, response := range responses {
			_ = response.Body.Close()
		}
		t.Fatal("timed out waiting for combined streams to terminate")
	}

	for index, body := range bodies {
		assertCombinedLogStreamBody(t, index, body, stdoutAppend, stderrAppend)
	}

	observationsMu.Lock()
	defer observationsMu.Unlock()
	stateRefreshes, fileReads, bytesRead := 0, 0, 0
	for _, observation := range observations {
		switch observation.Kind {
		case "state_refresh":
			stateRefreshes++
		case "file_read":
			fileReads++
			bytesRead += observation.Bytes
		}
	}
	// Snapshot delivery and final-byte observation are coordinated above, so each
	// follower needs its initial state plus at most two 1 Hz refreshes to project
	// the terminal run without relying on arbitrary sleep durations.
	maxStateRefreshes := tabs * 3
	if stateRefreshes > maxStateRefreshes {
		t.Fatalf("state refreshes = %d, want <= %d for %d tabs", stateRefreshes, maxStateRefreshes, tabs)
	}
	if fileReads > tabs*2*12 {
		t.Fatalf("file reads = %d, want <= %d for %d tabs", fileReads, tabs*2*12, tabs)
	}
	if bytesRead != wantBytes {
		t.Fatalf("incremental bytes read = %d, want %d", bytesRead, wantBytes)
	}
}

func TestCombinedLoopLogsStreamFallsBackToInlineOutput(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	missingStdoutPath := filepath.Join(fixture.config.Daemon.LogDir, "loops", "missing.stdout.log")
	missingStderrPath := filepath.Join(fixture.config.Daemon.LogDir, "loops", "missing.stderr.log")
	initialOutput, err := json.Marshal(agentOutputPayload{
		Stdout:        "first\n",
		StdoutLogPath: missingStdoutPath,
		StderrLogPath: missingStderrPath,
	})
	if err != nil {
		t.Fatalf("marshal initial output: %v", err)
	}
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:         "exec_inline",
		ProjectID:  stringPtr("project_1"),
		LoopID:     stringPtr("loop_1"),
		RunID:      stringPtr("run_1"),
		Vendor:     "codex",
		Status:     "running",
		StartedAt:  nowISO,
		OutputJSON: stringPtr(string(initialOutput)),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("upsert inline execution: %v", err)
	}

	server := httptest.NewServer(NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime}))
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1&streams=both")
	if err != nil {
		t.Fatalf("open combined stream: %v", err)
	}
	defer response.Body.Close()

	go func() {
		time.Sleep(250 * time.Millisecond)
		execution, _ := fixture.runtime.Services().Repositories.AgentExecutions.GetByID(context.Background(), "exec_inline")
		if execution != nil {
			nextOutput, marshalErr := json.Marshal(agentOutputPayload{
				Stdout:        "first\nsecond\n",
				Stderr:        "warning\n",
				StdoutLogPath: missingStdoutPath,
				StderrLogPath: missingStderrPath,
			})
			if marshalErr != nil {
				t.Errorf("marshal next output: %v", marshalErr)
				return
			}
			execution.OutputJSON = stringPtr(string(nextOutput))
			execution.UpdatedAt = fixture.now.Add(time.Minute).UTC().Format(javaScriptISOString)
			_ = fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), *execution)
		}
		markRunSuccess(t, fixture, "run_1")
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read combined stream: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, `"stream":"stdout"`) || !strings.Contains(text, `"content":"second\n"`) {
		t.Fatalf("stream body = %q, want inline stdout suffix", text)
	}
	if !strings.Contains(text, `"stream":"stderr"`) || !strings.Contains(text, `"content":"warning\n"`) {
		t.Fatalf("stream body = %q, want inline stderr suffix", text)
	}
	if !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want terminal event", text)
	}
}

func TestSingleLoopLogsStreamReadsOnlyAppendedFileBytes(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	logRoot := filepath.Join(fixture.config.Daemon.LogDir, "loops", "loop_1", "run_1")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(log root): %v", err)
	}
	stdoutPath := filepath.Join(logRoot, "exec_single.stdout.log")
	history := strings.Repeat("history-line\n", 64)
	if err := os.WriteFile(stdoutPath, []byte(history), 0o644); err != nil {
		t.Fatalf("write stdout history: %v", err)
	}

	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	output, err := json.Marshal(agentOutputPayload{Stdout: "history tail", StdoutLogPath: stdoutPath})
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:         "exec_single",
		ProjectID:  stringPtr("project_1"),
		LoopID:     stringPtr("loop_1"),
		RunID:      stringPtr("run_1"),
		Vendor:     "codex",
		Status:     "running",
		StartedAt:  nowISO,
		OutputJSON: stringPtr(string(output)),
		CreatedAt:  nowISO,
		UpdatedAt:  nowISO,
	}); err != nil {
		t.Fatalf("upsert execution: %v", err)
	}

	const appended = "new line\n"
	var observationsMu sync.Mutex
	incrementalBytes := 0
	snapshotReady := make(chan struct{})
	bytesReady := make(chan struct{})
	snapshotSignaled, bytesSignaled := false, false
	handler := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	handler.loopLogsFollowObserve = func(observation loopLogsFollowObservation) {
		observationsMu.Lock()
		defer observationsMu.Unlock()
		switch observation.Kind {
		case "snapshot_delivered":
			if !snapshotSignaled {
				close(snapshotReady)
				snapshotSignaled = true
			}
		case "file_read":
			incrementalBytes += observation.Bytes
			if !bytesSignaled && incrementalBytes >= len(appended) {
				close(bytesReady)
				bytesSignaled = true
			}
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1")
	if err != nil {
		t.Fatalf("open single stream: %v", err)
	}
	defer response.Body.Close()

	select {
	case <-snapshotReady:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for single-stream snapshot")
	}
	appendFile(t, stdoutPath, appended)
	select {
	case <-bytesReady:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for single-stream appended bytes")
	}
	markRunSuccess(t, fixture, "run_1")

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read single stream: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "event: snapshot") || !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want snapshot/end events", text)
	}
	if !strings.Contains(text, `"content":"new line\n"`) {
		t.Fatalf("stream body = %q, want appended chunk", text)
	}
	observationsMu.Lock()
	defer observationsMu.Unlock()
	if incrementalBytes != len(appended) {
		t.Fatalf("incremental file bytes = %d, want %d", incrementalBytes, len(appended))
	}
}

func TestLoopLogsFileCursorKeepsInlineFallbackWhenPathIsMissing(t *testing.T) {
	fixture := newTestFixture(t)
	handler := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	path := filepath.Join(fixture.config.Daemon.LogDir, "loops", "missing.stdout.log")
	cursor, err := handler.newLoopLogsFileCursor(path, "inline output\n")
	if err != nil {
		t.Fatalf("new cursor: %v", err)
	}
	if cursor.path != "" {
		t.Fatalf("cursor path = %q, want empty until a log file exists", cursor.path)
	}
	if got := cursor.snapshotContent(); got != "inline output\n" {
		t.Fatalf("snapshot content = %q, want inline output", got)
	}
}

func TestLogContentAfterKnownRequiresCumulativePrefix(t *testing.T) {
	if got := logContentAfterKnown("BOOK", "OK"); got != "BOOK" {
		t.Fatalf("gap = %q, want recreated-file content without arbitrary suffix match", got)
	}
	if got := logContentAfterKnown("OK next", "OK"); got != " next" {
		t.Fatalf("gap = %q, want cumulative suffix", got)
	}
}

func TestLoopLogsCursorDrainsOldExecutionBeforeSwitch(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.stdout.log")
	oldOutput := strings.Repeat("old ", loopLogsFollowMaxChunkBytes/2)
	if err := os.WriteFile(oldPath, []byte(oldOutput), 0o644); err != nil {
		t.Fatalf("write old output: %v", err)
	}
	previous := loopLogsResponse{Agent: &loopLogsAgentPayload{ExecutionID: "old_exec", Vendor: "codex", Status: "completed"}}
	next := loopLogsCombinedState{
		response: loopLogsResponse{Agent: &loopLogsAgentPayload{ExecutionID: "new_exec", Vendor: "codex", Status: "running"}},
		output:   agentOutputPayload{Stdout: "new output\n"},
	}
	cursor := loopLogsCombinedCursor{
		executionID: "old_exec",
		stdout:      loopLogsFileCursor{path: oldPath},
	}
	handler := NewHandler(Context{Config: newTestFixture(t).config})
	recorder := httptest.NewRecorder()
	if err := handler.updateLoopLogsCombinedCursor(recorder, recorder, previous, next, &cursor); err != nil {
		t.Fatalf("switch cursor: %v", err)
	}
	var oldJoined strings.Builder
	var newJoined strings.Builder
	for _, event := range strings.Split(recorder.Body.String(), "\n\n") {
		if !strings.HasPrefix(event, "event: chunk\n") {
			continue
		}
		data := strings.TrimPrefix(strings.SplitN(event, "\n", 2)[1], "data: ")
		var chunk loopLogsFollowChunkEvent
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		switch {
		case chunk.ExecutionID != nil && *chunk.ExecutionID == "old_exec":
			oldJoined.WriteString(chunk.Content)
		case chunk.ExecutionID != nil && *chunk.ExecutionID == "new_exec":
			newJoined.WriteString(chunk.Content)
		}
	}
	if oldJoined.String() != oldOutput {
		t.Fatalf("old execution bytes = %d, want %d", oldJoined.Len(), len(oldOutput))
	}
	if newJoined.String() != "new output\n" {
		t.Fatalf("new execution output = %q", newJoined.String())
	}
}

func TestLoopLogsFileCursorResetsForReplaceRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.log")
	if err := os.WriteFile(path, []byte("old log\n"), 0o644); err != nil {
		t.Fatalf("write old log: %v", err)
	}
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat old log: %v", err)
	}
	cursor := loopLogsFileCursor{path: path, offset: oldInfo.Size(), fileInfo: oldInfo}
	rotated := filepath.Join(dir, "stdout.log.1")
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rotate old log: %v", err)
	}
	const replacement = "replacement log that is longer than the old log\n"
	if err := os.WriteFile(path, []byte(replacement), 0o644); err != nil {
		t.Fatalf("write replacement log: %v", err)
	}
	chunk, attempted, err := cursor.readNext(loopLogsFollowMaxChunkBytes)
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if !attempted || chunk != replacement {
		t.Fatalf("rotation chunk = %q attempted=%t, want full replacement", chunk, attempted)
	}
}

func TestLoopLogsFileCursorPreservesUTF8AtReadAndSnapshotBoundaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.log")
	first := strings.Repeat("a", loopLogsFollowMaxChunkBytes-1) + "界" + "tail"
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	cursor := loopLogsFileCursor{path: path}
	var joined strings.Builder
	for {
		chunk, _, err := cursor.readNext(loopLogsFollowMaxChunkBytes)
		if err != nil {
			t.Fatalf("read chunk: %v", err)
		}
		if chunk == "" {
			break
		}
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk is not valid UTF-8: %q", chunk)
		}
		joined.WriteString(chunk)
	}
	if got := joined.String(); got != first {
		t.Fatalf("joined chunk = %q, want %q", got, first)
	}
	partialSource := append([]byte("partial "), 0xe7)
	if err := os.WriteFile(path, partialSource, 0o644); err != nil {
		t.Fatalf("write partial rune: %v", err)
	}
	partialCursor := loopLogsFileCursor{path: path}
	partial, _, err := partialCursor.readNext(loopLogsFollowMaxChunkBytes)
	if err != nil {
		t.Fatalf("read partial rune: %v", err)
	}
	if partial != "partial " || partialCursor.offset != int64(len("partial ")) {
		t.Fatalf("partial EOF = %q offset=%d, want retained rune bytes", partial, partialCursor.offset)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open partial rune append: %v", err)
	}
	if _, err := file.Write([]byte{0x95, 0x8c}); err != nil {
		_ = file.Close()
		t.Fatalf("complete partial rune: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close partial rune append: %v", err)
	}
	completed, _, err := partialCursor.readNext(loopLogsFollowMaxChunkBytes)
	if err != nil {
		t.Fatalf("read completed rune: %v", err)
	}
	if completed != "界" || !utf8.ValidString(completed) {
		t.Fatalf("completed rune = %q validUTF8=%t", completed, utf8.ValidString(completed))
	}

	snapshotPath := filepath.Join(dir, "snapshot.log")
	snapshotSource := strings.Repeat("a", 10) + "界" + strings.Repeat("b", loopLogsFollowSnapshotBytes-2)
	if err := os.WriteFile(snapshotPath, []byte(snapshotSource), 0o644); err != nil {
		t.Fatalf("write snapshot log: %v", err)
	}
	snapshot, _, _, found, err := readLoopLogsSnapshot(snapshotPath, loopLogsFollowSnapshotBytes)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !found || !utf8.ValidString(snapshot) {
		t.Fatalf("snapshot found=%t validUTF8=%t", found, utf8.ValidString(snapshot))
	}
	if strings.HasPrefix(snapshot, "\u008c") || !strings.HasPrefix(snapshot, "b") {
		t.Fatalf("snapshot begins %q, want complete tail bytes", snapshot[:min(4, len(snapshot))])
	}
	inlineTail := tailLogBytes(snapshotSource, loopLogsFollowSnapshotBytes)
	if !utf8.ValidString(inlineTail) || !strings.HasPrefix(inlineTail, "b") {
		t.Fatalf("inline tail validUTF8=%t starts %q, want complete tail bytes", utf8.ValidString(inlineTail), inlineTail[:min(4, len(inlineTail))])
	}
}

func TestWriteLoopLogsChunkSplitsLargeExecutionSnapshot(t *testing.T) {
	content := strings.Repeat("x", loopLogsFollowSnapshotBytes)
	recorder := httptest.NewRecorder()
	if err := writeLoopLogsChunk(recorder, recorder, loopLogsResponse{}, "stdout", content); err != nil {
		t.Fatalf("write chunks: %v", err)
	}
	var joined strings.Builder
	for _, event := range strings.Split(recorder.Body.String(), "\n\n") {
		if !strings.HasPrefix(event, "event: chunk\n") {
			continue
		}
		data := strings.TrimPrefix(strings.SplitN(event, "\n", 2)[1], "data: ")
		var chunk loopLogsFollowChunkEvent
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		if len(chunk.Content) > loopLogsFollowMaxChunkBytes {
			t.Fatalf("chunk bytes = %d, want <= %d", len(chunk.Content), loopLogsFollowMaxChunkBytes)
		}
		joined.WriteString(chunk.Content)
	}
	if got := joined.String(); got != content {
		t.Fatalf("joined content bytes = %d, want %d", len(got), len(content))
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Errorf("open append %s: %v", path, err)
		return
	}
	defer file.Close()
	if _, err := io.WriteString(file, content); err != nil {
		t.Errorf("append %s: %v", path, err)
	}
}

func markRunSuccess(t *testing.T, fixture testFixture, runID string) {
	t.Helper()
	run, err := fixture.runtime.Services().Repositories.Runs.GetByID(context.Background(), runID)
	if err != nil || run == nil {
		t.Errorf("get run %s: %v", runID, err)
		return
	}
	completedAt := fixture.now.Add(2 * time.Minute).UTC().Format(javaScriptISOString)
	run.Status = "success"
	run.EndedAt = &completedAt
	run.UpdatedAt = completedAt
	if err := fixture.runtime.Services().Repositories.Runs.Upsert(context.Background(), *run); err != nil {
		t.Errorf("complete run %s: %v", runID, err)
	}
}

func assertCombinedLogStreamBody(t *testing.T, tab int, body []byte, stdoutAppend, stderrAppend string) {
	t.Helper()
	if !strings.Contains(string(body), "event: snapshot") || !strings.Contains(string(body), "event: end") {
		t.Fatalf("tab %d stream missing snapshot/end events", tab)
	}
	firstEvent := strings.SplitN(string(body), "\n\n", 2)[0]
	snapshotData := strings.TrimPrefix(strings.SplitN(firstEvent, "\n", 2)[1], "data: ")
	var snapshot loopLogsResponse
	if err := json.Unmarshal([]byte(snapshotData), &snapshot); err != nil {
		t.Fatalf("tab %d decode snapshot: %v", tab, err)
	}
	if snapshot.Agent == nil {
		t.Fatalf("tab %d snapshot missing agent payload", tab)
	}
	if len(snapshot.Agent.Stdout) > loopLogsFollowSnapshotBytes || len(snapshot.Agent.Stderr) > loopLogsFollowSnapshotBytes {
		t.Fatalf(
			"tab %d snapshot bytes stdout=%d stderr=%d, want each <= %d",
			tab,
			len(snapshot.Agent.Stdout),
			len(snapshot.Agent.Stderr),
			loopLogsFollowSnapshotBytes,
		)
	}
	var stdout, stderr strings.Builder
	for _, event := range strings.Split(string(body), "\n\n") {
		if !strings.HasPrefix(event, "event: chunk\n") {
			continue
		}
		data := strings.TrimPrefix(strings.SplitN(event, "\n", 2)[1], "data: ")
		var chunk loopLogsFollowChunkEvent
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("tab %d decode chunk: %v", tab, err)
		}
		if len(chunk.Content) > loopLogsFollowMaxChunkBytes {
			t.Fatalf("tab %d chunk bytes = %d, want <= %d", tab, len(chunk.Content), loopLogsFollowMaxChunkBytes)
		}
		if chunk.Stream == "stderr" {
			stderr.WriteString(chunk.Content)
		} else {
			stdout.WriteString(chunk.Content)
		}
	}
	if stdout.String() != stdoutAppend {
		t.Fatalf("tab %d stdout bytes = %d, want %d", tab, stdout.Len(), len(stdoutAppend))
	}
	if stderr.String() != stderrAppend {
		t.Fatalf("tab %d stderr bytes = %d, want %d", tab, stderr.Len(), len(stderrAppend))
	}
}
