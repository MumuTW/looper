package webui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/escalator"
	"github.com/MumuTW/looper/internal/gatekeeper"
	"github.com/MumuTW/looper/internal/storage"
)

func newTestHandler(load Loader) http.Handler {
	return Handler(Options{Load: load, Now: func() time.Time { return testNow }})
}

func get(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder
}

func assertContains(t *testing.T, body string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(body, needle) {
			t.Errorf("body is missing %q", needle)
		}
	}
}

func TestHandlerRendersTriagePageOnAnEmptyStore(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(func(context.Context) Input { return Input{Now: testNow} })
	recorder := get(t, handler, TriagePath)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", got)
	}
	if got := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") {
		t.Fatalf("content-security-policy = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", got)
	}

	body := recorder.Body.String()
	assertContains(t, body,
		"<title>Triage · looper</title>",
		`<link rel="stylesheet" href="/ui/static/app.css">`,
		`<script src="/ui/static/htmx.min.js" defer></script>`,
		`id="triage-board"`,
		`hx-get="/ui/triage/board"`,
		`hx-trigger="every 10s"`,
		`hx-swap="innerHTML transition:true"`,
		`class="wordmark"`,
		`<h1 class="page-title">Triage</h1>`,
		"Actionable now",
		"Machine working",
		"Stuck — needs decision",
		"last updated",
		"auto-refresh 10s",
		`id="triage-status"`,
		`role="status"`,
		`href="/ui/triage?refresh=off"`,
		"pause updates",
		"Nothing is waiting on you.",
		"Nothing has stopped moving.",
	)
	if strings.Contains(body, "<script>") {
		t.Fatalf("page must carry no inline script")
	}
}

func TestHandlerRendersRowsAndMeta(t *testing.T) {
	t.Parallel()

	record := snapshot(t, 42, payloadOptions{additions: 1234, deletions: 56})
	record.UnresolvedThreadCount = ptrInt64(4)

	handler := newTestHandler(func(context.Context) Input {
		return Input{
			Now:       testNow,
			Snapshots: []storage.PullRequestSnapshotRecord{record},
			Reports:   []gatekeeper.Report{report(42, false, gatekeeper.ReasonCheckFailed)},
			Links:     testLinker{},
		}
	})

	body := get(t, handler, TriagePath).Body.String()
	assertContains(t, body,
		`id="row-pr-proj-acme-widgets-42"`,
		`id="link-pr-proj-acme-widgets-42"`,
		`href="https://github.com/acme/widgets/pull/42"`,
		`>#42<`,
		"Change 42",
		`class="chip chip-danger"`,
		"CI failing",
		"threads",
		// html/template writes the leading plus as a numeric entity.
		"&#43;1.2k",
		`title="1234 added, 56 removed"`,
		`<span class="tile-count">1</span>`,
	)
}

func TestHandlerCanPauseAndResumePolling(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(nil)
	paused := get(t, handler, TriagePath+"?refresh=off").Body.String()
	if strings.Contains(paused, `hx-trigger="every`) {
		t.Fatal("paused page must not install the polling trigger")
	}
	assertContains(t, paused, "auto-refresh paused", `href="/ui/triage?refresh=on"`, "resume updates")

	resumed := get(t, handler, TriagePath+"?refresh=on").Body.String()
	assertContains(t, resumed, `hx-trigger="every 10s"`, "auto-refresh 10s", "pause updates")
}

func TestHandlerRendersNotices(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(func(context.Context) Input {
		return Input{Now: testNow, Notices: []string{"Escalator digest could not be collected."}}
	})

	body := get(t, handler, TriagePath).Body.String()
	assertContains(t, body, `class="notices"`, "Escalator digest could not be collected.")
}

func TestHandlerBoardFragmentIsSwappableOnItsOwn(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(func(context.Context) Input {
		return Input{Now: testNow, Escalator: escalator.Snapshot{Items: []escalator.Item{
			{ID: "hitl_question:proj:loop-3", Kind: escalator.KindWaiting, Reason: escalator.ReasonHITLQuestion, Stage: "worker", Title: "worker loop #3 is waiting on a human", Link: testLinker{}.Loop("proj", 3), AgeSeconds: 4},
		}}, Links: testLinker{}}
	})

	recorder := get(t, handler, boardPath)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "<body") {
		t.Fatalf("board fragment must not repeat the document chrome")
	}
	assertContains(t, body, `id="group-actionable"`, "answer needed", "loop #3", `class="row is-changed"`)
}

