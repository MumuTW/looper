package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

// TestSplitGlobalFlags covers the bug that made --config/--host/--port
// documented but unreachable: routing ran on the raw argv, so a flag before the
// verb was an unknown command and a flag after it was an arity error.
func TestSplitGlobalFlags(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantVerb     string
		wantOperands []string
		wantGlobal   []string
		wantErr      string
	}{
		{
			name:         "flags before the verb",
			args:         []string{"--config", "/etc/looper.toml", "stop", "12"},
			wantVerb:     "stop",
			wantOperands: []string{"12"},
			wantGlobal:   []string{"--config", "/etc/looper.toml"},
		},
		{
			name:         "flags after the operands",
			args:         []string{"stop", "12", "--config", "/etc/looper.toml"},
			wantVerb:     "stop",
			wantOperands: []string{"12"},
			wantGlobal:   []string{"--config", "/etc/looper.toml"},
		},
		{
			name:         "flags interleaved with the operands",
			args:         []string{"respond", "12", "--host", "looper.internal", "ship it"},
			wantVerb:     "respond",
			wantOperands: []string{"12", "ship it"},
			wantGlobal:   []string{"--host", "looper.internal"},
		},
		{
			name:         "inline values",
			args:         []string{"--port=9000", "stop", "12"},
			wantVerb:     "stop",
			wantOperands: []string{"12"},
			wantGlobal:   []string{"--port=9000"},
		},
		{
			name:         "several global flags",
			args:         []string{"--host", "127.0.0.1", "stop", "12", "--port", "9000"},
			wantVerb:     "stop",
			wantOperands: []string{"12"},
			wantGlobal:   []string{"--host", "127.0.0.1", "--port", "9000"},
		},
		{
			// Without this, an answer that opens with a dash is unanswerable.
			name:         "a double dash ends flag parsing",
			args:         []string{"respond", "12", "--", "--not-a-flag"},
			wantVerb:     "respond",
			wantOperands: []string{"12", "--not-a-flag"},
		},
		{
			// A swallowed flag would be read as the selector and stop the wrong
			// loop, so an unknown one is refused rather than passed through.
			name:    "an unknown flag is refused rather than treated as a selector",
			args:    []string{"stop", "--force", "12"},
			wantErr: `unknown flag "--force"`,
		},
		{
			name:    "a global flag with no value fails before the request",
			args:    []string{"stop", "12", "--config"},
			wantErr: "missing value for --config",
		},
		{
			name:    "a global flag whose value is another flag fails",
			args:    []string{"--config", "--host", "stop", "12"},
			wantErr: "missing value for --config",
		},
		{
			name:    "flags alone are not a command",
			args:    []string{"--config", "/etc/looper.toml"},
			wantErr: "a command is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitGlobalFlags(tc.args)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Verb != tc.wantVerb {
				t.Errorf("verb = %q, want %q", got.Verb, tc.wantVerb)
			}
			if !reflect.DeepEqual(got.Operands, tc.wantOperands) {
				t.Errorf("operands = %#v, want %#v", got.Operands, tc.wantOperands)
			}
			if !reflect.DeepEqual(got.Global, tc.wantGlobal) {
				t.Errorf("global = %#v, want %#v", got.Global, tc.wantGlobal)
			}
		})
	}
}

// TestSplitGlobalFlagsFeedsConfigLoading closes the loop the unit table cannot:
// the flags this splitter hands to config.LoadFile must actually change where
// the CLI dials, in both argument orders.
func TestSplitGlobalFlagsFeedsConfigLoading(t *testing.T) {
	for _, args := range [][]string{
		{"--host", "looper.internal", "--port", "9443", "stop", "12"},
		{"stop", "12", "--host", "looper.internal", "--port", "9443"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// No config file, so the flags are the only thing that can move the
			// address off the built-in default.
			t.Setenv("HOME", t.TempDir())
			parsed, err := splitGlobalFlags(args)
			if err != nil {
				t.Fatalf("splitGlobalFlags() error = %v", err)
			}
			cfg, err := loadConfig(parsed.Global)
			if err != nil {
				t.Fatalf("loadConfig() error = %v", err)
			}
			if got := daemonBaseURL(cfg); got != "http://looper.internal:9443" {
				t.Fatalf("daemonBaseURL = %q, want http://looper.internal:9443", got)
			}
		})
	}
}

