package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	looperapi "github.com/nexu-io/looper/internal/api"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/dashboard"
)

func TestDashboardCommandCompletesUnauthenticatedBrowserLogin(t *testing.T) {
	acceptedToken := "daemon-secret"
	server := httptest.NewUnstartedServer(nil)
	host, portText, found := strings.Cut(server.Listener.Addr().String(), ":")
	if !found {
		t.Fatalf("split listener address %q", server.Listener.Addr())
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	baseURL := "http://" + server.Listener.Addr().String()
	apiHandler := looperapi.NewHandler(looperapi.Context{Config: config.Config{
		Server: config.ServerConfig{
			Host:       host,
			Port:       port,
			BaseURL:    &baseURL,
			AuthMode:   config.AuthModeLocalToken,
			LocalToken: &acceptedToken,
		},
	}})
	root := looperapi.NewRootHandler(apiHandler, dashboard.Handler())
	var dashboardAuthorization string
	var dashboardAuthorizationMu sync.Mutex
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dashboard/" {
			dashboardAuthorizationMu.Lock()
			dashboardAuthorization = r.Header.Get("Authorization")
			dashboardAuthorizationMu.Unlock()
		}
		root.ServeHTTP(w, r)
	})
	server.Start()
	t.Cleanup(server.Close)

	// The file deliberately contains no secret. LOOPER_TOKEN supplies the
	// ordinary environment-precedence layer used by both CLI and daemon config.
	writeConfigFile(t, fmt.Sprintf(
		"[server]\nbaseUrl = %q\nauthMode = \"local-token\"\n",
		server.URL,
	))
	t.Setenv("LOOPER_TOKEN", acceptedToken)

	exitCode, stdout, stderr := runCLI(t, "dashboard")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", exitCode, stderr)
	}
	loginURL := strings.TrimSpace(stdout)
	if strings.Contains(loginURL, acceptedToken) {
		t.Fatal("dashboard URL leaked the long-lived token")
	}
	parsed, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parse dashboard URL: %v", err)
	}
	if parsed.Path != "/dashboard/" || parsed.Query().Get("code") == "" {
		t.Fatalf("dashboard URL = %q, want /dashboard/?code=<one-shot>", loginURL)
	}

	// Start like a fresh browser: no Authorization header and no session token.
	page, err := http.Get(loginURL)
	if err != nil {
		t.Fatalf("open dashboard URL: %v", err)
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", page.StatusCode)
	}
	dashboardAuthorizationMu.Lock()
	gotDashboardAuthorization := dashboardAuthorization
	dashboardAuthorizationMu.Unlock()
	if gotDashboardAuthorization != "" {
		t.Fatalf("unauthenticated dashboard Authorization = %q, want empty", gotDashboardAuthorization)
	}

	requestBody, err := json.Marshal(map[string]string{"code": parsed.Query().Get("code")})
	if err != nil {
		t.Fatalf("encode exchange: %v", err)
	}
	exchangeRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/dashboard/bootstrap/exchange", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create exchange request: %v", err)
	}
	exchangeRequest.Header.Set("Content-Type", "application/json")
	exchangeRequest.Header.Set("Origin", server.URL)
	exchange, err := http.DefaultClient.Do(exchangeRequest)
	if err != nil {
		t.Fatalf("exchange bootstrap code: %v", err)
	}
	defer exchange.Body.Close()
	if exchange.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(exchange.Body)
		t.Fatalf("exchange status = %d, want 200; body=%s", exchange.StatusCode, body)
	}
	var envelope struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(exchange.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode exchange: %v", err)
	}
	if envelope.Data.Token != acceptedToken {
		t.Fatalf("exchanged token = %q, want daemon token", envelope.Data.Token)
	}

	replay := exchangeBootstrapCodeRequest(t, server.URL, parsed.Query().Get("code"))
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		body, _ := io.ReadAll(replay.Body)
		t.Fatalf("replayed exchange status = %d, want 401; body=%s", replay.StatusCode, body)
	}
	if body, _ := io.ReadAll(replay.Body); bytes.Contains(body, []byte(acceptedToken)) {
		t.Fatalf("replayed exchange leaked daemon token: %s", body)
	}
}

func exchangeBootstrapCodeRequest(t *testing.T, serverURL, code string) *http.Response {
	t.Helper()
	requestBody, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatalf("encode exchange: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/v1/dashboard/bootstrap/exchange", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("create exchange request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", serverURL)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("exchange bootstrap code: %v", err)
	}
	return response
}

func TestDashboardCommandWithoutAuthPrintsDirectURL(t *testing.T) {
	configForDaemon(t, "http://127.0.0.1:19310")
	exitCode, stdout, stderr := runCLI(t, "dashboard")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", exitCode, stderr)
	}
	if got, want := stdout, "http://127.0.0.1:19310/dashboard/\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestDashboardCommandRejectsArguments(t *testing.T) {
	exitCode, _, stderr := runCLI(t, "dashboard", "extra")
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr, "dashboard takes no arguments") {
		t.Fatalf("stderr = %q, want arity error", stderr)
	}
}

func TestDashboardURLPreservesBasePathAndEscapesCode(t *testing.T) {
	if got, want := dashboardURL("https://ops.example/looper/", "a+b/c"), "https://ops.example/looper/dashboard/?code=a%2Bb%2Fc"; got != want {
		t.Fatalf("dashboardURL() = %q, want %q", got, want)
	}
}
