package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatekeeperAgreementsCLIUsesProjectFilterAndPrintsEvidence(t *testing.T) {
	var seenProjectID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/gatekeeper/agreements" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s, want GET /api/v1/gatekeeper/agreements", r.Method, r.URL.Path)
		}
		seenProjectID = r.URL.Query().Get("projectId")
		writeEnvelope(w, http.StatusOK, map[string]any{
			"items": []map[string]any{{
				"repo": "acme/looper", "prNumber": 42, "verdictEventId": "verdict_42",
				"outcome": "merged_as_is", "agreement": true, "recordedAt": "2026-04-11T12:00:00Z",
			}},
		})
	}))
	defer server.Close()
	configForDaemon(t, server.URL)

	code, stdout, stderr := runCLI(t, "gatekeeper", "agreements", "project_a")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if seenProjectID != "project_a" {
		t.Fatalf("projectId = %q, want project_a", seenProjectID)
	}
	if !strings.Contains(stdout, "acme/looper#42  merged_as_is  agreement") || !strings.Contains(stdout, "verdict=verdict_42") {
		t.Fatalf("stdout = %q, want formatted agreement evidence", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestGatekeeperAgreementsCLIRejectsUnknownSubcommand(t *testing.T) {
	code, _, stderr := runCLI(t, "gatekeeper", "merge")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "gatekeeper requires the agreements subcommand") {
		t.Fatalf("stderr = %q, want usage error", stderr)
	}
}
