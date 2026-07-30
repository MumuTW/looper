package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

func TestLoopLogsFollowEmitsTypedErrorAndClosesAfterMidStreamStorageFailure(t *testing.T) {
	fixture := newTestFixture(t)
	seedRunRouteData(t, fixture.runtime)

	server := httptest.NewServer(NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
	}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/loops/loop_1/logs?follow=1")
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	// http.Get returns only after the snapshot headers/body have been flushed.
	// Closing the real coordinator now deterministically makes the next poll fail
	// after a successful snapshot, without adding a production test hook.
	if err := fixture.runtime.Services().Coordinator.Close(); err != nil {
		t.Fatalf("Coordinator.Close() error = %v", err)
	}

	bodyCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			errCh <- readErr
			return
		}
		bodyCh <- body
	}()

	var body []byte
	select {
	case body = <-bodyCh:
	case readErr := <-errCh:
		t.Fatalf("io.ReadAll() error = %v", readErr)
	case <-time.After(3 * time.Second):
		_ = response.Body.Close()
		t.Fatal("stream kept polling after persistent storage failure")
	}

	text := string(body)
	snapshotIndex := strings.Index(text, "event: snapshot")
	errorIndex := strings.Index(text, "event: error")
	if snapshotIndex < 0 || errorIndex <= snapshotIndex {
		t.Fatalf("stream body = %q, want snapshot followed by typed error", text)
	}
	if strings.Count(text, "event: error") != 1 {
		t.Fatalf("stream body = %q, want one terminal error event", text)
	}
	if strings.Contains(text, "event: end") {
		t.Fatalf("stream body = %q, storage failure must not masquerade as normal end", text)
	}

	data := sseEventData(t, text[errorIndex:])
	var event struct {
		Code         pkgapi.ErrorCode `json:"code"`
		Message      string           `json:"message"`
		Retryable    bool             `json:"retryable"`
		RetryAfterMS int64            `json:"retryAfterMs"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatalf("decode error event %q: %v", data, err)
	}
	if event.Code != pkgapi.ErrorCodeInternalError || !event.Retryable || event.RetryAfterMS != 1000 {
		t.Fatalf("error event = %#v, want retryable INTERNAL_ERROR with 1000ms floor", event)
	}
	if !strings.Contains(strings.ToLower(event.Message), "closed") {
		t.Fatalf("error message = %q, want storage failure cause", event.Message)
	}
}

func sseEventData(t *testing.T, eventText string) string {
	t.Helper()
	for _, line := range strings.Split(eventText, "\n") {
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("SSE event has no data line: %q", eventText)
	return ""
}
