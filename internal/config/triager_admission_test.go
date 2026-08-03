package config

import (
	"errors"
	"testing"
)

func TestTriagerAdmissionDefaultsPreserveLegacyPolicy(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	role := cfg.Roles.Triager
	if role.Preset != TriagerPresetLegacy || !role.Classify || len(role.AuthorTiers) != 0 {
		t.Fatalf("triager defaults = %#v", role)
	}
	if role.Legacy.AutoRouteConfidence != 0.8 || role.Legacy.MaxAutoRouteRisk != "low" || !role.Legacy.RequireInScope || !role.Legacy.RequireNoMissingInformation || !role.Legacy.RequirePlanner || !role.Legacy.RequireRationale {
		t.Fatalf("legacy defaults = %#v", role.Legacy)
	}
}

func TestNormalizeTriagerAdmissionConfig(t *testing.T) {
	t.Parallel()
	preset := TriagerPresetMaintainedOSS
	classify := true
	risk := "medium"
	overrides := map[string]TriagerAdmissionOutcome{"member": TriagerAdmissionAuto}
	cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Triager: &PartialTriagerRoleConfig{
		Preset: &preset, Classify: &classify, AuthorTiers: &overrides,
		Legacy: &PartialTriagerLegacyPolicyConfig{MaxAutoRouteRisk: &risk},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles.Triager.Preset != preset || cfg.Roles.Triager.AuthorTiers["member"] != TriagerAdmissionAuto || cfg.Roles.Triager.Legacy.MaxAutoRouteRisk != "medium" {
		t.Fatalf("normalized triager = %#v", cfg.Roles.Triager)
	}
}

func TestProjectTriagerAdmissionOverridesMergeWithoutMutatingGlobal(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Roles.Triager.AuthorTiers["member"] = TriagerAdmissionAssess
	preset := TriagerPresetCompany
	classify := false
	confidence := 0.65
	overrides := map[string]TriagerAdmissionOutcome{"member": TriagerAdmissionAuto, "bot": TriagerAdmissionIgnore}
	cfg.Projects = []ProjectRefConfig{{ID: "demo", Roles: &PartialRoleConfigs{Triager: &PartialTriagerRoleConfig{
		Preset: &preset, Classify: &classify, AuthorTiers: &overrides,
		Legacy: &PartialTriagerLegacyPolicyConfig{AutoRouteConfidence: &confidence},
	}}}}

	got := ProjectRoleConfigs(cfg, "demo").Triager
	if got.Preset != TriagerPresetCompany || got.Classify || got.AuthorTiers["member"] != TriagerAdmissionAuto || got.AuthorTiers["bot"] != TriagerAdmissionIgnore || got.Legacy.AutoRouteConfidence != 0.65 {
		t.Fatalf("project triager = %#v", got)
	}
	if cfg.Roles.Triager.AuthorTiers["member"] != TriagerAdmissionAssess || len(cfg.Roles.Triager.AuthorTiers) != 1 {
		t.Fatalf("global overrides mutated = %#v", cfg.Roles.Triager.AuthorTiers)
	}
}

func TestClonePartialRoleConfigsPreservesTriagerAdmission(t *testing.T) {
	t.Parallel()
	preset := TriagerPresetPersonal
	classify := true
	risk := "medium"
	overrides := map[string]TriagerAdmissionOutcome{"unaffiliated": TriagerAdmissionIgnore}
	original := &PartialRoleConfigs{Triager: &PartialTriagerRoleConfig{
		Preset: &preset, Classify: &classify, AuthorTiers: &overrides,
		Legacy: &PartialTriagerLegacyPolicyConfig{MaxAutoRouteRisk: &risk},
	}}
	cloned := clonePartialRoleConfigs(original)
	if cloned.Triager == nil || cloned.Triager.Preset == nil || *cloned.Triager.Preset != preset || cloned.Triager.AuthorTiers == nil || (*cloned.Triager.AuthorTiers)["unaffiliated"] != TriagerAdmissionIgnore || cloned.Triager.Legacy == nil || *cloned.Triager.Legacy.MaxAutoRouteRisk != "medium" {
		t.Fatalf("cloned triager = %#v", cloned.Triager)
	}
	(*cloned.Triager.AuthorTiers)["unaffiliated"] = TriagerAdmissionAuto
	if (*original.Triager.AuthorTiers)["unaffiliated"] != TriagerAdmissionIgnore {
		t.Fatal("clone shares author-tier override map")
	}
}

func TestValidateTriagerAdmissionRejectsUnsafeOrUnknownPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		edit    func(*Config)
		path    string
		message string
	}{
		{name: "unknown preset", edit: func(cfg *Config) { cfg.Roles.Triager.Preset = "trust-me" }, path: "roles.triager.preset", message: "must be one of: legacy, personal, maintained-oss, company, contributing"},
		{name: "bad confidence", edit: func(cfg *Config) { cfg.Roles.Triager.Legacy.AutoRouteConfidence = 1.1 }, path: "roles.triager.legacy.autoRouteConfidence", message: "must be between 0 and 1"},
		{name: "bad risk", edit: func(cfg *Config) { cfg.Roles.Triager.Legacy.MaxAutoRouteRisk = "critical" }, path: "roles.triager.legacy.maxAutoRouteRisk", message: "must be one of: low, medium, high"},
		{name: "unknown tier", edit: func(cfg *Config) { cfg.Roles.Triager.AuthorTiers["friend"] = TriagerAdmissionAuto }, path: "roles.triager.authorTiers.friend", message: "author tier must be one of: owner, member, past-contributor, unaffiliated, bot"},
		{name: "contributor auto", edit: func(cfg *Config) {
			cfg.Roles.Triager.Preset = TriagerPresetContributing
			cfg.Roles.Triager.AuthorTiers["owner"] = TriagerAdmissionAuto
		}, path: "roles.triager.authorTiers.owner", message: "cannot be auto under the contributing preset"},
		{name: "bot auto", edit: func(cfg *Config) { cfg.Roles.Triager.AuthorTiers["bot"] = TriagerAdmissionAuto }, path: "roles.triager.authorTiers.bot", message: "bot authors cannot be auto-admitted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			test.edit(&cfg)
			err = Validate(cfg)
			var validationErr *ConfigValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate() error = %T, want *ConfigValidationError", err)
			}
			assertValidationIssue(t, validationErr, test.path, test.message)
		})
	}
}
