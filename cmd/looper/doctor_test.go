package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestDiscoverProviderCandidatesReportsOnlyCredentialKeyPresence(t *testing.T) {
	t.Parallel()
	descriptors := []config.AgentProviderDescriptor{{
		Vendor: config.AgentVendorHermes, ExecutableCandidates: []string{"hermes"}, VersionArgs: []string{"--version"}, CredentialEnvKeys: []string{"HERMES_API_KEY"},
	}}
	candidates := discoverProviderCandidates(context.Background(), descriptors,
		func(name string) (string, error) { return "/tools/" + name, nil },
		func(key string) (string, bool) { return "seeded-secret-value", key == "HERMES_API_KEY" },
		func(context.Context, string, []string) (string, error) { return "hermes 1.2.3", nil },
	)
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	candidate := candidates[0]
	if candidate.ExecutablePath != "/tools/hermes" || candidate.Version != "hermes 1.2.3" {
		t.Fatalf("candidate = %#v", candidate)
	}
	if got := strings.Join(candidate.CredentialKeyPresent, ","); got != "HERMES_API_KEY" || strings.Contains(got, "seeded-secret-value") {
		t.Fatalf("credential presence = %q, must contain only the key name", got)
	}
}

func TestDiscoverProviderCandidatesContinuesAfterProbeFailure(t *testing.T) {
	t.Parallel()
	descriptors := []config.AgentProviderDescriptor{
		{Vendor: config.AgentVendorHermes, ExecutableCandidates: []string{"broken"}, VersionArgs: []string{"--version"}},
		{Vendor: config.AgentVendorCodex, ExecutableCandidates: []string{"codex"}, VersionArgs: []string{"--version"}},
	}
	candidates := discoverProviderCandidates(context.Background(), descriptors,
		func(name string) (string, error) { return "/tools/" + name, nil },
		func(string) (string, bool) { return "", false },
		func(_ context.Context, executable string, _ []string) (string, error) {
			if strings.HasSuffix(executable, "broken") {
				return "", errors.New("malformed version output")
			}
			return "codex 0.1", nil
		},
	)
	if got := candidates[0].ProbeDiagnostic; !strings.Contains(got, "malformed") {
		t.Fatalf("first probe diagnostic = %q", got)
	}
	if got := candidates[1].Version; got != "codex 0.1" {
		t.Fatalf("second candidate version = %q, want successful later probe", got)
	}
}

func TestRunDoctorRejectsUnknownArguments(t *testing.T) {
	t.Parallel()
	err := runDoctor(context.Background(), nil, []string{"--unknown"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "takes no arguments, or exactly --apply") {
		t.Fatalf("runDoctor(--unknown) error = %v", err)
	}
}

func TestPatchTOMLAgentVendorPreservesExistingAgentTable(t *testing.T) {
	t.Parallel()
	patched, err := patchTOMLAgentVendor([]byte("[agent]\nmodel = \"fast\"\n\n[defaults]\nbaseBranch = \"main\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(patched), "[agent]\nvendor = \"hermes\"\nmodel = \"fast\"\n\n[defaults]\nbaseBranch = \"main\"\n"; got != want {
		t.Fatalf("patched config = %q, want %q", got, want)
	}
}

func TestApplyHermesVendorPatchRejectsStaleRevision(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[agent]\nmodel = \"fast\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyHermesVendorPatch(path, config.ConfigFileRevision([]byte("other"), true)); err == nil || !strings.Contains(err.Error(), "changed since provider discovery") {
		t.Fatalf("applyHermesVendorPatch(stale) error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("stale patch changed config: %q", got)
	}
}

func TestApplyHermesVendorPatchAppliesCanonicalPatch(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[agent]\nmodel = \"fast\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyHermesVendorPatch(path, config.ConfigFileRevision(original, true)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[agent]\nvendor = \"hermes\"\nmodel = \"fast\"\n"; string(got) != want {
		t.Fatalf("patched config = %q, want %q", got, want)
	}
}
