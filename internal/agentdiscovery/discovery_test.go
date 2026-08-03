package agentdiscovery

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/infra/shell"
)

func TestHermesDiscoveryProducesAdvisoryPatchWithoutMutation(t *testing.T) {
	t.Parallel()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	report := Discover(context.Background(), cfg, Dependencies{
		LookPath: func(command string) (string, error) {
			if command == "hermes" {
				return "/trusted/bin/hermes", nil
			}
			return "", exec.ErrNotFound
		},
		EvalSymlinks: func(path string) (string, error) { return path, nil },
		Probe: func(_ context.Context, descriptor config.AgentProviderDescriptor, path string) (string, error) {
			if descriptor.Vendor != config.AgentVendorHermes || path != "/trusted/bin/hermes" {
				t.Fatalf("probe = (%q, %q)", descriptor.Vendor, path)
			}
			return "Hermes Agent 1.2.3", nil
		},
	})
	if cfg.Agent.Vendor != nil {
		t.Fatalf("discovery mutated agent.vendor to %v", cfg.Agent.Vendor)
	}
	if report.Suggestion == nil || string(report.Suggestion.Set["agent.vendor"]) != `"hermes"` {
		t.Fatalf("suggestion = %#v, want canonical Hermes patch", report.Suggestion)
	}
	hermes := candidateFor(t, report, config.AgentVendorHermes)
	if hermes.Status != "ready" || hermes.Version != "Hermes Agent 1.2.3" || hermes.Executable != "/trusted/bin/hermes" {
		t.Fatalf("Hermes candidate = %#v", hermes)
	}
}

func TestHermesSuggestionNeverOverridesExplicitAgentAuthority(t *testing.T) {
	t.Parallel()
	base, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vendor := config.AgentVendorCodex
	model := "gpt-5"
	profileVendor := config.AgentVendorClaudeCode
	roleVendor := config.AgentVendorOpenCode
	for _, test := range []struct {
		name      string
		configure func(*config.Config)
	}{
		{name: "global vendor", configure: func(cfg *config.Config) { cfg.Agent.Vendor = &vendor }},
		{name: "global model", configure: func(cfg *config.Config) { cfg.Agent.Model = &model }},
		{name: "global reasoning effort", configure: func(cfg *config.Config) {
			effort := config.ReasoningEffortHigh
			cfg.Agent.ReasoningEffort = &effort
		}},
		{name: "global params", configure: func(cfg *config.Config) { cfg.Agent.Params = map[string]any{"args": []any{"--provider", "openrouter"}} }},
		{name: "global env", configure: func(cfg *config.Config) { cfg.Agent.Env = map[string]string{"OPENAI_API_KEY": "secret"} }},
		{name: "profile", configure: func(cfg *config.Config) {
			cfg.Agent.Profiles = map[string]config.AgentBindingConfig{"fast": {Vendor: &profileVendor}}
		}},
		{name: "role", configure: func(cfg *config.Config) { cfg.Roles.Worker.Agent = &config.RoleAgentConfig{Vendor: &roleVendor} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.CloneConfig(base)
			test.configure(&cfg)
			report := Discover(context.Background(), cfg, readyHermesDependencies())
			if report.Suggestion != nil {
				t.Fatalf("suggestion = %#v, want none for explicit authority", report.Suggestion)
			}
		})
	}
}

