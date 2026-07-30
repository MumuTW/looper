package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	if !reflect.DeepEqual(got, version.Current()) {
		t.Fatalf("version json = %#v, want current identity %#v", got, version.Current())
	}
}

func TestVersionCheckDaemonReportsSameBuildAndFailsOnMismatch(t *testing.T) {
	current := completeVersionInfo()
	for _, test := range []struct {
		name       string
		detail     version.Info
		wantError  bool
		comparable bool
		wantMatch  bool
	}{
		{name: "same build", detail: current, comparable: true, wantMatch: true},
		{name: "different source commit", detail: withVersionCommit(current, "different"), wantError: true, comparable: true, wantMatch: false},
		{name: "incomplete identity", detail: version.Current(), wantError: true, comparable: false, wantMatch: false},
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
			err := runVersion(context.Background(), nil, []string{"--check-daemon", "--json"}, stdout, current)
			if (err != nil) != test.wantError {
				t.Fatalf("runVersion() error = %v, wantError %t", err, test.wantError)
			}
			var report versionCheckReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("decode comparison json: %v\n%s", err, stdout.String())
			}
			if report.Comparable != test.comparable || report.SameBuild != test.wantMatch || !reflect.DeepEqual(report.CLI, current) || !reflect.DeepEqual(report.Daemon, test.detail) {
				t.Fatalf("comparison report = %#v", report)
			}
		})
	}
}

func completeVersionInfo() version.Info {
	clean := false
	commit := "abc123"
	timestamp := "2026-07-31T00:00:00Z"
	return version.Info{Version: "1.2.3", Metadata: version.BuildMetadata{
		VersionSource: "git-tag:v1.2.3", Channel: "stable", APIVersion: "v1",
		GitCommitSHA: &commit, BuildTimestamp: &timestamp, Dirty: &clean,
	}}
}

func withVersionCommit(info version.Info, commit string) version.Info {
	value := commit
	info.Metadata.GitCommitSHA = &value
	return info
}
