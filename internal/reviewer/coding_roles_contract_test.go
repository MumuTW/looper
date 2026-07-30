package reviewer

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestDiscoveryPolicyForProjectReadsCanonicalReviewerRegistry(t *testing.T) {
	t.Parallel()
	enabled := false
	includeDrafts := true
	requireReviewRequest := false
	enableSelfReview := true
	labels := []string{"canonical-review"}
	mode := config.LabelModeAny
	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{Roles: &config.PartialRoleConfigs{Coding: map[string]config.PartialCodingRoleConfig{
		config.CodingRoleReviewer: {Discovery: &config.PartialRoleDiscoveryConfig{
			Enabled:              &enabled,
			IncludeDrafts:        &includeDrafts,
			RequireReviewRequest: &requireReviewRequest,
			EnableSelfReview:     &enableSelfReview,
			Labels:               &labels,
			LabelMode:            &mode,
		}},
	}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	runner := New(Options{CustomInstructions: &cfg, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true}})
	got := runner.discoveryPolicyForProject("")
	if got.AutoDiscovery || !got.IncludeDrafts || got.RequireReviewRequest || !got.EnableSelfReview || len(got.Labels) != 1 || got.Labels[0] != "canonical-review" || got.LabelMode != config.LabelModeAny {
		t.Fatalf("reviewer discovery policy = %#v", got)
	}
}
