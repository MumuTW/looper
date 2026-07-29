package fixer

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestDiscoveryPolicyForProjectReadsCanonicalFixerRegistry(t *testing.T) {
	t.Parallel()
	enabled := false
	includeDrafts := true
	filter := config.AuthorFilterAny
	labels := []string{"canonical-fix"}
	mode := config.LabelModeAny
	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{Roles: &config.PartialRoleConfigs{Coding: map[string]config.PartialCodingRoleConfig{
		config.CodingRoleFixer: {Discovery: &config.PartialRoleDiscoveryConfig{
			Enabled:       &enabled,
			IncludeDrafts: &includeDrafts,
			AuthorFilter:  &filter,
			Labels:        &labels,
			LabelMode:     &mode,
		}},
	}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	runner := New(Options{CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true}})
	got := runner.discoveryPolicyForProject("")
	if got.AutoDiscovery || !got.IncludeDrafts || got.AuthorFilter != config.FixerAuthorFilterAny || len(got.Labels) != 1 || got.Labels[0] != "canonical-fix" || got.LabelMode != config.LabelModeAny {
		t.Fatalf("fixer discovery policy = %#v", got)
	}
}
