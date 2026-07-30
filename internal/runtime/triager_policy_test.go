package runtime

import (
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/triager/admission"
)

func TestTriagerProjectPolicyUsesEffectiveProjectOverride(t *testing.T) {
	t.Parallel()
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preset := config.TriagerPresetCompany
	classify := false
	overrides := map[string]config.TriagerAdmissionOutcome{"member": config.TriagerAdmissionAuto}
	cfg.Projects = []config.ProjectRefConfig{{ID: "demo", Roles: &config.PartialRoleConfigs{Triager: &config.PartialTriagerRoleConfig{Preset: &preset, Classify: &classify, AuthorTiers: &overrides}}}}
	policy := triagerProjectPolicy(cfg, "demo")
	if policy.Admission.Preset != admission.PresetCompany || policy.Admission.Classify {
		t.Fatalf("admission policy = %#v", policy.Admission)
	}
	if policy.Admission.Overrides[admission.AuthorTierMember] != admission.OutcomeAuto {
		t.Fatalf("overrides = %#v", policy.Admission.Overrides)
	}
	if policy.Legacy.AutoRouteConfidence != 0.8 || !policy.Legacy.RequireRationale {
		t.Fatalf("legacy policy = %#v", policy.Legacy)
	}
}
