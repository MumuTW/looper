package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderDiscoveryPreviewAndConfirmedApplyUseSamePatch(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		wantPatches int
		wantOutput  string
	}{
		{name: "preview", args: []string{"provider", "discover"}, wantOutput: "not applied; review the suggestion"},
		{name: "apply without confirmation", args: []string{"provider", "discover", "--apply"}, wantOutput: "--confirm is required"},
		{name: "confirmed apply", args: []string{"provider", "discover", "--apply", "--confirm"}, wantPatches: 1, wantOutput: "applied the reviewed provider suggestion"},
	} {
		t.Run(test.name, func(t *testing.T) {
			patches := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/providers/discovery":
					writeEnvelope(w, http.StatusOK, map[string]any{
						"configRevision": "sha256:preview",
						"candidates":     []map[string]any{{"vendor": "hermes", "status": "ready", "executable": "/daemon/bin/hermes", "version": "Hermes Agent 1.2.3"}},
						"suggestion":     map[string]any{"set": map[string]any{"agent.vendor": "hermes"}},
					})
				case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/config":
					patches++
					var patch providerConfigPatch
					if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
						t.Fatalf("decode patch: %v", err)
					}
					if patch.Revision != "sha256:preview" || string(patch.Set["agent.vendor"]) != `"hermes"` || len(patch.Unset) != 0 {
						t.Fatalf("patch = %#v, want exact preview identity", patch)
					}
					writeEnvelope(w, http.StatusOK, map[string]any{"agent": map[string]any{"vendor": "hermes"}})
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			configPath := providerTestConfig(t, server.URL)
			args := append([]string{"--config", configPath}, test.args...)
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("run() code = %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
			}
			if patches != test.wantPatches {
				t.Fatalf("patches = %d, want %d", patches, test.wantPatches)
			}
			if !strings.Contains(stdout.String(), `suggestion: {"set":{"agent.vendor":"hermes"}}`) || !strings.Contains(stdout.String(), test.wantOutput) {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}

func TestProviderDiscoveryWithoutSuggestionNeverMutates(t *testing.T) {
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches++
			t.Fatal("unexpected mutation without suggestion")
		}
		writeEnvelope(w, http.StatusOK, map[string]any{
			"configRevision": "sha256:none",
			"candidates":     []map[string]any{{"vendor": "hermes", "status": "unavailable", "diagnostic": "restricted probe boundary is unavailable"}},
		})
	}))
	defer server.Close()
	configPath := providerTestConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", configPath, "provider", "discover", "--apply", "--confirm"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("run() code = %d stderr=%s", code, stderr.String())
	}
	if patches != 0 || !strings.Contains(stdout.String(), "suggestion: none") {
		t.Fatalf("patches=%d stdout=%q", patches, stdout.String())
	}
}

func TestProviderConfirmWithoutApplyIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"provider", "discover", "--confirm"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("run() code = %d stderr=%s", code, stderr.String())
	}
}

func providerTestConfig(t *testing.T, address string) string {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[server]\nhost = %q\nport = %s\n", parsed.Hostname(), parsed.Port())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
