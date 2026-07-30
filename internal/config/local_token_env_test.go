package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileAcceptsLocalTokenFromEnvironment(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.toml")
	if err := os.WriteFile(configPath, []byte("[server]\nauthMode = \"local-token\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_TOKEN": "environment-token"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Config.Server.LocalToken == nil || *loaded.Config.Server.LocalToken != "environment-token" {
		t.Fatalf("server.localToken = %v, want environment token", loaded.Config.Server.LocalToken)
	}
	if got := loaded.Metadata.FieldSources["server.localToken"]; got != ValueSourceEnv {
		t.Fatalf("server.localToken source = %q, want %q", got, ValueSourceEnv)
	}
}

func TestLoadFileTrimsLocalTokenFromEnvironment(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.toml")
	if err := os.WriteFile(configPath, []byte("[server]\nauthMode = \"local-token\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_TOKEN": "  environment-token  "}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Config.Server.LocalToken == nil || *loaded.Config.Server.LocalToken != "environment-token" {
		t.Fatalf("server.localToken = %v, want trimmed environment token", loaded.Config.Server.LocalToken)
	}
}

func TestEmptyLocalTokenEnvironmentDoesNotFallBackToFileSecret(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.toml")
	if err := os.WriteFile(configPath, []byte("[server]\nauthMode = \"local-token\"\nlocalToken = \"file-token\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_TOKEN": ""}),
	})
	var validationErr *ConfigValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("LoadFile() error = %T %v, want validation error", err, err)
	}
	assertValidationIssue(t, validationErr, "server.localToken", "is required when authMode is local-token")
}

func TestLocalTokenEnvironmentOverridesFile(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, "config.toml")
	if err := os.WriteFile(configPath, []byte("[server]\nauthMode = \"local-token\"\nlocalToken = \"file-token\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := LoadFile(LoadFileOptions{
		CWD:        cwd,
		ConfigPath: configPath,
		LookupEnv:  mapEnvLookup(map[string]string{"LOOPER_TOKEN": "environment-token"}),
	})
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if loaded.Config.Server.LocalToken == nil || *loaded.Config.Server.LocalToken != "environment-token" {
		t.Fatalf("server.localToken = %v, want environment token", loaded.Config.Server.LocalToken)
	}
}
