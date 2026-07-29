package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStopAllResultCountsFromEnvelope(t *testing.T) {
	payload := `{"ok":true,"data":{"summary":{"total":2,"stopped":1,"pausedOnly":1,"failed":0},"items":[]}}`
	failed, pausedOnly, err := stopAllResultCounts([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if failed != 0 || pausedOnly != 1 {
		t.Fatalf("failed=%d pausedOnly=%d, want 0 and 1", failed, pausedOnly)
	}
}

func TestStopAllPartialFailure(t *testing.T) {
	if err := stopAllPartialFailure(`{"ok":true,"data":{"summary":{"failed":2,"pausedOnly":0}}}`); err == nil || !strings.Contains(err.Error(), "failed to stop 2") {
		t.Fatalf("error = %v, want failed-to-stop message", err)
	}
	if err := stopAllPartialFailure(`{"ok":true,"data":{"summary":{"failed":0,"pausedOnly":3}}}`); err == nil || !strings.Contains(err.Error(), "paused 3") {
		t.Fatalf("error = %v, want paused-only message", err)
	}
	if err := stopAllPartialFailure(`{"ok":true,"data":{"summary":{"failed":0,"pausedOnly":0}}}`); err != nil {
		t.Fatalf("error = %v, want nil for full success", err)
	}
}

func TestClassifyRetryWorktree(t *testing.T) {
	trueVal := true
	falseVal := false
	if got := classifyRetryWorktree(loopWorktreeStatus{Present: true, Managed: true, Dirty: &trueVal}); got != retryWorktreeOfferDiscard {
		t.Fatalf("dirty managed = %v, want offer discard", got)
	}
	if got := classifyRetryWorktree(loopWorktreeStatus{Present: true, Managed: false, Dirty: &trueVal}); got != retryWorktreeInspectOnly {
		t.Fatalf("dirty unmanaged = %v, want inspect only", got)
	}
	if got := classifyRetryWorktree(loopWorktreeStatus{Present: true, Managed: true, Dirty: &falseVal}); got != retryWorktreeOK {
		t.Fatalf("clean managed = %v, want ok", got)
	}
	if got := classifyRetryWorktree(loopWorktreeStatus{Present: false}); got != retryWorktreeOK {
		t.Fatalf("missing worktree = %v, want ok", got)
	}
}

func TestSplitRetryArgs(t *testing.T) {
	parsed, err := splitRetryArgs([]string{"retry", "12", "--discard-worktree-changes", "--confirm"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Retry.Selector != "12" || !parsed.Retry.DiscardWorktreeChanges || !parsed.Retry.Confirm {
		t.Fatalf("opts = %+v", parsed.Retry)
	}
	parsed, err = splitRetryArgs([]string{"--config", "/tmp/c.toml", "retry", "12"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(parsed.Global, " "); got != "--config /tmp/c.toml" {
		t.Fatalf("global = %q", got)
	}
	if _, err := splitRetryArgs([]string{"retry", "12", "--nope"}); err == nil {
		t.Fatal("want unknown flag error")
	}
}

func TestRunRetryRefusesDirtyWithoutDiscard(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/worktree"):
			_, _ = w.Write([]byte(`{"ok":true,"data":{"loopId":"loop_1","seq":12,"present":true,"managed":true,"dirty":true,"clean":false,"worktreePath":"/tmp/wt"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/retry"):
			posts++
			_, _ = w.Write([]byte(`{"ok":true,"data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	host, port := splitHostPort(server.URL)
	stderr := &strings.Builder{}
	code := run([]string{"--host", host, "--port", port, "retry", "12"}, strings.NewReader(""), io.Discard, stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if posts != 0 {
		t.Fatalf("retry POSTed %d times, want 0", posts)
	}
	if !strings.Contains(stderr.String(), "--discard-worktree-changes --confirm") {
		t.Fatalf("stderr = %q, want discard guidance", stderr.String())
	}
}

func TestRunRetryDiscardsWhenConfirmed(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/retry") {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"data":{"discardWorktreeChanges":true}}`))
	}))
	defer server.Close()

	host, port := splitHostPort(server.URL)
	code := run([]string{"--host", host, "--port", port, "retry", "12", "--discard-worktree-changes", "--confirm"}, strings.NewReader(""), io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if body["discardWorktreeChanges"] != true {
		t.Fatalf("body = %#v, want discardWorktreeChanges true", body)
	}
}

// writeAPIError emits the envelope a real daemon sends, so these tests exercise
// the code the CLI branches on rather than Go's bare "404 page not found" body.
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"ok":false,"error":{"code":%q,"message":%q}}`, code, message)
}

func TestRunRetrySkipsPreflightWhenRouteMissing(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/worktree") {
			writeAPIError(w, http.StatusNotFound, "ROUTE_NOT_FOUND", "Unknown route")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/retry") {
			posts++
			_, _ = w.Write([]byte(`{"ok":true,"data":{}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	host, port := splitHostPort(server.URL)
	code := run([]string{"--host", host, "--port", port, "retry", "12"}, strings.NewReader(""), io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 on missing worktree route fallback", code)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1", posts)
	}
}

// A 404 that is not ROUTE_NOT_FOUND means the route exists and answered. The
// gate must hold: /worktree returns PROJECT_NOT_FOUND when the loop's project
// row is archived or missing, and skipping the preflight there would requeue a
// dirty worktree — the exact regression the preflight exists to prevent.
func TestRunRetryKeepsPreflightOnNonRouteNotFound(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		status  int
		code    string
		message string
	}{
		{"archived project", http.StatusNotFound, "PROJECT_NOT_FOUND", "Project not found: acme"},
		{"stat failure naming pr 404", http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to stat worktree at /w/looper-fix-acme-pr-404: permission denied"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var posts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/worktree") {
					writeAPIError(w, testCase.status, testCase.code, testCase.message)
					return
				}
				posts++
				_, _ = w.Write([]byte(`{"ok":true,"data":{}}`))
			}))
			defer server.Close()

			host, port := splitHostPort(server.URL)
			code := run([]string{"--host", host, "--port", port, "retry", "12"}, strings.NewReader(""), io.Discard, io.Discard)
			if code == 0 {
				t.Fatalf("exit = 0, want non-zero: the preflight was skipped on a %s", testCase.code)
			}
			if posts != 0 {
				t.Fatalf("posts = %d, want 0: retry was requeued without a worktree check", posts)
			}
		})
	}
}

func TestRunStopAllExitsNonZeroWhenPausedOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"data":{"summary":{"total":1,"stopped":0,"pausedOnly":1,"failed":0,"alreadyFinished":0,"alreadyStopping":0},"items":[]}}`))
	}))
	defer server.Close()

	host, port := splitHostPort(server.URL)
	stderr := &strings.Builder{}
	code := run([]string{"--host", host, "--port", port, "stop", "all"}, strings.NewReader(""), io.Discard, stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "paused") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func splitHostPort(rawURL string) (host, port string) {
	trimmed := strings.TrimPrefix(rawURL, "http://")
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		return "127.0.0.1", "80"
	}
	return parts[0], parts[1]
}
