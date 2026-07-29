package config

import (
	"reflect"
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

func TestLoadFileLegacyDiscoveryOverridesHonorCodingRoleLayerPrecedence(t *testing.T) {
	t.Parallel()
	const fileConfig = `{
  "roles": {
    "coding": {
      "planner": {"discovery": {"enabled": false, "labels": ["file-plan"], "labelMode": "all", "requireAssigneeCurrentUser": true}},
      "worker": {"discovery": {"enabled": false, "labels": ["file-work"], "labelMode": "all", "requireAssigneeCurrentUser": true}},
      "reviewer": {"discovery": {"enabled": false, "includeDrafts": true, "requireReviewRequest": true, "enableSelfReview": true, "labels": ["file-review"], "labelMode": "all"}},
      "fixer": {"discovery": {"enabled": false, "includeDrafts": true, "authorFilter": "any", "labels": ["file-fix"], "labelMode": "all"}}
    }
  }
}`
	env := map[string]string{
		"LOOPER_ROLES_PLANNER_AUTO_DISCOVERY":                             "true",
		"LOOPER_ROLES_PLANNER_TRIGGERS_LABELS":                            "env-plan",
		"LOOPER_ROLES_PLANNER_TRIGGERS_LABEL_MODE":                        "any",
		"LOOPER_ROLES_PLANNER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER":     "false",
		"LOOPER_ROLES_WORKER_AUTO_DISCOVERY":                              "true",
		"LOOPER_ROLES_WORKER_TRIGGERS_LABELS":                             "env-work",
		"LOOPER_ROLES_WORKER_TRIGGERS_LABEL_MODE":                         "any",
		"LOOPER_ROLES_WORKER_TRIGGERS_REQUIRE_ASSIGNEE_CURRENT_USER":      "false",
		"LOOPER_ROLES_REVIEWER_DISCOVERY_AUTO_DISCOVERY":                  "true",
		"LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_INCLUDE_DRAFTS":         "false",
		"LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_REQUIRE_REVIEW_REQUEST": "false",
		"LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_ENABLE_SELF_REVIEW":     "false",
		"LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABELS":                 "env-review",
		"LOOPER_ROLES_REVIEWER_DISCOVERY_TRIGGERS_LABEL_MODE":             "any",
		"LOOPER_ROLES_FIXER_AUTO_DISCOVERY":                               "true",
		"LOOPER_ROLES_FIXER_TRIGGERS_INCLUDE_DRAFTS":                      "false",
		"LOOPER_ROLES_FIXER_TRIGGERS_LABELS":                              "env-fix",
		"LOOPER_ROLES_FIXER_TRIGGERS_LABEL_MODE":                          "any",
		"LOOPER_ROLES_FIXER_TRIGGERS_AUTHOR_FILTER":                       "current_user",
	}
	assertDiscovery := func(t *testing.T, cfg Config, wantFixerAuthor AuthorFilter) {
		t.Helper()
		planner := cfg.Roles.Coding[CodingRolePlanner].Discovery
		if !planner.Enabled || !reflect.DeepEqual(planner.Labels, []string{"env-plan"}) || planner.LabelMode != LabelModeAny || planner.RequireAssigneeCurrentUser {
			t.Fatalf("planner discovery = %#v", planner)
		}
		worker := cfg.Roles.Coding[CodingRoleWorker].Discovery
		if !worker.Enabled || !reflect.DeepEqual(worker.Labels, []string{"env-work"}) || worker.LabelMode != LabelModeAny || worker.RequireAssigneeCurrentUser {
			t.Fatalf("worker discovery = %#v", worker)
		}
		reviewer := cfg.Roles.Coding[CodingRoleReviewer].Discovery
		if !reviewer.Enabled || reviewer.IncludeDrafts || reviewer.RequireReviewRequest || reviewer.EnableSelfReview || !reflect.DeepEqual(reviewer.Labels, []string{"env-review"}) || reviewer.LabelMode != LabelModeAny {
			t.Fatalf("reviewer discovery = %#v", reviewer)
		}
		fixer := cfg.Roles.Coding[CodingRoleFixer].Discovery
		if !fixer.Enabled || fixer.IncludeDrafts || fixer.AuthorFilter != wantFixerAuthor || !reflect.DeepEqual(fixer.Labels, []string{"env-fix"}) || fixer.LabelMode != LabelModeAny {
			t.Fatalf("fixer discovery = %#v", fixer)
		}
	}

	t.Run("environment wins over file coding config", func(t *testing.T) {
		loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, fileConfig, env, nil)
		assertDiscovery(t, loaded.Config, AuthorFilterCurrentUser)
	})
	t.Run("CLI wins over environment and file coding config", func(t *testing.T) {
		loaded := loadConfigFromJSONWithEnvAndArgsFixture(t, fileConfig, env, []string{"--roles-fixer-triggers-author-filter=any"})
		assertDiscovery(t, loaded.Config, AuthorFilterAny)
	})
}
