package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/shell"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/cloud"
	"github.com/nexu-io/looper/internal/network/protocol"
)

func TestNetworkJoinCreatesDurableStateAfterRemoteEnrollment(t *testing.T) {
	t.Parallel()
	var joined protocol.JoinRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/join" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&joined); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(protocol.JoinResponse{NetworkID: "net_1", NodeID: "node_1", NodeToken: "node-secret-token"})
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "network.json")
	configPath := writeNetworkJoinConfig(t)
	var output bytes.Buffer
	err := runNetworkJoin(context.Background(), []string{"--config", configPath}, statePath, []string{server.URL, "--key", "join-secret-key", "--name", "worker-1"}, &output)
	if err != nil {
		t.Fatalf("runNetworkJoin() error = %v", err)
	}
	if joined.JoinKey != "join-secret-key" || joined.NodeName != "worker-1" || joined.GitHub.Login != "octocat" || joined.GitHub.NumericID != 42 {
		t.Fatalf("join request = %#v", joined)
	}
	state, err := networkclient.LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.NetworkID != "net_1" || state.NodeID != "node_1" || state.NodeToken != "node-secret-token" || state.GitHub.Login != "octocat" {
		t.Fatalf("saved state = %#v", state)
	}
	if strings.Contains(output.String(), "join-secret-key") || strings.Contains(output.String(), "node-secret-token") {
		t.Fatalf("join output leaked a secret: %q", output.String())
	}
}

func TestNetworkLeaveRetainsStateWhenRemoteRevocationFails(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token rejected", http.StatusUnauthorized)
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "network.json")
	if err := networkclient.SaveState(statePath, networkclient.LocalState{URL: server.URL, NetworkID: "net_1", NodeID: "node_1", NodeName: "worker-1", NodeToken: "node-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := runNetworkLeave(context.Background(), statePath, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "leave Network") {
		t.Fatalf("runNetworkLeave() error = %v", err)
	}
	if _, err := networkclient.LoadState(statePath); err != nil {
		t.Fatalf("failed remote leave removed local state: %v", err)
	}
}

func TestNetworkLeaveRemovesStateAfterRemoteRevocation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leave" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer node-secret" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "network.json")
	if err := networkclient.SaveState(statePath, networkclient.LocalState{URL: server.URL, NetworkID: "net_1", NodeID: "node_1", NodeName: "worker-1", NodeToken: "node-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := runNetworkLeave(context.Background(), statePath, &bytes.Buffer{}); err != nil {
		t.Fatalf("runNetworkLeave() error = %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("successful remote leave retained state, stat error = %v", err)
	}
}

func TestNetworkJoinSurvivesManagerRestartAndReportsStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, err := cloud.Open(ctx, cloud.Config{DBPath: filepath.Join(t.TempDir(), "loopernet.sqlite"), AdminToken: "admin-token", ProtocolVersion: protocol.CurrentVersion, MinimumDaemonVersion: "0.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	server := cloud.NewServer(cloud.Config{AdminToken: "admin-token"}, service)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	joinKey, err := service.CreateJoinKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "network.json")
	configPath := writeNetworkJoinConfig(t)
	if err := runNetworkJoin(ctx, []string{"--config", configPath}, statePath, []string{httpServer.URL, "--key", joinKey, "--name", "worker-1"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runNetworkJoin() error = %v", err)
	}
	routed := config.Config{
		Network:  config.NetworkConfig{LoopernetBaseURL: httpServer.URL, NodeName: "worker-1", GitHubLogin: "octocat", GitHubUserID: 42},
		Projects: []config.ProjectRefConfig{{ID: "project_1", Network: config.ProjectNetworkConfig{Mode: config.NetworkModeRouted}}},
		Roles:    config.RoleConfigs{Coordinator: config.CoordinatorRoleConfig{Enabled: true}},
	}
	github := githubinfra.New(githubinfra.Options{GHRun: func(context.Context, shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: `{"login":"octocat","id":42}`}, nil
	}})
	for restart := 0; restart < 2; restart++ {
		manager := networkclient.NewManager(statePath, routed, nil, github)
		if err := manager.Start(ctx); err != nil {
			t.Fatalf("manager restart %d Start() error = %v", restart, err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for !manager.Status().CloudReachable && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !manager.Status().CloudReachable || manager.Status().Lease.HolderNodeID == "" {
			manager.Stop()
			t.Fatalf("manager restart %d did not heartbeat and hold a lease: %#v", restart, manager.Status())
		}
		manager.Stop()
	}
	var output bytes.Buffer
	if err := runNetworkStatus(ctx, statePath, &output); err != nil {
		t.Fatalf("runNetworkStatus() error = %v", err)
	}
	if !strings.Contains(output.String(), "network=") || !strings.Contains(output.String(), "membership=worker-1") {
		t.Fatalf("network status output = %q", output.String())
	}
}

func writeNetworkJoinConfig(t *testing.T) string {
	t.Helper()
	ghPath := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"login\":\"octocat\",\"id\":42}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("[tools]\nghPath = "+strconvQuote(ghPath)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
