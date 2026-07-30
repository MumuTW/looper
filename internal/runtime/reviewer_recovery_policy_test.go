package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestReviewerRecoveryPolicyUsesCanonicalDraftSetting(t *testing.T) {
	t.Parallel()

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Roles.Reviewer.Discovery.Triggers.IncludeDrafts = false
	cfg.Roles.Coding = config.EffectiveCodingRoles(cfg.Roles)
	reviewer := cfg.Roles.Coding[config.CodingRoleReviewer]
	reviewer.Discovery.IncludeDrafts = true
	cfg.Roles.Coding[config.CodingRoleReviewer] = reviewer

	policy := New(Options{Config: cfg}).reviewerRecoveryPolicyForProject("api-managed")
	if !policy.includeDrafts {
		t.Fatal("reviewer recovery includeDrafts = false, want canonical roles.coding.reviewer value")
	}
}
