package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseForgejoSandboxConfig(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		_, enabled, err := parseForgejoSandboxConfig(func(string) string { return "" }, func() []string { return nil })
		if enabled || err != nil {
			t.Fatalf("enabled=%v err=%v, want disabled with nil error", enabled, err)
		}
	})

	t.Run("enabled config", func(t *testing.T) {
		env := map[string]string{
			envForgejoSandboxEnabled: "1",
			envForgejoBaseURL:        "https://code.example.test/",
			envForgejoSandboxRepo:    "acme/looper",
			envForgejoToken:          "secret-token",
		}
		cfg, enabled, err := parseForgejoSandboxConfig(func(key string) string { return env[key] }, func() []string { return []string{"PATH=/bin"} })
		if err != nil || !enabled {
			t.Fatalf("enabled=%v err=%v, want enabled config", enabled, err)
		}
		if cfg.BaseURL != "https://code.example.test" || cfg.Repo != "acme/looper" || cfg.Owner != "acme" || cfg.Name != "looper" {
			t.Fatalf("cfg = %#v", cfg)
		}
		if cfg.CloneURL != "https://looper-e2e:secret-token@code.example.test/acme/looper.git" {
			t.Fatalf("CloneURL = %q", cfg.CloneURL)
		}
	})

	for _, tc := range []struct {
		name     string
		override map[string]string
		wantErr  string
	}{
		{name: "missing base URL", override: map[string]string{envForgejoBaseURL: ""}, wantErr: envForgejoBaseURL},
		{name: "invalid base URL", override: map[string]string{envForgejoBaseURL: "://bad"}, wantErr: "absolute URL"},
		{name: "missing repo", override: map[string]string{envForgejoSandboxRepo: ""}, wantErr: envForgejoSandboxRepo},
		{name: "invalid repo", override: map[string]string{envForgejoSandboxRepo: "acme"}, wantErr: "owner/repo"},
		{name: "nested repo", override: map[string]string{envForgejoSandboxRepo: "acme/looper/extra"}, wantErr: "owner/repo"},
		{name: "missing token", override: map[string]string{envForgejoToken: ""}, wantErr: envForgejoToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{envForgejoSandboxEnabled: "1", envForgejoBaseURL: "https://code.example.test", envForgejoSandboxRepo: "acme/looper", envForgejoToken: "secret-token"}
			for key, value := range tc.override {
				env[key] = value
			}
			_, enabled, err := parseForgejoSandboxConfig(func(key string) string { return env[key] }, func() []string { return nil })
			if !enabled || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("enabled=%v err=%v, want error containing %q", enabled, err, tc.wantErr)
			}
		})
	}
}

func TestValidateForgejoSandboxPrerequisites(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/user":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":42,"login":"ralph"}`))
			case "/api/v1/repos/acme/looper":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"html_url":"https://forge.example.test/acme/looper"}`))
			case "/api/v1/repos/acme/looper/pulls":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		}))
		defer server.Close()

		cfg, err := validateForgejoSandboxPrerequisites(context.Background(), forgejoSandboxConfig{BaseURL: server.URL, Repo: "acme/looper", Token: "secret"})
		if err != nil {
			t.Fatalf("validateForgejoSandboxPrerequisites() error = %v", err)
		}
		if cfg.CurrentUser.Login != "ralph" || cfg.RepoHTMLURL != "https://forge.example.test/acme/looper" || cfg.Client == nil || cfg.HTTPClient == nil {
			t.Fatalf("validated cfg = %#v", cfg)
		}
	})

	t.Run("repo inaccessible fails fast", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/user":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":42,"login":"ralph"}`))
			case "/api/v1/repos/acme/looper":
				http.Error(w, "repo denied", http.StatusForbidden)
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		}))
		defer server.Close()

		_, err := validateForgejoSandboxPrerequisites(context.Background(), forgejoSandboxConfig{BaseURL: server.URL, Repo: "acme/looper", Token: "secret"})
		if err == nil || !strings.Contains(err.Error(), "sandbox repo lookup failed") {
			t.Fatalf("error = %v, want sandbox repo lookup failure", err)
		}
	})
}