func TestProbeFailuresAreBoundedAndDoNotBlockOtherCandidates(t *testing.T) {
	t.Parallel()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	report := Discover(context.Background(), cfg, Dependencies{
		LookPath:     func(command string) (string, error) { return "/trusted/bin/" + command, nil },
		EvalSymlinks: func(path string) (string, error) { return path, nil },
		Probe: func(_ context.Context, descriptor config.AgentProviderDescriptor, _ string) (string, error) {
			switch descriptor.Vendor {
			case config.AgentVendorClaudeCode:
				return "", &shell.CommandExecutionError{Category: shell.FailureSupervisorTimeout}
			case config.AgentVendorCodex:
				return "", errors.New("malformed output containing seeded-secret-value")
			case config.AgentVendorHermes:
				return "Hermes Agent 2.0.0", nil
			default:
				return "Tool 1.0.0", nil
			}
		},
	})
	if candidateFor(t, report, config.AgentVendorClaudeCode).Status != "timed_out" {
		t.Fatalf("Claude candidate = %#v", candidateFor(t, report, config.AgentVendorClaudeCode))
	}
	codex := candidateFor(t, report, config.AgentVendorCodex)
	if codex.Status != "failed" || strings.Contains(codex.Diagnostic, "seeded-secret-value") {
		t.Fatalf("Codex diagnostic leaked probe error: %#v", codex)
	}
	if candidateFor(t, report, config.AgentVendorHermes).Status != "ready" || report.Suggestion == nil {
		t.Fatalf("Hermes was blocked by sibling failures: %#v", report)
	}
}

func TestDiscoveryJSONContainsNoConfigOrProbeSecretValues(t *testing.T) {
	t.Parallel()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Env = map[string]string{"HERMES_API_KEY": "seeded-api-secret"}
	report := Discover(context.Background(), cfg, readyHermesDependencies())
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "seeded-api-secret") {
		t.Fatalf("report leaked configured secret: %s", raw)
	}
	if report.Suggestion != nil {
		t.Fatal("explicit environment authority should suppress vendor suggestion")
	}
}

func TestHermesVersionOutputMustIdentifyHermesAndRemainBounded(t *testing.T) {
	t.Parallel()
	descriptor, _ := config.AgentProviderDescriptorFor(config.AgentVendorHermes)
	for _, test := range []struct {
		name    string
		result  shell.Result
		want    string
		wantErr bool
	}{
		{name: "stdout", result: shell.Result{Stdout: "Hermes Agent 1.2.3\n"}, want: "Hermes Agent 1.2.3"},
		{name: "stderr", result: shell.Result{Stderr: "hermes-agent version 2.0\n"}, want: "hermes-agent version 2.0"},
		{name: "wrong executable", result: shell.Result{Stdout: "unrelated 1.2.3"}, wantErr: true},
		{name: "missing version", result: shell.Result{Stdout: "Hermes Agent"}, wantErr: true},
		{name: "truncated", result: shell.Result{Stdout: "Hermes Agent 1.2.3", StdoutTruncated: true}, wantErr: true},
		{name: "oversized", result: shell.Result{Stdout: "Hermes Agent 1.2.3 " + strings.Repeat("x", 200)}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseVersionOutput(descriptor, test.result)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseVersionOutput() = %q, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseVersionOutput() = (%q, %v), want %q", got, err, test.want)
			}
		})
	}
}

func readyHermesDependencies() Dependencies {
	return Dependencies{
		LookPath: func(command string) (string, error) {
			if command == "hermes" {
				return "/trusted/bin/hermes", nil
			}
			return "", exec.ErrNotFound
		},
		EvalSymlinks: func(path string) (string, error) { return path, nil },
		Probe: func(context.Context, config.AgentProviderDescriptor, string) (string, error) {
			return "Hermes Agent 1.2.3", nil
		},
	}
}

func candidateFor(t *testing.T, report Report, vendor config.AgentVendor) Candidate {
	t.Helper()
	for _, candidate := range report.Candidates {
		if candidate.Vendor == vendor {
			return candidate
		}
	}
	t.Fatalf("candidate %q missing from %#v", vendor, report.Candidates)
	return Candidate{}
}

func TestAgentProviderDescriptorCopiesAreDetached(t *testing.T) {
	t.Parallel()
	one := config.AgentProviderDescriptors()
	two := config.AgentProviderDescriptors()
	one[0].VersionArgs[0] = "changed"
	if reflect.DeepEqual(one, two) {
		t.Fatal("descriptor copies unexpectedly share mutation")
	}
}
