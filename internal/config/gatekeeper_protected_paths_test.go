package config

import (
	"fmt"
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
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{ProtectedPaths: []string{"", " ../secret", "/absolute/*", "[bad"}}, "roles.gatekeeper", &issues)
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
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{ProtectedPaths: []string{"./", ".", "foo/.."}}, "roles.gatekeeper", &issues)
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
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{ProtectedPaths: []string{"foo/../bar", "a/./b", "x/.."}}, "roles.gatekeeper", &issues)
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

func TestGatekeeperProtectedPathsRejectEmptySegments(t *testing.T) {
	issues := []ValidationIssue{}
	validateGatekeeperRoleConfig(GatekeeperRoleConfig{ProtectedPaths: []string{"internal/migrations/", "internal//generated/**"}}, "roles.gatekeeper", &issues)
	if len(issues) != 2 {
		t.Fatalf("issues = %#v, want one issue per empty-segment pattern", issues)
	}
}
