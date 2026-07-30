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

	"github.com/nexu-io/looper/internal/storage"
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

	var observationsMu sync.Mutex
	observations := make([]loopLogsFollowObservation, 0, 128)
	handler := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})
	handler.loopLogsFollowObserve = func(observation loopLogsFollowObservation) {
		observationsMu.Lock()
		observations = append(observations, observation)
		observationsMu.Unlock()
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	const tabs = 4
	responses := make([]*http.Response, 0, tabs)
	for index := 0; index < tabs; index++ {
		response, getErr := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1&streams=both")
		if getErr != nil {
			t.Fatalf("open tab %d: %v", index, getErr)
		}
		responses = append(responses, response)
	}

	stdoutAppend := strings.Repeat("o", loopLogsFollowMaxChunkBytes*2+17)
	stderrAppend := strings.Repeat("e", loopLogsFollowMaxChunkBytes+29)
	go func() {
		time.Sleep(250 * time.Millisecond)
		appendFile(t, stdoutPath, stdoutAppend)
		appendFile(t, stderrPath, stderrAppend)
		time.Sleep(1100 * time.Millisecond)
		markRunSuccess(t, fixture, "run_1")
	}()

	bodies := make([][]byte, tabs)
	var readers sync.WaitGroup
	readers.Add(tabs)
	for index, response := range responses {
		index, response := index, response
		go func() {
			defer readers.Done()
			defer response.Body.Close()
			bodies[index], _ = io.ReadAll(response.Body)
		}()
	}
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
	if stateRefreshes > tabs*3 {
		t.Fatalf("state refreshes = %d, want <= %d for %d tabs", stateRefreshes, tabs*3, tabs)
	}
	if fileReads > tabs*2*12 {
		t.Fatalf("file reads = %d, want <= %d for %d tabs", fileReads, tabs*2*12, tabs)
	}
	wantBytes := tabs * (len(stdoutAppend) + len(stderrAppend))
	if bytesRead != wantBytes {
		t.Fatalf("incremental bytes read = %d, want %d", bytesRead, wantBytes)
	}
}

func TestCombinedLoopLogsStreamFallsBackToInlineOutput(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)
	nowISO := fixture.now.UTC().Format(javaScriptISOString)
	if err := fixture.runtime.Services().Repositories.AgentExecutions.Upsert(context.Background(), storage.AgentExecutionRecord{
		ID:         "exec_inline",
		ProjectID:  stringPtr("project_1"),
		LoopID:     stringPtr("loop_1"),
		RunID:      stringPtr("run_1"),
		Vendor:     "codex",
		Status:     "running",
		StartedAt:  nowISO,
		OutputJSON: stringPtr(`{"stdout":"first\\n","stderr":""}`),
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
			execution.OutputJSON = stringPtr(`{"stdout":"first\\nsecond\\n","stderr":"warning\\n"}`)
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
	if !strings.Contains(text, `"stream":"stdout"`) || !strings.Contains(text, `"content":"second\\n"`) {
		t.Fatalf("stream body = %q, want inline stdout suffix", text)
	}
	if !strings.Contains(text, `"stream":"stderr"`) || !strings.Contains(text, `"content":"warning\\n"`) {
		t.Fatalf("stream body = %q, want inline stderr suffix", text)
	}
	if !strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, want terminal event", text)
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
