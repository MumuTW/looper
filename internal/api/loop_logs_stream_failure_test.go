package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	pkgapi "github.com/MumuTW/looper/pkg/api"
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

// loop.logs.follow is the one contract entry that is declared rather than
// replayed, so regeneration cannot notice it drifting from the wire. Bind the
// declared error event to the struct the server actually writes: adding a field
// to loopLogsFollowErrorEvent fails here until the declaration in
// contract_artifact_regen_test.go and the artifact are updated with it.
func TestDeclaredLoopLogsFollowErrorEventMatchesWireStruct(t *testing.T) {
	artifactPath := filepath.Join("testdata", "contracts", "daemon-http.responses.compat.json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", artifactPath, err)
	}

	var artifact struct {
		Routes []struct {
			ID     string `json:"id"`
			Events map[string]struct {
				Event string          `json:"event"`
				Data  json.RawMessage `json:"data"`
			} `json:"events"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", artifactPath, err)
	}

	var declared json.RawMessage
	for _, route := range artifact.Routes {
		if route.ID != "loop.logs.follow" {
			continue
		}
		event, ok := route.Events["error"]
		if !ok {
			t.Fatal("loop.logs.follow declares no error event, but the stream emits one on poll, state, or log-read failure")
		}
		if event.Event != "error" {
			t.Fatalf("declared error event name = %q, want %q", event.Event, "error")
		}
		declared = event.Data
	}
	if declared == nil {
		t.Fatal("response artifact has no loop.logs.follow route")
	}

	var fields map[string]any
	if err := json.Unmarshal(declared, &fields); err != nil {
		t.Fatalf("decode declared error data: %v", err)
	}
	got := make([]string, 0, len(fields))
	for name := range fields {
		got = append(got, name)
	}
	sort.Strings(got)

	structType := reflect.TypeOf(loopLogsFollowErrorEvent{})
	want := make([]string, 0, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		want = append(want, strings.Split(structType.Field(i).Tag.Get("json"), ",")[0])
	}
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("declared error event fields = %v, want %v (regenerate: %s)", got, want, contractRegenerateCommand)
	}
}