// TestDaemonBaseURL pins the address the CLI dials. The bind host is not the
// address a daemon is reachable at once anything fronts it, so server.baseUrl
// wins when set; a wildcard bind is not dialable at all.
func TestDaemonBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		server  config.ServerConfig
		wantURL string
	}{
		{
			name:    "baseUrl wins over the bind address",
			server:  config.ServerConfig{Host: "0.0.0.0", Port: 8080, BaseURL: stringPtr("https://looper.example.com")},
			wantURL: "https://looper.example.com",
		},
		{
			name:    "baseUrl keeps its path prefix but loses its trailing slash",
			server:  config.ServerConfig{Host: "127.0.0.1", Port: 8080, BaseURL: stringPtr("https://ops.example.com/looper/")},
			wantURL: "https://ops.example.com/looper",
		},
		{
			name:    "a blank baseUrl falls back to the bind address",
			server:  config.ServerConfig{Host: "127.0.0.1", Port: 8080, BaseURL: stringPtr("   ")},
			wantURL: "http://127.0.0.1:8080",
		},
		{
			name:    "an IPv4 wildcard bind maps to loopback",
			server:  config.ServerConfig{Host: "0.0.0.0", Port: 8080},
			wantURL: "http://127.0.0.1:8080",
		},
		{
			name:    "an IPv6 wildcard bind maps to loopback",
			server:  config.ServerConfig{Host: "::", Port: 8080},
			wantURL: "http://127.0.0.1:8080",
		},
		{
			name:    "an IPv6 literal host is bracketed so it stays dialable",
			server:  config.ServerConfig{Host: "::1", Port: 8080},
			wantURL: "http://[::1]:8080",
		},
		{
			name:    "an unset host means loopback",
			server:  config.ServerConfig{Port: 8080},
			wantURL: "http://127.0.0.1:8080",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonBaseURL(config.Config{Server: tc.server}); got != tc.wantURL {
				t.Fatalf("daemonBaseURL = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

func TestApplyEndpointOverridesPreservesUnchangedBaseURLComponents(t *testing.T) {
	tests := []struct {
		name   string
		global []string
		host   string
		port   int
		want   string
	}{
		{name: "port keeps scheme host and path", global: []string{"--port", "18443"}, host: "127.0.0.1", port: 18443, want: "https://daemon.example:18443/looper"},
		{name: "host keeps scheme port and path", global: []string{"--host", "other.example"}, host: "other.example", port: 17310, want: "https://other.example:9443/looper"},
		{name: "both replace host and port", global: []string{"--host", "other.example", "--port", "18443"}, host: "other.example", port: 18443, want: "https://other.example:18443/looper"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			baseURL := "https://daemon.example:9443/looper"
			cfg := config.Config{Server: config.ServerConfig{Host: testCase.host, Port: testCase.port, BaseURL: &baseURL}}

			if got := daemonBaseURL(applyEndpointOverrides(cfg, testCase.global)); got != testCase.want {
				t.Fatalf("daemonBaseURL = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestPostAuthorizationHeader pins when the local token is sent. A token set in
// config but not selected by authMode is not a credential this daemon asked
// for, and attaching it anyway hands it to whatever answers the address.
func TestPostAuthorizationHeader(t *testing.T) {
	tests := []struct {
		name     string
		server   config.ServerConfig
		wantAuth string
	}{
		{
			name:     "local-token auth sends the token",
			server:   config.ServerConfig{AuthMode: config.AuthModeLocalToken, LocalToken: stringPtr("s3cret")},
			wantAuth: "Bearer s3cret",
		},
		{
			name:   "a token left in config is not sent under authMode none",
			server: config.ServerConfig{AuthMode: config.AuthModeNone, LocalToken: stringPtr("s3cret")},
		},
		{
			name:   "an empty token sends no header",
			server: config.ServerConfig{AuthMode: config.AuthModeLocalToken, LocalToken: stringPtr("  ")},
		},
		{
			name:   "a missing token sends no header",
			server: config.ServerConfig{AuthMode: config.AuthModeLocalToken},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()

			cfg := config.Config{Server: tc.server}
			cfg.Server.BaseURL = &server.URL
			if _, err := post(context.Background(), cfg, apiRequest{Path: "/api/v1/loops/12/stop"}); err != nil {
				t.Fatalf("post() error = %v", err)
			}
			if gotAuth != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tc.wantAuth)
			}
		})
	}
}

// TestPostDialsTheBaseURL proves daemonBaseURL is what the request actually
// goes to, path included — the previous client built its own host:port and
// ignored server.baseUrl entirely.
func TestPostDialsTheBaseURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	// A wildcard bind that would be undialable if the client preferred it.
	cfg := config.Config{Server: config.ServerConfig{Host: "0.0.0.0", Port: 1, BaseURL: &server.URL}}
	if _, err := post(context.Background(), cfg, apiRequest{Path: "/api/v1/loops/12/takeover"}); err != nil {
		t.Fatalf("post() error = %v", err)
	}
	if gotPath != "/api/v1/loops/12/takeover" {
		t.Fatalf("path = %q, want /api/v1/loops/12/takeover", gotPath)
	}
}

func stringPtr(value string) *string { return &value }
