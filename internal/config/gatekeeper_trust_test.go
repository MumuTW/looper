package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func gatekeeperIssues(cfg Config) []ValidationIssue {
	var issues []ValidationIssue
	validateGatekeeperRoleConfig(cfg.Roles.Gatekeeper, "roles.gatekeeper", &issues)
	return issues
}

func TestGatekeeperDefaultsOwnMergeStrategy(t *testing.T) {
	t.Parallel()
	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Roles.Gatekeeper.Strategy != MergeStrategySquash {
		t.Fatalf("strategy = %q, want squash", cfg.Roles.Gatekeeper.Strategy)
	}
	if issues := gatekeeperIssues(cfg); len(issues) != 0 {
		t.Fatalf("default config produced issues: %+v", issues)
	}
}

func TestGatekeeperTrustAndStrategyValidation(t *testing.T) {
	t.Parallel()
	for _, trust := range []GatekeeperTrustLevel{GatekeeperTrustObserve, GatekeeperTrustAdvise, GatekeeperTrustAuto} {
		for _, strategy := range []MergeStrategy{MergeStrategySquash, MergeStrategyMerge, MergeStrategyRebase} {
			cfg := Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: trust, Strategy: strategy}}}
			if issues := gatekeeperIssues(cfg); len(issues) != 0 {
				t.Fatalf("trust=%q strategy=%q issues=%+v", trust, strategy, issues)
			}
		}
	}
	for _, test := range []struct {
		role GatekeeperRoleConfig
		path string
	}{
		{role: GatekeeperRoleConfig{Trust: "merge-everything", Strategy: MergeStrategySquash}, path: "roles.gatekeeper.trust"},
		{role: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto, Strategy: "octopus"}, path: "roles.gatekeeper.strategy"},
	} {
		issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: test.role}})
		if len(issues) != 1 || issues[0].Path != test.path {
			t.Fatalf("role=%+v issues=%+v, want %s", test.role, issues, test.path)
		}
	}
}

func TestReviewerAutoMergeEnabledProducesMigrationError(t *testing.T) {
	t.Parallel()
	enabled := true
	for _, partial := range []PartialConfig{
		{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &enabled}}}},
		{Projects: &[]PartialProjectRefConfig{{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &enabled}}}}}},
	} {
		cfg, err := Normalize(t.TempDir(), partial)
		if err != nil {
			t.Fatal(err)
		}
		err = Validate(cfg)
		if err == nil || !strings.Contains(err.Error(), "roles.gatekeeper.trust") {
			t.Fatalf("Validate() error = %v, want explicit Gatekeeper migration", err)
		}
	}
}

func TestDisabledReviewerAutoMergeDefaultStrategyIsAcceptedButNonDefaultFails(t *testing.T) {
	t.Parallel()
	disabled := false
	defaultStrategy := MergeStrategySquash
	cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &disabled, Strategy: &defaultStrategy}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("disabled compatibility input rejected: %v", err)
	}
	raw := string(mustJSON(t, cfg.Roles.Reviewer))
	if strings.Contains(raw, "autoMerge") {
		t.Fatalf("deprecated autoMerge was projected: %s", raw)
	}
	for _, strategy := range []MergeStrategy{MergeStrategyRebase, MergeStrategyMerge} {
		cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{Enabled: &disabled, Strategy: &strategy}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "roles.reviewer.autoMerge.strategy") {
			t.Fatalf("Validate(strategy=%q) error = %v, want explicit migration", strategy, err)
		}
	}
}

func TestRedactProjectSecretsOmitsReviewerCompatibilityBlock(t *testing.T) {
	t.Parallel()
	projects := []ProjectRefConfig{{ID: "project_1", Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{}}}}}
	redacted := RedactProjectSecrets(projects)
	if redacted[0].Roles == projects[0].Roles {
		t.Fatal("redaction reused project roles pointer")
	}
	if redacted[0].Roles.Reviewer == nil || redacted[0].Roles.Reviewer.AutoMerge != nil {
		t.Fatalf("redacted reviewer role = %#v, want compatibility block omitted", redacted[0].Roles.Reviewer)
	}
	if projects[0].Roles.Reviewer.AutoMerge == nil {
		t.Fatal("redaction mutated source project")
	}
}

func TestGatekeeperProjectOverrideSurvivesNormalizationAndClone(t *testing.T) {
	t.Parallel()
	auto := GatekeeperTrustAuto
	rebase := MergeStrategyRebase
	partial := PartialConfig{Projects: &[]PartialProjectRefConfig{{Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto, Strategy: &rebase}}}}}
	cloned := clonePartialConfig(partial)
	role := (*cloned.Projects)[0].Roles.Gatekeeper
	if role == nil || role.Trust == nil || *role.Trust != auto || role.Strategy == nil || *role.Strategy != rebase {
		t.Fatalf("cloned role = %+v", role)
	}
	observe := GatekeeperTrustObserve
	(*partial.Projects)[0].Roles.Gatekeeper.Trust = &observe
	if *role.Trust != auto {
		t.Fatal("clone aliases original trust pointer")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
