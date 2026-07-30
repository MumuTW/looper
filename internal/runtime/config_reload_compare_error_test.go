package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestReloadConfigReportsComparisonFailureAndKeepsLastGoodConfig(t *testing.T) {
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loaded := config.LoadedFileConfig{Config: cfg, Metadata: config.LoadFileMetadata{ConfigPath: "/tmp/config.toml", ConfigFilePresent: true}}
	candidate := loaded
	candidate.Config = config.CloneConfig(cfg)
	candidate.Config.Agent.Params["callback"] = func() {}
	runtime := New(Options{Config: cfg, InitialConfig: loaded, ReloadConfig: func() (config.LoadedFileConfig, error) { return candidate, nil }})

	err = runtime.ReloadConfig(context.Background())
	var reloadErr *ConfigReloadError
	if !errors.As(err, &reloadErr) || reloadErr.Kind != "invalid" {
		t.Fatalf("ReloadConfig() error = %#v, want invalid ConfigReloadError", err)
	}
	status := runtime.ConfigReloadStatus()
	if status.LastAttemptAt == nil || status.LastError != "configuration reload rejected: config file could not be decoded or validated" {
		t.Fatalf("ConfigReloadStatus() = %#v", status)
	}
	if _, exists := runtime.Config().Agent.Params["callback"]; exists {
		t.Fatal("failed comparison replaced last-known-good config")
	}
}
