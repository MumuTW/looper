package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MumuTW/looper/internal/version"
)

func TestVersionJSONPrintsCompleteLocalBuildIdentity(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := run([]string{"version", "--json"}, nil, stdout, stderr); code != 0 {
		t.Fatalf("looper version --json exit code = %d, stderr = %q", code, stderr.String())
	}
	var got version.Info
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode version json: %v\n%s", err, stdout.String())
	}
	if !got.SameBuild(version.Current()) {
		t.Fatalf("version json = %#v, want current identity %#v", got, version.Current())
	}
}

func TestVersionCheckDaemonReportsSameBuildAndFailsOnMismatch(t *testing.T) {
	current := version.Current()
	for _, test := range []struct {
		name      string
		detail    version.Info
		wantCode  int
		wantMatch bool
	}{
		{name: "same build", detail: current, wantCode: 0, wantMatch: true},
		{name: "different source commit", detail: withVersionCommit(current, "different"), wantCode: 1, wantMatch: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/version" {
					t.Fatalf("request = %s %s, want GET /api/v1/version", r.Method, r.URL.Path)
				}
				writeEnvelope(w, http.StatusOK, daemonVersionResponse{Version: test.detail.Version, Build: test.detail.Metadata})
			}))
			t.Cleanup(server.Close)
			configForDaemon(t, server.URL)

			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			code := run([]string{"version", "--check-daemon", "--json"}, nil, stdout, stderr)
			if code != test.wantCode {
				t.Fatalf("looper version --check-daemon --json exit code = %d, stderr = %q, want %d", code, stderr.String(), test.wantCode)
			}
			var report versionCheckReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("decode comparison json: %v\n%s", err, stdout.String())
			}
			if report.SameBuild != test.wantMatch || !report.CLI.SameBuild(current) || !report.Daemon.SameBuild(test.detail) {
				t.Fatalf("comparison report = %#v", report)
			}
		})
	}
}

func withVersionCommit(info version.Info, commit string) version.Info {
	value := commit
	info.Metadata.GitCommitSHA = &value
	return info
}
