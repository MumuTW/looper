package worker

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestDiscoveryPolicyForProjectReadsCanonicalWorkerRegistry(t *testing.T) {
	t.Parallel()
	enabled := false
	labels := []string{"canonical-work"}
	mode := config.LabelModeAny
	assignee := false
	planeUser := "plane-user"
	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{Roles: &config.PartialRoleConfigs{Coding: map[string]config.PartialCodingRoleConfig{
		config.CodingRoleWorker: {Discovery: &config.PartialRoleDiscoveryConfig{
			Enabled:                    &enabled,
			Labels:                     &labels,
			LabelMode:                  &mode,
			RequireAssigneeCurrentUser: &assignee,
			PlaneAssigneeID:            &planeUser,
		}},
	}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	runner := New(Options{CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true}})
	got := runner.discoveryPolicyForProject("")
	if got.AutoDiscovery || len(got.Labels) != 1 || got.Labels[0] != "canonical-work" || got.LabelMode != config.LabelModeAny || got.RequireAssigneeCurrentUser || got.PlaneAssigneeID != "plane-user" {
		t.Fatalf("worker discovery policy = %#v", got)
	}
}
