package config

import (
	"strings"
	"testing"
)

func TestNormalizeCodingRoleOverlayDrivesRegistryConsumers(t *testing.T) {
	t.Parallel()
	partial := mustDecodeTOML(t, `
[agent.profiles.fast]
vendor = "codex"

[roles.worker]
instructions = "legacy guidance"

[roles.coding.worker]
priority = 5
instructions = "canonical guidance"
[roles.coding.worker.discovery]
source = "issue"
enabled = false
labels = ["ready"]
labelMode = "any"
requireAssigneeCurrentUser = false
[roles.coding.worker.agent]
profile = "fast"
`)
	cfg := mustNormalize(t, partial)
	worker := cfg.Roles.Coding[CodingRoleWorker]
	if worker.Priority != 5 || worker.Discovery.Enabled || worker.Discovery.LabelMode != LabelModeAny || worker.Instructions != "canonical guidance" {
		t.Fatalf("worker registry = %#v", worker)
	}
	if got, ok := ResolveAgent(cfg, "", CodingRoleWorker); !ok || got.Vendor != AgentVendorCodex || got.ProfileID != "fast" {
		t.Fatalf("ResolveAgent(worker) = %#v, %v", got, ok)
	}
	block := BuildCustomInstructionBlock(cfg, "", CodingRoleWorker)
	if !strings.Contains(block.Text, "canonical guidance") || strings.Contains(block.Text, "legacy guidance") {
		t.Fatalf("custom instruction block = %q", block.Text)
	}
	if got := CodingRoleNames(cfg.Roles)[0]; got != CodingRoleWorker {
		t.Fatalf("first lane = %q, want worker priority override", got)
	}
}

func TestNormalizeCodingRoleOverlaysMergeWithoutMutatingLayers(t *testing.T) {
	t.Parallel()
	base := mustDecodeTOML(t, `
[roles.coding.reviewer]
priority = 25
[roles.coding.reviewer.discovery]
source = "pull_request"
labels = ["base"]
`)
	override := mustDecodeTOML(t, `
[roles.coding.reviewer.discovery]
labels = ["override"]
[roles.coding.reviewer.agent]
model = "fast"
`)
	merged := mustNormalize(t, base, override).Roles.Coding[CodingRoleReviewer]
	if len(merged.Discovery.Labels) != 1 || merged.Discovery.Labels[0] != "override" || merged.Agent == nil || merged.Agent.Model == nil {
		t.Fatalf("merged reviewer = %#v", merged)
	}
	replayed := mustNormalize(t, base).Roles.Coding[CodingRoleReviewer]
	if len(replayed.Discovery.Labels) != 1 || replayed.Discovery.Labels[0] != "base" || replayed.Agent != nil {
		t.Fatalf("base was mutated: %#v", replayed)
	}
}

func TestNormalizeRejectsUnrunnableOrPolicyCodingRoles(t *testing.T) {
	t.Parallel()
	for name, wantPath := range map[string]string{
		"auditor":     "roles.coding.auditor",
		"gatekeeper":  "roles.coding.gatekeeper",
		"coordinator": "roles.coding.coordinator",
	} {
		name, wantPath := name, wantPath
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Normalize(t.TempDir(), mustDecodeTOML(t, "[roles.coding."+name+"]\npriority = 1\n"))
			if !hasIssuePath(normalizeIssuePaths(err), wantPath) {
				t.Fatalf("issues = %v, want %s (err = %v)", normalizeIssuePaths(err), wantPath, err)
			}
		})
	}
}

func TestNormalizeRejectsCodingRoleSourceMismatches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		toml string
		path string
	}{
		{"wrong source", "[roles.coding.worker.discovery]\nsource = \"pull_request\"\n", "roles.coding.worker.discovery.source"},
		{"pr field", "[roles.coding.worker.discovery]\nincludeDrafts = false\n", "roles.coding.worker.discovery.includeDrafts"},
		{"issue field", "[roles.coding.reviewer.discovery]\nrequireAssigneeCurrentUser = false\n", "roles.coding.reviewer.discovery.requireAssigneeCurrentUser"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Normalize(t.TempDir(), mustDecodeTOML(t, tc.toml))
			if !hasIssuePath(normalizeIssuePaths(err), tc.path) {
				t.Fatalf("issues = %v, want %s", normalizeIssuePaths(err), tc.path)
			}
		})
	}
}

func TestNormalizeRejectsProjectCodingRoles(t *testing.T) {
	t.Parallel()
	partial := mustDecodeTOML(t, `
[[projects]]
id = "demo"
name = "Demo"
repoPath = "/tmp/demo"
[projects.roles.coding.worker]
priority = 1
`)
	_, err := Normalize(t.TempDir(), partial)
	if !hasIssuePath(normalizeIssuePaths(err), "projects[0].roles.coding") {
		t.Fatalf("issues = %v", normalizeIssuePaths(err))
	}
}
