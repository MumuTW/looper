package config_test

import (
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func mustDefaultConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	return cfg
}

func TestResolveAgent_ReasoningEffort_GlobalOnly(t *testing.T) {
	cfg := mustDefaultConfig(t)
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	model := "gpt-5.6-terra"
	cfg.Agent.Model = &model
	effort := config.ReasoningEffortHigh
	cfg.Agent.ReasoningEffort = &effort

	resolved, ok := config.ResolveAgent(cfg, "", config.CodingRoleFixer)
	if !ok {
		t.Fatalf("ResolveAgent() ok = false, want true")
	}
	if resolved.ReasoningEffort == nil || *resolved.ReasoningEffort != config.ReasoningEffortHigh {
		t.Fatalf("ResolveAgent().ReasoningEffort = %v, want high", resolved.ReasoningEffort)
	}
}

func TestResolveAgent_ReasoningEffort_RoleOverride(t *testing.T) {
	cfg := mustDefaultConfig(t)
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	model := "gpt-5.6-terra"
	cfg.Agent.Model = &model
	globalEffort := config.ReasoningEffortMedium
	cfg.Agent.ReasoningEffort = &globalEffort

	roleEffort := config.ReasoningEffortHigh
	cfg.Roles.Fixer.Agent = &config.RoleAgentConfig{
		ReasoningEffort: &roleEffort,
	}

	resolved, ok := config.ResolveAgent(cfg, "", config.CodingRoleFixer)
	if !ok {
		t.Fatalf("ResolveAgent() ok = false, want true")
	}
	if resolved.ReasoningEffort == nil || *resolved.ReasoningEffort != config.ReasoningEffortHigh {
		t.Fatalf("ResolveAgent().ReasoningEffort = %v, want high (role override)", resolved.ReasoningEffort)
	}
}

func TestResolveAgent_ReasoningEffort_ProfileOverride(t *testing.T) {
	cfg := mustDefaultConfig(t)
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	model := "gpt-5.6-terra"
	cfg.Agent.Model = &model
	globalEffort := config.ReasoningEffortLow
	cfg.Agent.ReasoningEffort = &globalEffort

	profileEffort := config.ReasoningEffortVeryHigh
	cfg.Agent.Profiles = map[string]config.AgentBindingConfig{
		"performance": {
			Vendor:          &vendor,
			ReasoningEffort: &profileEffort,
		},
	}
	profileID := "performance"
	cfg.Roles.Fixer.Agent = &config.RoleAgentConfig{
		Profile: &profileID,
	}

	resolved, ok := config.ResolveAgent(cfg, "", config.CodingRoleFixer)
	if !ok {
		t.Fatalf("ResolveAgent() ok = false, want true")
	}
	if resolved.ReasoningEffort == nil || *resolved.ReasoningEffort != config.ReasoningEffortVeryHigh {
		t.Fatalf("ResolveAgent().ReasoningEffort = %v, want xhigh (profile override)", resolved.ReasoningEffort)
	}
}

func TestResolveAgent_ReasoningEffort_NoneSuppresses(t *testing.T) {
	cfg := mustDefaultConfig(t)
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	model := "gpt-5.6-terra"
	cfg.Agent.Model = &model
	globalEffort := config.ReasoningEffortHigh
	cfg.Agent.ReasoningEffort = &globalEffort

	roleEffort := config.ReasoningEffortNone
	cfg.Roles.Fixer.Agent = &config.RoleAgentConfig{
		ReasoningEffort: &roleEffort,
	}

	resolved, ok := config.ResolveAgent(cfg, "", config.CodingRoleFixer)
	if !ok {
		t.Fatalf("ResolveAgent() ok = false, want true")
	}
	if resolved.ReasoningEffort == nil || *resolved.ReasoningEffort != config.ReasoningEffortNone {
		t.Fatalf("ResolveAgent().ReasoningEffort = %v, want none", resolved.ReasoningEffort)
	}
}

func TestResolveAgent_ReasoningEffort_NotSet(t *testing.T) {
	cfg := mustDefaultConfig(t)
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	model := "gpt-5.6-terra"
	cfg.Agent.Model = &model

	resolved, ok := config.ResolveAgent(cfg, "", config.CodingRoleFixer)
	if !ok {
		t.Fatalf("ResolveAgent() ok = false, want true")
	}
	if resolved.ReasoningEffort != nil {
		t.Fatalf("ResolveAgent().ReasoningEffort = %v, want nil (unset)", resolved.ReasoningEffort)
	}
}

func TestParseReasoningEffort_Valid(t *testing.T) {
	for _, want := range []config.ReasoningEffort{
		config.ReasoningEffortLow,
		config.ReasoningEffortMedium,
		config.ReasoningEffortHigh,
		config.ReasoningEffortVeryHigh,
		config.ReasoningEffortNone,
	} {
		got, ok := config.ParseReasoningEffort(string(want))
		if !ok {
			t.Fatalf("ParseReasoningEffort(%q) ok = false", want)
		}
		if got != want {
			t.Fatalf("ParseReasoningEffort(%q) = %q", want, got)
		}
	}
}

func TestParseReasoningEffort_Invalid(t *testing.T) {
	for _, bad := range []string{"", "invalid", "max"} {
		if got, ok := config.ParseReasoningEffort(bad); ok {
			t.Fatalf("ParseReasoningEffort(%q) ok = true (got %q), want false", bad, got)
		}
	}
}

func TestParseReasoningEffort_CaseInsensitive(t *testing.T) {
	got, ok := config.ParseReasoningEffort("High")
	if !ok {
		t.Fatalf("ParseReasoningEffort(%q) ok = false", "High")
	}
	if got != config.ReasoningEffortHigh {
		t.Fatalf("ParseReasoningEffort(%q) = %q, want %q", "High", got, config.ReasoningEffortHigh)
	}
}

func TestAgentSnapshotFromResolved_ReasoningEffort(t *testing.T) {
	vendor := config.AgentVendorCodex
	model := "gpt-5.6-terra"
	effort := config.ReasoningEffortHigh
	resolved := config.ResolvedAgent{
		Vendor:          vendor,
		Model:           &model,
		ReasoningEffort: &effort,
	}

	snapshot := config.AgentSnapshotFromResolved(resolved)
	if snapshot.ReasoningEffort == nil || *snapshot.ReasoningEffort != config.ReasoningEffortHigh {
		t.Fatalf("AgentSnapshotFromResolved().ReasoningEffort = %v, want high", snapshot.ReasoningEffort)
	}
}

func TestAgentSnapshotFromResolved_ReasoningEffortUnset(t *testing.T) {
	vendor := config.AgentVendorCodex
	model := "gpt-5.6-terra"
	resolved := config.ResolvedAgent{
		Vendor: vendor,
		Model:  &model,
	}

	snapshot := config.AgentSnapshotFromResolved(resolved)
	if snapshot.ReasoningEffort != nil {
		t.Fatalf("AgentSnapshotFromResolved().ReasoningEffort = %v, want nil", snapshot.ReasoningEffort)
	}
}
