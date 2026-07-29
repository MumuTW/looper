package config

import "testing"

func TestValidateDirectCanonicalRegistryContracts(t *testing.T) {
	t.Parallel()
	base := mustNormalize(t)
	cases := []struct {
		name   string
		mutate func(*Config)
		path   string
	}{
		{
			name: "priority", path: "roles.coding.worker.priority",
			mutate: func(cfg *Config) {
				role := cfg.Roles.Coding[CodingRoleWorker]
				role.Priority = 0
				cfg.Roles.Coding[CodingRoleWorker] = role
			},
		},
		{
			name: "gatekeeper", path: "roles.coding.gatekeeper",
			mutate: func(cfg *Config) {
				role := cfg.Roles.Coding[RoleGatekeeper]
				role.Priority--
				cfg.Roles.Coding[RoleGatekeeper] = role
			},
		},
		{
			name: "inert", path: "roles.coding.auditor",
			mutate: func(cfg *Config) {
				cfg.Roles.Coding["auditor"] = CodingRoleConfig{Priority: 60, Discovery: RoleDiscoveryConfig{Source: WorkSourceIssue, LabelMode: LabelModeAll}}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			cfg.Roles.Coding = cloneCodingRoleRegistry(base.Roles.Coding)
			tc.mutate(&cfg)
			if err := Validate(cfg); !hasIssuePath(normalizeIssuePaths(err), tc.path) {
				t.Fatalf("issues = %v, want %s (err = %v)", normalizeIssuePaths(err), tc.path, err)
			}
		})
	}
}

func TestValidateRejectsProjectScopedCodingRolesFromDirectConfig(t *testing.T) {
	t.Parallel()
	cfg := mustNormalize(t)
	cfg.Projects = []ProjectRefConfig{{
		ID: "demo", Name: "Demo", RepoPath: t.TempDir(),
		Roles: &PartialRoleConfigs{Coding: map[string]PartialCodingRoleConfig{
			CodingRoleWorker: {Priority: intPtr(60)},
		}},
	}}
	if err := Validate(cfg); !hasIssuePath(normalizeIssuePaths(err), "projects[0].roles.coding") {
		t.Fatalf("issues = %v", normalizeIssuePaths(err))
	}
}

func TestProjectLegacyOverridesRemainVisibleThroughRegistry(t *testing.T) {
	t.Parallel()
	falseValue := false
	cfg := mustNormalize(t, PartialConfig{Projects: &[]PartialProjectRefConfig{{
		ID: "demo", Name: "Demo", RepoPath: t.TempDir(),
		Roles: &PartialRoleConfigs{Worker: &PartialWorkerRoleConfig{
			AutoDiscovery: &falseValue,
			Instructions:  stringPtr("project guidance"),
		}},
	}}})
	worker, ok := ProjectCodingRoleConfig(cfg, "demo", CodingRoleWorker)
	if !ok || worker.Discovery.Enabled || worker.Instructions != "project guidance" {
		t.Fatalf("project worker registry = %#v, %v", worker, ok)
	}
}

func TestCloneConfigPreservesDetachedCanonicalRegistry(t *testing.T) {
	t.Parallel()
	cfg := mustNormalize(t, mustDecodeTOML(t, `
[roles.coding.worker]
instructions = "canonical clone guidance"
[roles.coding.worker.discovery]
labels = ["canonical"]
`))
	clone := CloneConfig(cfg)
	worker := clone.Roles.Coding[CodingRoleWorker]
	if worker.Instructions != "canonical clone guidance" || len(worker.Discovery.Labels) != 1 || worker.Discovery.Labels[0] != "canonical" {
		t.Fatalf("cloned worker registry = %#v", worker)
	}
	worker.Discovery.Labels[0] = "mutated"
	clone.Roles.Coding[CodingRoleWorker] = worker
	if cfg.Roles.Coding[CodingRoleWorker].Discovery.Labels[0] != "canonical" {
		t.Fatal("CloneConfig() retained an aliased coding registry")
	}
}
