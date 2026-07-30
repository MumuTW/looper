package github

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
)

func configWithAgentEnv(env map[string]string) config.Config {
	cfg := config.Config{}
	cfg.Agent.Env = env
	return cfg
}

func TestAuthEnvReadsTokenFromConfig(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	env := AuthEnv(configWithAgentEnv(map[string]string{"GH_TOKEN": "config-token"}))
	if env["GH_TOKEN"] != "config-token" {
		t.Fatalf("AuthEnv()[GH_TOKEN] = %q, want config-token", env["GH_TOKEN"])
	}
}

func TestAuthEnvPrefersDaemonEnvironmentOverConfig(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-token")

	env := AuthEnv(configWithAgentEnv(map[string]string{"GH_TOKEN": "config-token"}))
	if env["GH_TOKEN"] != "ambient-token" {
		t.Fatalf("AuthEnv()[GH_TOKEN] = %q, want the daemon environment to win", env["GH_TOKEN"])
	}
}

func TestAuthEnvIsNilWithNoCredentialAnywhere(t *testing.T) {
	for _, key := range authEnvKeys {
		t.Setenv(key, "")
	}
	if env := AuthEnv(config.Config{}); env != nil {
		t.Fatalf("AuthEnv() = %#v, want nil so the gateway does not set an empty override", env)
	}
	if MissingAuthWarning(config.Config{}) == "" {
		t.Fatal("MissingAuthWarning() = \"\", want an explanation when no credential resolves")
	}
}

func TestMissingAuthWarningIsSilentWhenResolved(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	if warning := MissingAuthWarning(configWithAgentEnv(map[string]string{"GH_TOKEN": "config-token"})); warning != "" {
		t.Fatalf("MissingAuthWarning() = %q, want empty", warning)
	}
}

// This is the regression that matters: the gateway used to set no Env at all, so
// every daemon gh call inherited a launchd environment with no token and went out
// anonymous. Assert the token actually reaches the shell options.
func TestGatewayCarriesConfiguredAuthIntoEveryGhCall(t *testing.T) {
	t.Setenv("GH_TOKEN", "")

	captured := make([]shell.Options, 0, 2)
	gateway := New(Options{
		GHPath: "gh",
		Env:    AuthEnv(configWithAgentEnv(map[string]string{"GH_TOKEN": "config-token"})),
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			captured = append(captured, options)
			return shell.Result{Stdout: "{}"}, nil
		},
	})

	if _, err := gateway.runGh(context.Background(), "/tmp/repo", "", "pr", "view", "1"); err != nil {
		t.Fatalf("runGh() error = %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured %d gh invocations, want 1", len(captured))
	}
	if captured[0].Env["GH_TOKEN"] != "config-token" {
		t.Fatalf("gh invocation Env = %#v, want GH_TOKEN=config-token", captured[0].Env)
	}
}

func TestGatewayLeavesEnvUnsetWhenNoCredentialResolves(t *testing.T) {
	for _, key := range authEnvKeys {
		t.Setenv(key, "")
	}

	var captured shell.Options
	gateway := New(Options{
		GHPath: "gh",
		Env:    AuthEnv(config.Config{}),
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			captured = options
			return shell.Result{Stdout: "{}"}, nil
		},
	})

	if _, err := gateway.runGh(context.Background(), "", "", "pr", "list"); err != nil {
		t.Fatalf("runGh() error = %v", err)
	}
	// An empty override map must stay empty rather than become a replacement
	// environment; gh then still gets its chance at the keyring.
	if len(captured.Env) != 0 {
		t.Fatalf("gh invocation Env = %#v, want empty", captured.Env)
	}
}

func TestAuthEnvCoversEveryTokenVariableGhReads(t *testing.T) {
	for _, key := range authEnvKeys {
		t.Setenv(key, "")
	}
	for _, key := range authEnvKeys {
		env := AuthEnv(configWithAgentEnv(map[string]string{key: "token-" + strings.ToLower(key)}))
		if env[key] == "" {
			t.Fatalf("AuthEnv() dropped %s", key)
		}
	}
}
