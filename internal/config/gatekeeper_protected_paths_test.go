package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestGatekeeperProtectedPathsNormalizeAndClone(t *testing.T) {
	paths := []string{"internal/gatekeeper/**", ".github/workflows/*"}
	cfg, err := Normalize(t.TempDir(), PartialConfig{Roles: &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{ProtectedPaths: &paths}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if len(cfg.Roles.Gatekeeper.ProtectedPaths) != len(paths) || cfg.Roles.Gatekeeper.ProtectedPaths[0] != paths[0] {
		t.Fatalf("protected paths = %#v, want %#v", cfg.Roles.Gatekeeper.ProtectedPaths, paths)
	}
	original := &PartialRoleConfigs{Gatekeeper: &PartialGatekeeperRoleConfig{ProtectedPaths: &paths}}
	cloned := clonePartialRoleConfigs(original)
	paths[0] = "changed"
	if cloned == nil || cloned.Gatekeeper == nil || cloned.Gatekeeper.ProtectedPaths == nil || (*cloned.Gatekeeper.ProtectedPaths)[0] != "internal/gatekeeper/**" {
		t.Fatalf("cloned protected paths = %#v, want independent copy", cloned)
	}
}

func TestGatekeeperProtectedPathsValidation(t *testing.T) {
	issues := []ValidationIssue{}
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{ProtectedPaths: []string{"", " ../secret", "/absolute/*", "[bad"}}, "roles.gatekeeper", false, &issues)
	want := []string{"roles.gatekeeper.protectedPaths[0]", "roles.gatekeeper.protectedPaths[1]", "roles.gatekeeper.protectedPaths[2]", "roles.gatekeeper.protectedPaths[3]"}
	for _, path := range want {
		found := false
		for _, issue := range issues {
			if issue.Path == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("issues = %#v, want validation at %s", issues, path)
		}
	}
}

func TestGatekeeperProtectedPathsRejectNoOpPatterns(t *testing.T) {
	issues := []ValidationIssue{}
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{ProtectedPaths: []string{"./", ".", "foo/.."}}, "roles.gatekeeper", false, &issues)
	for _, index := range []int{0, 1, 2} {
		want := fmt.Sprintf("roles.gatekeeper.protectedPaths[%d]", index)
		found := false
		for _, issue := range issues {
			if issue.Path == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("issues = %#v, want no-op pattern validation at %s", issues, want)
		}
	}
}

func TestGatekeeperProtectedPathsRejectDotSegments(t *testing.T) {
	issues := []ValidationIssue{}
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{ProtectedPaths: []string{"foo/../bar", "a/./b", "x/.."}}, "roles.gatekeeper", false, &issues)
	for _, index := range []int{0, 1, 2} {
		want := fmt.Sprintf("roles.gatekeeper.protectedPaths[%d]", index)
		found := false
		for _, issue := range issues {
			if issue.Path == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("issues = %#v, want dot-segment rejection at %s", issues, want)
		}
	}
}

func TestGatekeeperProtectedPathsRejectReviewerAutoMerge(t *testing.T) {
	for _, trust := range []GatekeeperTrustLevel{"", GatekeeperTrustObserve, GatekeeperTrustAdvise, GatekeeperTrustAuto} {
		issues := []ValidationIssue{}
		validateGatekeeperRoleConfig(GatekeeperRoleConfig{Trust: trust, ProtectedPaths: []string{"internal/gatekeeper/**"}}, "roles.gatekeeper", true, &issues)
		found := false
		for _, issue := range issues {
			if issue.Path == "roles.gatekeeper.protectedPaths" && strings.Contains(issue.Message, "roles.reviewer.autoMerge.enabled") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("trust=%q issues = %#v, want protectedPaths/reviewer auto-merge conflict", trust, issues)
		}
	}
}

func TestGatekeeperProtectedPathsAllowReviewerAutoMergeWithoutPolicy(t *testing.T) {
	issues := []ValidationIssue{}
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{Trust: GatekeeperTrustAdvise, ProtectedPaths: nil}, "roles.gatekeeper", true, &issues)
	for _, issue := range issues {
		if issue.Path == "roles.gatekeeper.protectedPaths" {
			t.Fatalf("issues = %#v, want no protectedPaths conflict when policy is empty", issues)
		}
	}
}
