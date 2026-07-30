package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	looperdruntime "github.com/nexu-io/looper/internal/runtime"
)

func TestHandlerStatusExposesDaemonGitHubIdentityAndCoreRate(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	recorder := httptest.NewRecorder()

	NewHandler(Context{
		Config:  cfg,
		Runtime: rt,
		GitHubHealth: func(context.Context) looperdruntime.GitHubHealth {
			return looperdruntime.GitHubHealth{
				Credential: looperdruntime.ForgeCredentialReadiness{GitHubProjects: true, Resolved: true},
				Hosts: []githubinfra.AuthHealth{{
					Hostname:          "github.com",
					Authenticated:     true,
					Login:             "MumuTW",
					CoreRateLimit:     5000,
					CoreRateRemaining: 4182,
					CheckedAt:         "2026-07-30T12:00:00Z",
				}},
			}
		},
	}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	github := data["github"].(map[string]any)
	host := github["hosts"].([]any)[0].(map[string]any)
	assertEqual(t, host["hostname"], "github.com")
	assertEqual(t, host["authenticated"], true)
	assertEqual(t, host["login"], "MumuTW")
	assertEqual(t, host["coreRateRemaining"], float64(4182))
}

func TestHandlerStatusUsesEmptyGitHubHostsWithoutHealthCallback(t *testing.T) {
	rt, cfg := startTestRuntime(t)
	recorder := httptest.NewRecorder()

	NewHandler(Context{Config: cfg, Runtime: rt}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := parseJSONMap(t, recorder.Body.Bytes())
	data := body["data"].(map[string]any)
	github := data["github"].(map[string]any)
	hosts, ok := github["hosts"].([]any)
	if !ok || len(hosts) != 0 {
		t.Fatalf("github hosts = %#v, want empty array", github["hosts"])
	}
}
