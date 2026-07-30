package config

import (
	"strings"
	"testing"
)

func gatekeeperIssues(cfg Config) []ValidationIssue {
	var issues []ValidationIssue
	validateGatekeeperRoleConfig(cfg.Roles.Gatekeeper, "roles.gatekeeper", &issues)
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

// "auto" is rejected rather than accepted-and-ignored. A merge authority that
// silently behaves one level below what the operator configured is the worst
// possible failure for this setting.
func TestGatekeeperTrustRejectsUnimplementedAuto(t *testing.T) {
	t.Parallel()

	issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: GatekeeperTrustAuto}}})

	if len(issues) != 1 || issues[0].Path != "roles.gatekeeper.trust" {
		t.Fatalf("issues = %+v, want one rejecting auto", issues)
	}
	if !strings.Contains(issues[0].Message, "not implemented") {
		t.Fatalf("message = %q, want it to say auto is not implemented", issues[0].Message)
	}
}

func TestGatekeeperTrustRejectsUnknownLevel(t *testing.T) {
	t.Parallel()

	issues := gatekeeperIssues(Config{Roles: RoleConfigs{Gatekeeper: GatekeeperRoleConfig{Trust: "merge-everything"}}})

	if len(issues) != 1 || issues[0].Path != "roles.gatekeeper.trust" {
		t.Fatalf("issues = %+v, want one rejecting the unknown level", issues)
	}
}

// A project override is validated too: rejecting only the global value would let
// an unimplemented level in through the per-project door.
func TestGatekeeperTrustValidatesProjectOverrides(t *testing.T) {
	t.Parallel()
	auto := GatekeeperTrustAuto
	cfg := Config{Projects: []ProjectRefConfig{{
		ID:    "looper",
		Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{Trust: &auto}},
	}}}

	var issues []ValidationIssue
	validateCoreConfig(cfg, &issues)

	found := false
	for _, issue := range issues {
		if issue.Path == "projects[0].roles.gatekeeper.trust" {
			found = true
		}
	}
	if !found {
		t.Fatalf("project override of auto passed validation; issues = %+v", issues)
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
