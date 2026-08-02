package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	looperdruntime "github.com/MumuTW/looper/internal/runtime"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func TestWebUITriageRendersThroughTheAPIHandler(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/triage", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/html", got)
	}
	body := recorder.Body.String()
	for _, landmark := range []string{`id="triage-board"`, "Actionable now", "Machine working", "Stuck — needs decision"} {
		if !strings.Contains(body, landmark) {
			t.Fatalf("body is missing %q", landmark)
		}
	}
}

func TestWebUIMountRedirectsToTriage(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	for _, target := range []string{"/ui", "/ui/"} {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusFound {
			t.Fatalf("%s status = %d, want 302", target, recorder.Code)
		}
		if got := recorder.Header().Get("Location"); got != "/ui/triage" {
			t.Fatalf("%s location = %q, want /ui/triage", target, got)
		}
	}
}

// The point of mounting inside the API handler is that /ui/ inherits the JSON
// API's auth-mode handling rather than growing a second one.
func TestWebUIRequiresTheSameAuthorizationAsTheAPI(t *testing.T) {
	token := "webui-local-token"
	fixture := newTestFixture(t, func(options *looperdruntime.Options) {
		options.Config.Server.AuthMode = config.AuthModeLocalToken
		options.Config.Server.LocalToken = &token
	})
	h := NewHandler(Context{Config: fixture.config, Runtime: fixture.runtime})

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/triage", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", recorder.Code)
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	assertEqual(t, body["ok"], false)
	if errorPayload, ok := body["error"].(map[string]any); !ok || errorPayload["code"] != string(pkgapi.ErrorCodeUnauthorized) {
		t.Fatalf("error payload = %#v, want %s", body["error"], pkgapi.ErrorCodeUnauthorized)
	}

	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ui/triage", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200: %s", authorized.Code, authorized.Body.String())
	}
}

func TestWebUIStaticAssetsShipWithTheDaemon(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	h := NewHandler(Context{Config: cfg, Runtime: rt})

	for path, contentType := range map[string]string{
		"/ui/static/app.css":     "text/css; charset=utf-8",
		"/ui/static/htmx.min.js": "text/javascript; charset=utf-8",
	} {
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != contentType {
			t.Fatalf("%s content-type = %q, want %q", path, got, contentType)
		}
	}
}
