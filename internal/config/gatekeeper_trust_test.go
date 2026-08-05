package config

import (
	"testing"
)

func gatekeeperIssues(cfg Config) []ValidationIssue {
	var issues []ValidationIssue
	validateGatekeeperRoleConfig(cfg.Roles.Gatekeeper, "roles.gatekeeper", cfg.Roles.Reviewer.AutoMerge.Enabled, &issues)
	return issues
}

func TestGatekeeperTrustDefaultsToObserve(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	if trust := cfg.Roles.Gatekeeper.Trust; trust != "" && trust != GatekeeperTrustObserve {
		t.Fatalf("default trust = %q, want observe (or unset)", trust)
	}
	if issues := gatekeeperIssues(cfg); len(issues) != 0 {
		t.Fatalf("default config produced issues: %+v", issues)
	}
}

func TestGatekeeperTrustAcceptsObserveAndAdvise(t *testing.T) {
	t.Parallel()

	for _, trust := range []GatekeeperTrustLevel{GatekeeperTrustObserve, GatekeeperTrustAdvise} {
		cfg := Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: trust}}}
		if issues := gatekeeperIssues(cfg); len(issues) != 0 {
			t.Fatalf("trust %q produced issues: %+v", trust, issues)
		}
	}
}

func TestGatekeeperTrustAcceptsAuto(t *testing.T) {
	t.Parallel()

	issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto}}})

	if len(issues) != 0 {
		t.Fatalf("issues = %+v, want auto accepted", issues)
	}
}

func TestGatekeeperTrustRejectsUnknownLevel(t *testing.T) {
	t.Parallel()

	issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: "merge-everything"}}})

	if len(issues) != 1 || issues[0].Path != "roles.gatekeeper.trust" {
		t.Fatalf("issues = %+v, want one rejecting the unknown level", issues)
	}
}

func TestGatekeeperTrustAcceptsProjectAutoOverride(t *testing.T) {
	t.Parallel()
	auto := GatekeeperTrustAuto
	cfg := Config{
		Roles: RoleConfigs{Reviewer: ReviewerRoleConfig{AutoMerge: ReviewerAutoMergeConfig{Enabled: true}}},
		Projects: []ProjectRefConfig{{
			ID:    "looper",
			Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto}},
		}},
	}

	var issues []ValidationIssue
	validateCoreConfig(cfg, &issues)

	for _, issue := range issues {
		if issue.Path == "projects[0].roles.gatekeeper.trust" {
			t.Fatalf("project auto override was rejected: %+v", issues)
		}
	}
}

func TestMergePartialGatekeeperTrust(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	advise := GatekeeperTrustAdvise
	mergeConfig(&cfg, PartialConfig{Roles: &PartialRoleConfigs{
		Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &advise},
	}})

	if cfg.Roles.Gatekeeper.Trust != GatekeeperTrustAdvise {
		t.Fatalf("merged trust = %q, want advise", cfg.Roles.Gatekeeper.Trust)
	}
}

// clonePartialRoleConfigs is invoked on every configuration layer. A field it
// forgets is silently dropped, and nothing else in the pipeline notices: the
// value type-checks, merges, and validates, then vanishes.
func TestClonePartialRoleConfigsPreservesGatekeeperTrust(t *testing.T) {
	t.Parallel()
	advise := GatekeeperTrustAdvise
	original := &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &advise}}

	cloned := clonePartialRoleConfigs(original)

	if cloned == nil || cloned.Gatekeeper == nil || cloned.Gatekeeper.Trust == nil {
		t.Fatal("clone dropped roles.gatekeeper.trust")
	}
	if *cloned.Gatekeeper.Trust != GatekeeperTrustAdvise {
		t.Fatalf("cloned trust = %q, want advise", *cloned.Gatekeeper.Trust)
	}
	// A shallow copy of the pointer would let a later edit of one layer rewrite
	// another.
	observe := GatekeeperTrustObserve
	*original.Gatekeeper.Trust = observe
	if *cloned.Gatekeeper.Trust != GatekeeperTrustAdvise {
		t.Fatal("clone aliases the original trust pointer")
	}
}

// The end-to-end property the clone bug broke: a project override configured in a
// file must survive every normalization layer and reach Config.
func TestProjectGatekeeperTrustSurvivesNormalization(t *testing.T) {
	t.Parallel()
	advise := GatekeeperTrustAdvise
	partial := PartialConfig{Projects: &[]PartialProjectRefConfig{{
		Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &advise}},
	}}}

	cloned := clonePartialConfig(partial)

	projects := cloned.Projects
	if projects == nil || len(*projects) != 1 {
		t.Fatalf("cloned projects = %v", projects)
	}
	roles := (*projects)[0].Roles
	if roles == nil || roles.Gatekeeper == nil || roles.Gatekeeper.Trust == nil {
		t.Fatal("project gatekeeper trust did not survive clonePartialConfig")
	}
}
