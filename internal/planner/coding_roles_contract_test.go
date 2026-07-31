package planner

import (
	"testing"

	"github.com/MumuTW/looper/internal/config"
)

func TestDiscoveryPolicyForProjectReadsCanonicalPlannerRegistry(t *testing.T) {
	t.Parallel()
	enabled := false
	labels := []string{"canonical-plan"}
	mode := config.LabelModeAny
	assignee := false
	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{Roles: &config.PartialRoleConfigs{Coding: map[string]config.PartialCodingRoleConfig{
		config.CodingRolePlanner: {Discovery: &config.PartialRoleDiscoveryConfig{
			Enabled:                    &enabled,
			Labels:                     &labels,
			LabelMode:                  &mode,
			RequireAssigneeCurrentUser: &assignee,
		}},
	}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	runner := New(Options{CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true}})
	got := runner.discoveryPolicyForProject("")
	if got.AutoDiscovery || len(got.Labels) != 1 || got.Labels[0] != "canonical-plan" || got.LabelMode != config.LabelModeAny || got.RequireAssigneeCurrentUser {
		t.Fatalf("planner discovery policy = %#v", got)
	}
}
