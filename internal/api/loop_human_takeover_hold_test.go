package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkgapi "github.com/nexu-io/looper/pkg/api"
)

// The API is the other way a human hold gets released. /takeover parks the loop
// and tells the operator no lane will claim it until handback — but /start,
// /pause and a direct /retry all rewrote the status through a blind upsert, and
// once the status is no longer human_takeover the claim boundary stops applying.
// These cover the promise end to end: refused through the API, and still held at
// the claim boundary afterwards.

// newHumanHeldLoopHandler seeds loop_1 in human_takeover with no active run or
// queue blocker, so any refusal below is the hold and not an unrelated
// precondition.
func newHumanHeldLoopHandler(t *testing.T) *Handler {
	t.Helper()
	fixture := newTestFixture(t)
	seedLoopRouteData(t, fixture.runtime)
	prepareLoopRouteForRetry(t, fixture.runtime, "human_takeover")
	return NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
	})
}

func postLoopRoute(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPost, path, reader)
	if body != "" {
		req.Header.Set("content-type", "application/json")
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	return recorder
}

// TestReactivationRoutesRefuseHumanHeldLoop is the API half of #162's hold:
// every route that would put the loop back in a claimable status must refuse
// while a human owns the worktree.
func TestReactivationRoutesRefuseHumanHeldLoop(t *testing.T) {
	for _, tt := range []struct{ name, path, body string }{
		{"start", "/api/v1/loops/loop_1/start", ""},
		{"pause", "/api/v1/loops/loop_1/pause", ""},
		{"retry", "/api/v1/loops/loop_1/retry", `{"mode":"auto"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHumanHeldLoopHandler(t)
			recorder := postLoopRoute(t, h, tt.path, tt.body)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
			}
			body := parseJSONMap(t, recorder.Body.Bytes())
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("body = %s, want an error object", recorder.Body.String())
			}
			assertEqual(t, errObj["code"], string(pkgapi.ErrorCodeValidationFailed))
			message, _ := errObj["message"].(string)
			if !contains(message, "human_takeover") || !contains(message, "handback") {
				t.Fatalf("message = %q, want it to name the hold and the release command", message)
			}

			// The loop is still held, and the claim boundary still applies: the
			// refusal is not cosmetic.
			services := h.context.Runtime.Services()
			loop, err := services.Repositories.Loops.GetByID(context.Background(), "loop_1")
			if err != nil || loop == nil {
				t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
			}
			if loop.Status != "human_takeover" {
				t.Fatalf("status = %q, want human_takeover preserved", loop.Status)
			}
			scheduled, err := services.Repositories.Queue.ListScheduled(context.Background(), "2026-04-11T12:05:00.000Z", 50)
			if err != nil {
				t.Fatalf("ListScheduled() error = %v", err)
			}
			if len(scheduled) != 0 {
				t.Fatalf("ListScheduled() = %#v, want nothing claimable for a held loop", scheduled)
			}
		})
	}
}

// TestHandbackReleasesHumanHoldAndRestoresClaims is the counterpart: handback is
// the sanctioned exit, and after it the loop's work is claimable again. Without
// it the refusals above would be a deadlock rather than a hold.
func TestHandbackReleasesHumanHoldAndRestoresClaims(t *testing.T) {
	h := newHumanHeldLoopHandler(t)

	recorder := postLoopRoute(t, h, "/api/v1/loops/loop_1/handback", `{"mode":"auto"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("handback status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}

	services := h.context.Runtime.Services()
	loop, err := services.Repositories.Loops.GetByID(context.Background(), "loop_1")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", loop, err)
	}
	if loop.Status != "queued" {
		t.Fatalf("status = %q, want queued after handback", loop.Status)
	}
	scheduled, err := services.Repositories.Queue.ListScheduled(context.Background(), "2026-04-11T12:05:00.000Z", 50)
	if err != nil {
		t.Fatalf("ListScheduled() error = %v", err)
	}
	if len(scheduled) == 0 {
		t.Fatalf("ListScheduled() = %#v, want the handed-back loop claimable again", scheduled)
	}

	// And /start works again once the hold is gone.
	startRecorder := postLoopRoute(t, h, "/api/v1/loops/loop_1/start", "")
	if startRecorder.Code != http.StatusOK {
		t.Fatalf("post-handback start status = %d, want 200; body=%s", startRecorder.Code, startRecorder.Body.String())
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// TestTakeoverMessageStatesTheThreeTiers: the response an operator reads has to
// separate what is enforced inside the writing statement from what is a
// check-then-act, because the incident behind this control was a control that
// reported more than it did.
func TestTakeoverMessageStatesTheThreeTiers(t *testing.T) {
	fixture := newTestFixture(t)
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		TakeoverLoop: func(_ context.Context, loopID, _ string) (TakeoverResult, error) {
			return TakeoverResult{LoopID: loopID, Vendor: "codex", SessionID: "session_1", WorktreePath: "/tmp/wt"}, nil
		},
	})

	resp, err := h.takeoverLoop(context.Background(), "loop_1")
	if err != nil {
		t.Fatalf("takeoverLoop() error = %v", err)
	}
	for _, want := range []string{"Guaranteed:", "Best-effort only:", "Not covered:", "looper handback", "#210"} {
		if !contains(resp.Message, want) {
			t.Fatalf("message = %q, want it to contain %q", resp.Message, want)
		}
	}
}

// The operator-visible half of the same thing: a stop failure must not be
// rendered as a plain failure while the loop sits parked. The response has to
// name the committed hold, the cancelled queue item, and the command that
// releases them.
func TestTakeoverStopFailureReportsCommittedHold(t *testing.T) {
	fixture := newTestFixture(t)
	h := NewHandler(Context{
		Config:  fixture.config,
		Runtime: fixture.runtime,
		TakeoverLoop: func(_ context.Context, loopID, _ string) (TakeoverResult, error) {
			return TakeoverResult{
					LoopID: loopID, Vendor: "codex", SessionID: "session_1", WorktreePath: "/tmp/wt",
					HoldCommitted: true,
				},
				errors.New("agent live containment handle is missing")
		},
	})

	_, err := h.takeoverLoop(context.Background(), "loop_1")
	if err == nil {
		t.Fatal("takeoverLoop() error = nil, want the stop failure surfaced")
	}
	var typed apiError
	if !asAPIError(err, &typed) {
		t.Fatalf("takeoverLoop() error = %v, want a structured apiError", err)
	}
	for _, want := range []string{"human_takeover", "queue item was cancelled", "looper handback loop_1", "containment handle is missing"} {
		if !contains(typed.message, want) {
			t.Fatalf("message = %q, want it to contain %q", typed.message, want)
		}
	}
	partial, ok := typed.details.(takeoverPartialFailure)
	if !ok {
		t.Fatalf("details = %#v, want takeoverPartialFailure", typed.details)
	}
	if !partial.HoldCommitted || !partial.QueueCancelled || partial.RecoveryCommand != "looper handback loop_1" {
		t.Fatalf("details = %#v, want the committed state and recovery command named", partial)
	}
}
