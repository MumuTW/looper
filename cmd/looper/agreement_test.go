package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/labels"
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
				"projectId": "project_a",
				"repo":      "acme/looper", "prNumber": 42, "verdictEventId": "verdict_42",
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
	if !strings.Contains(stdout, "project_a  acme/looper#42  merged_as_is  agreement") || !strings.Contains(stdout, "verdict=verdict_42") {
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
	if !strings.Contains(stderr, "gatekeeper requires the agreements, verdicts, or promote subcommand") {
		t.Fatalf("stderr = %q, want usage error", stderr)
	}
}

func TestGatekeeperVerdictsCLIUsesProjectFilterAndPrintsReasons(t *testing.T) {
	var seenProjectID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/gatekeeper/verdicts" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s, want GET /api/v1/gatekeeper/verdicts", r.Method, r.URL.Path)
		}
		seenProjectID = r.URL.Query().Get("projectId")
		writeEnvelope(w, http.StatusOK, map[string]any{
			"items": []map[string]any{{
				"projectId": "project_a",
				"repo":      "acme/looper", "prNumber": 42, "status": "blocked", "eligible": false,
				"observedHeadSha": "head-42", "evaluatedAt": "2026-04-11T12:00:00Z",
				"reasons": []map[string]any{{"code": "hold", "subject": labels.HoldGlobal}},
			}},
		})
	}))
	defer server.Close()
	configForDaemon(t, server.URL)

	code, stdout, stderr := runCLI(t, "gatekeeper", "verdicts", "project_a")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if seenProjectID != "project_a" {
		t.Fatalf("projectId = %q, want project_a", seenProjectID)
	}
	if !strings.Contains(stdout, "project_a  acme/looper#42  blocked") || !strings.Contains(stdout, "head=head-42") || !strings.Contains(stdout, "reasons=hold("+labels.HoldGlobal+")") {
		t.Fatalf("stdout = %q, want formatted verdict evidence", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestGatekeeperPromoteCLIUpdatesProjectTrust(t *testing.T) {
	var seenProjectID string
	var seenTrust string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/project_a" || r.Method != http.MethodPatch {
			t.Fatalf("request = %s %s, want PATCH /api/v1/projects/project_a", r.Method, r.URL.Path)
		}
		seenProjectID = strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		seenTrust = body["gatekeeperTrust"]
		writeEnvelope(w, http.StatusOK, map[string]any{"id": "project_a", "gatekeeperTrust": "advise"})
	}))
	defer server.Close()
	configForDaemon(t, server.URL)

	code, stdout, stderr := runCLI(t, "gatekeeper", "promote", "project_a", "advise")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr %q), want 0", code, stderr)
	}
	if seenProjectID != "project_a" || seenTrust != "advise" {
		t.Fatalf("request = project %q trust %q, want project_a/advise", seenProjectID, seenTrust)
	}
	if stdout != "project project_a  gatekeeper-trust=advise\n" {
		t.Fatalf("stdout = %q, want promotion result", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestGatekeeperPromoteCLIRejectsObserveTarget(t *testing.T) {
	code, _, stderr := runCLI(t, "gatekeeper", "promote", "project_a", "observe")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "gatekeeper promote target must be advise or auto") {
		t.Fatalf("stderr = %q, want target validation", stderr)
	}
}
