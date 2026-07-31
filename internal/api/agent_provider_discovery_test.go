package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MumuTW/looper/internal/agentdiscovery"
	"github.com/MumuTW/looper/internal/config"
)

func TestAgentProviderDiscoveryUsesCoherentConfigSnapshot(t *testing.T) {
	t.Parallel()
	stale, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	live := config.CloneConfig(stale)
	model := "explicit-model"
	live.Agent.Model = &model

	handler := NewHandler(Context{
		Config: stale,
		ConfigSnapshot: func() (config.Config, ConfigMetadata) {
			return live, ConfigMetadata{Revision: "sha256:live"}
		},
		DiscoverAgentProviders: func(_ context.Context, got config.Config) (agentdiscovery.Report, error) {
			if got.Agent.Model == nil || *got.Agent.Model != model {
				t.Fatalf("discovery config = %#v, want live snapshot", got.Agent)
			}
			return agentdiscovery.Report{Candidates: []agentdiscovery.Candidate{{Vendor: config.AgentVendorHermes, Status: "ready"}}}, nil
		},
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/agent/providers/discovery", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data agentdiscovery.Report `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.ConfigRevision != "sha256:live" || len(envelope.Data.Candidates) != 1 {
		t.Fatalf("report = %#v", envelope.Data)
	}
}

func TestAgentProviderDiscoveryIsReadOnlyAndUnavailableFailsClosed(t *testing.T) {
	t.Parallel()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Context{Config: cfg})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/api/v1/agent/providers/discovery", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		handler.ServeHTTP(recorder, request)
		want := http.StatusServiceUnavailable
		if method == http.MethodPost {
			want = http.StatusMethodNotAllowed
		}
		if recorder.Code != want {
			t.Fatalf("%s status = %d body=%s, want %d", method, recorder.Code, recorder.Body.String(), want)
		}
	}
}
