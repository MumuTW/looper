package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

// CQ-001 regression: with server.host on a wildcard address (e.g. 0.0.0.0) and
// authMode=none, request authorization used to trust the client-controlled Host
// header, so a remote client sending "Host: localhost" gained full API access.
// Locality must come from the transport peer (RemoteAddr), never from Host.

func wildcardAuthTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfigRouteConfig(t)
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.AuthMode = config.AuthModeNone
	return cfg
}

func TestWildcardBindRejectsRemoteWithSpoofedLoopbackHost(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.AuthMode = config.AuthModeNone
	handler := NewHandler(Context{Config: cfg, Runtime: rt})

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "read route", method: http.MethodGet, path: apiBasePath + "/version"},
		{name: "mutation route", method: http.MethodPost, path: apiBasePath + "/workers"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			req := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(`{}`))
			req.RemoteAddr = "203.0.113.10:54321"
			req.Host = "localhost"
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestWildcardBindRejectsForwardedRequestsWithoutToken(t *testing.T) {
	cfg := wildcardAuthTestConfig(t)
	handler := NewHandler(Context{Config: cfg})

	// A loopback peer with forwarding headers is a local reverse proxy, not the
	// original caller; without token auth it must not inherit loopback trust.
	req := httptest.NewRequest(http.MethodGet, apiBasePath+"/version", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWildcardBindAllowsDirectLoopbackClients(t *testing.T) {
	cfg := wildcardAuthTestConfig(t)
	handler := NewHandler(Context{Config: cfg})

	for _, remoteAddr := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		t.Run(remoteAddr, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, apiBasePath+"/version", nil)
			req.RemoteAddr = remoteAddr
			req.Host = "localhost"
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestWildcardBindKeepsTokenVerifiedCallbacksReachable(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.AuthMode = config.AuthModeNone
	handler := NewHandler(Context{Config: cfg, Runtime: rt})

	// Feishu's servers call in from remote addresses and authenticate with
	// their own verification token, so the url_verification handshake must not
	// be blocked by the loopback restriction.
	req := httptest.NewRequest(http.MethodPost, apiBasePath+"/hitl/feishu", strings.NewReader(`{"type":"url_verification","challenge":"abc123","token":"t"}`))
	req.RemoteAddr = "203.0.113.10:54321"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"challenge":"abc123"`) {
		t.Fatalf("challenge echo missing: %s", recorder.Body.String())
	}
}