func TestHandlerRedirectsMountPointToTriage(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(nil)
	for _, target := range []string{BasePath, BasePath + "/"} {
		recorder := get(t, handler, target)
		if recorder.Code != http.StatusFound {
			t.Fatalf("%s status = %d, want 302", target, recorder.Code)
		}
		if got := recorder.Header().Get("Location"); got != TriagePath {
			t.Fatalf("%s location = %q, want %q", target, got, TriagePath)
		}
	}
}

func TestHandlerPreservesPublicBasePathPrefix(t *testing.T) {
	t.Parallel()

	handler := Handler(Options{Now: func() time.Time { return testNow }, PathPrefix: "/looper"})
	redirect := get(t, handler, BasePath+"?refresh=off")
	if got := redirect.Header().Get("Location"); got != "/looper/ui/triage?refresh=off" {
		t.Fatalf("prefixed redirect = %q, want /looper/ui/triage?refresh=off", got)
	}
	body := get(t, handler, TriagePath).Body.String()
	assertContains(t, body,
		`href="/looper/ui/static/app.css"`,
		`hx-get="/looper/ui/triage/board"`,
		`href="/looper/dashboard/"`,
	)
}

func TestHandlerServesVendoredStaticAssets(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(nil)

	script := get(t, handler, staticPrefix+"htmx.min.js")
	if script.Code != http.StatusOK {
		t.Fatalf("htmx status = %d, want 200", script.Code)
	}
	if got := script.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Fatalf("htmx content-type = %q", got)
	}
	if !strings.Contains(script.Body.String(), `version:"2.0.4"`) {
		t.Fatalf("vendored htmx must be a real htmx 2.x build")
	}

	stylesheet := get(t, handler, staticPrefix+"app.css")
	if stylesheet.Code != http.StatusOK {
		t.Fatalf("stylesheet status = %d, want 200", stylesheet.Code)
	}
	if got := stylesheet.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Fatalf("stylesheet content-type = %q", got)
	}

	if got := get(t, handler, staticPrefix+"missing.css").Code; got != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", got)
	}
}

func TestHandlerRejectsWritesAndUnknownPaths(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(nil)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, TriagePath, nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", recorder.Code)
	}
	if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("allow = %q", got)
	}

	if got := get(t, handler, BasePath+"/nope").Code; got != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", got)
	}
}

func TestOwnsMatchesOnlyTheUIMount(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"/ui":                 true,
		"/ui/triage":          true,
		"/ui/../ui/triage":    true,
		"/other/../ui/triage": true,
		"/ui/static/app.css":  true,
		"/uix":                false,
		"/api/v1/status":      false,
		"/dashboard/loops/12": false,
	}
	for path, want := range cases {
		if got := Owns(path); got != want {
			t.Errorf("Owns(%q) = %t, want %t", path, got, want)
		}
	}
}

func TestNormalizePathUsesOneRoutingRule(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"":                "/",
		"/ui///triage/":   "/ui/triage",
		"/ui/../triage":   "/triage",
		"/other/../ui":    "/ui",
		"/api/./v1//ping": "/api/v1/ping",
	}
	for input, want := range cases {
		if got := NormalizePath(input); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func ptrInt64(value int64) *int64 { return &value }

func TestHandlerRendersTheFoldedSummaryRow(t *testing.T) {
	t.Parallel()

	items := make([]escalator.Item, 0, 20)
	for index := 0; index < 20; index++ {
		items = append(items, escalator.Item{
			ID:         fmt.Sprintf("triage_confirmation:proj:acme/widgets:%d", index),
			Kind:       escalator.KindStuck,
			Reason:     escalator.ReasonTriageConfirmation,
			Stage:      "triager",
			Title:      fmt.Sprintf("Confirm triage for acme/widgets#%d", index),
			Link:       testLinker{}.Issue("proj", "acme/widgets", int64(index)),
			AgeSeconds: int64(3600 * (20 - index)),
		})
	}

	handler := newTestHandler(func(context.Context) Input {
		return Input{Now: testNow, Links: testLinker{}, Escalator: escalator.Snapshot{Items: items}}
	})

	body := get(t, handler, TriagePath).Body.String()
	assertContains(t, body,
		`class="row row-summary"`,
		`id="row-more-stuck-triage-confirmation"`,
		`class="row-summary-text"`,
		"and 17 more issues awaiting triage confirmation",
		// The tile still counts every stuck item, not every drawn line.
		`<span class="tile-count">20</span>`,
	)
	if strings.Count(body, `class="row row-summary"`) != 1 {
		t.Fatalf("summary rows rendered = %d, want 1", strings.Count(body, `class="row row-summary"`))
	}
	if strings.Count(body, `id="row-item-`) != 3 {
		t.Fatalf("exemplar rows rendered = %d, want 3", strings.Count(body, `id="row-item-`))
	}
}
