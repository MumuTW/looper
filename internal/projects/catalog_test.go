package projects

import (
	"strings"
	"sync"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

var _ ConfigSource = (*Catalog)(nil)

func TestCatalogPublishesFullConfigAtomically(t *testing.T) {
	t.Parallel()

	vendor := config.AgentVendorCodex
	global := config.Config{
		Agent:     config.AgentConfig{Vendor: &vendor, Params: map[string]any{"nested": map[string]any{"value": "original"}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}},
		Projects:  []config.ProjectRefConfig{{ID: "import-input"}},
	}
	catalog := NewCatalog(global)
	catalog.Publish([]config.ProjectRefConfig{{ID: "database", Repo: "core/database", Roles: &config.PartialRoleConfigs{}}})

	got := catalog.Snapshot()
	if len(got.Projects) != 1 || got.Projects[0].ID != "database" {
		t.Fatalf("Snapshot().Projects = %#v, want published database project", got.Projects)
	}
	if len(got.Providers) != 1 || got.Providers[0].ID != "forgejo-main" || got.Agent.Vendor == nil || *got.Agent.Vendor != vendor {
		t.Fatalf("Snapshot() lost global config: %#v", got)
	}
}

func TestCatalogPublishGlobalsPreservesMaterializedProjects(t *testing.T) {
	t.Parallel()

	oldModel := "old"
	newModel := "new"
	catalog := NewCatalog(config.Config{
		Agent:    config.AgentConfig{Model: &oldModel},
		Projects: []config.ProjectRefConfig{{ID: "import-input"}},
	})
	catalog.Publish([]config.ProjectRefConfig{{ID: "database", Repo: "core/database"}})

	candidate := config.Config{
		Agent:    config.AgentConfig{Model: &newModel, Env: map[string]string{"TOKEN": "original"}},
		Projects: []config.ProjectRefConfig{{ID: "new-import-input"}},
	}
	catalog.PublishGlobals(candidate)
	candidate.Agent.Env["TOKEN"] = "caller-mutated"

	got := catalog.Snapshot()
	if got.Agent.Model == nil || *got.Agent.Model != newModel || got.Agent.Env["TOKEN"] != "original" {
		t.Fatalf("Snapshot().Agent = %#v, want detached new globals", got.Agent)
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "database" {
		t.Fatalf("Snapshot().Projects = %#v, want preserved database view", got.Projects)
	}
}

func TestCatalogPreservesDetachedCanonicalCodingRegistryAcrossPublications(t *testing.T) {
	t.Parallel()

	worker := config.CodingRoleConfig{
		Instructions: "canonical worker guidance",
		Discovery: config.RoleDiscoveryConfig{
			Enabled: true,
			Labels:  []string{"canonical"},
		},
	}
	initial := config.Config{Roles: config.RoleConfigs{Coding: map[string]config.CodingRoleConfig{
		config.CodingRoleWorker: worker,
	}}}
	catalog := NewCatalog(initial)
	initial.Roles.Coding[config.CodingRoleWorker] = config.CodingRoleConfig{}
	catalog.Publish([]config.ProjectRefConfig{{ID: "database", Repo: "core/database"}})

	snapshot := catalog.Snapshot()
	got, ok := snapshot.Roles.Coding[config.CodingRoleWorker]
	if !ok || got.Instructions != worker.Instructions || len(got.Discovery.Labels) != 1 || got.Discovery.Labels[0] != "canonical" {
		t.Fatalf("Snapshot().Roles.Coding[worker] = %#v, want canonical detached role", got)
	}
	snapshot.Roles.Coding[config.CodingRoleWorker] = config.CodingRoleConfig{}

	reloaded := config.CloneConfig(catalog.Snapshot())
	reloaded.Roles.Coding[config.CodingRoleWorker] = config.CodingRoleConfig{
		Instructions: "reloaded worker guidance",
		Discovery:    config.RoleDiscoveryConfig{Enabled: true, Labels: []string{"reloaded"}},
	}
	catalog.PublishGlobals(reloaded)
	reloaded.Roles.Coding[config.CodingRoleWorker] = config.CodingRoleConfig{}

	got = catalog.Snapshot().Roles.Coding[config.CodingRoleWorker]
	if got.Instructions != "reloaded worker guidance" || len(got.Discovery.Labels) != 1 || got.Discovery.Labels[0] != "reloaded" {
		t.Fatalf("reloaded Snapshot().Roles.Coding[worker] = %#v", got)
	}
}

func TestCatalogPublishPreservesReloadedGlobals(t *testing.T) {
	t.Parallel()

	model := "reloaded"
	catalog := NewCatalog(config.Config{})
	catalog.PublishGlobals(config.Config{Agent: config.AgentConfig{Model: &model}})
	catalog.Publish([]config.ProjectRefConfig{{ID: "database"}})

	got := catalog.Snapshot()
	if got.Agent.Model == nil || *got.Agent.Model != model {
		t.Fatalf("Snapshot().Agent.Model = %#v, want reloaded globals", got.Agent.Model)
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "database" {
		t.Fatalf("Snapshot().Projects = %#v, want database view", got.Projects)
	}
}

func TestCatalogDoesNotRetainOrReturnMutableAliases(t *testing.T) {
	t.Parallel()

	roles := &config.PartialRoleConfigs{}
	projects := []config.ProjectRefConfig{{ID: "database", Repo: "core/database", Roles: roles}}
	global := config.Config{
		Agent:     config.AgentConfig{Params: map[string]any{"nested": map[string]any{"value": "original"}}, Env: map[string]string{"TOKEN": "env"}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}},
	}
	catalog := NewCatalog(global)
	catalog.Publish(projects)

	projects[0].ID = "caller-mutated"
	projects[0].Roles = nil
	global.Providers[0].ID = "caller-mutated"
	global.Agent.Env["TOKEN"] = "caller-mutated"

	first := catalog.Snapshot()
	first.Projects[0].ID = "snapshot-mutated"
	first.Projects[0].Roles = nil
	first.Providers[0].ID = "snapshot-mutated"
	first.Agent.Env["TOKEN"] = "snapshot-mutated"
	first.Agent.Params["nested"].(map[string]any)["value"] = "snapshot-mutated"

	got := catalog.Snapshot()
	if got.Projects[0].ID != "database" || got.Projects[0].Roles == nil {
		t.Fatalf("published projects were mutated through an alias: %#v", got.Projects)
	}
	if got.Providers[0].ID != "forgejo-main" || got.Agent.Env["TOKEN"] != "env" {
		t.Fatalf("published globals were mutated through an alias: %#v", got)
	}
	nested := got.Agent.Params["nested"].(map[string]any)
	if nested["value"] != "original" {
		t.Fatalf("Snapshot().Agent.Params nested value = %v, want original", nested["value"])
	}
}

func TestCatalogConcurrentSnapshotsObserveWholePublications(t *testing.T) {
	t.Parallel()

	catalog := NewCatalog(config.Config{})
	catalog.Publish([]config.ProjectRefConfig{{ID: "a", Name: "a"}})

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				catalog.Publish([]config.ProjectRefConfig{{ID: "a", Name: "a"}})
			} else {
				catalog.Publish([]config.ProjectRefConfig{{ID: "b", Name: "b"}, {ID: "b2", Name: "b2"}})
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			snapshot := catalog.Snapshot()
			switch len(snapshot.Projects) {
			case 1:
				if snapshot.Projects[0].ID != "a" || snapshot.Projects[0].Name != "a" {
					t.Errorf("observed torn a publication: %#v", snapshot.Projects)
					return
				}
			case 2:
				if snapshot.Projects[0].ID != "b" || snapshot.Projects[0].Name != "b" || snapshot.Projects[1].ID != "b2" || snapshot.Projects[1].Name != "b2" {
					t.Errorf("observed torn b publication: %#v", snapshot.Projects)
					return
				}
			default:
				t.Errorf("observed unexpected publication: %#v", snapshot.Projects)
				return
			}
		}
	}()
	wg.Wait()
}

func TestCatalogConcurrentGlobalAndProjectPublicationsPreserveBothAuthorities(t *testing.T) {
	t.Parallel()

	catalog := NewCatalog(config.Config{})
	const iterations = 300
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for index := 0; index < iterations; index++ {
			model := "global-a"
			if index == iterations-1 {
				model = "global-final"
			}
			catalog.PublishGlobals(config.Config{
				Agent:    config.AgentConfig{Model: &model},
				Projects: []config.ProjectRefConfig{{ID: "must-never-publish"}},
			})
		}
	}()
	go func() {
		defer wg.Done()
		for index := 0; index < iterations; index++ {
			projectID := "database-a"
			if index == iterations-1 {
				projectID = "database-final"
			}
			catalog.Publish([]config.ProjectRefConfig{{ID: projectID}})
		}
	}()
	wg.Wait()

	got := catalog.Snapshot()
	if got.Agent.Model == nil || *got.Agent.Model != "global-final" {
		t.Fatalf("Snapshot().Agent.Model = %#v, want final global publication", got.Agent.Model)
	}
	if len(got.Projects) != 1 || got.Projects[0].ID != "database-final" {
		t.Fatalf("Snapshot().Projects = %#v, want final database publication", got.Projects)
	}
}

func TestMaterializeCatalogUsesRecordsAsProjectAuthority(t *testing.T) {
	t.Parallel()

	baseBranch := "main"
	metadata := `{"network":{"mode":"routed"},"provider":"forgejo-main","repo":"core/odcrew","source":"config","worktreeRoot":"/tmp/worktrees"}`
	archivedMetadata := `{"repo":"acme/removed","source":"config"}`
	imported := config.Config{Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}, Projects: []config.ProjectRefConfig{
		{ID: "odcrew", Name: "stale name", RepoPath: "/stale", Provider: "stale-provider", Repo: "stale/repo"},
		{ID: "config-only", Name: "must not appear", RepoPath: "/config-only"},
	}}

	got, err := MaterializeCatalog(imported, []storage.ProjectRecord{
		{ID: "odcrew", Name: "ODCrew", RepoPath: "/repos/odcrew", BaseBranch: &baseBranch, MetadataJSON: &metadata},
		{ID: "removed", Name: "Removed", RepoPath: "/repos/removed", Archived: true, MetadataJSON: &archivedMetadata},
	})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(MaterializeCatalog()) = %d, want 1", len(got))
	}
	project := got[0]
	if project.ID != "odcrew" || project.Name != "ODCrew" || project.RepoPath != "/repos/odcrew" {
		t.Fatalf("project identity = %#v, want database record", project)
	}
	if project.Provider != "forgejo-main" || project.Repo != "core/odcrew" {
		t.Fatalf("project binding = (%q, %q), want stored binding", project.Provider, project.Repo)
	}
	if project.Network.Mode != config.NetworkModeRouted {
		t.Fatalf("project policy network mode = %q, want imported routed policy", project.Network.Mode)
	}
}

func TestMaterializeCatalogDoesNotApplyConfigPolicyToAPIRecord(t *testing.T) {
	t.Parallel()

	metadata := `{"repo":"acme/api","source":"api"}`
	imported := config.Config{Projects: []config.ProjectRefConfig{{
		ID: "api", Network: config.ProjectNetworkConfig{Mode: config.NetworkModeRouted},
	}}}
	got, err := MaterializeCatalog(imported, []storage.ProjectRecord{{ID: "api", Name: "API", RepoPath: "/repos/api", MetadataJSON: &metadata}})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(got) != 1 || got[0].Network.Mode != "" {
		t.Fatalf("MaterializeCatalog() = %#v, want record without unstored imported policy", got)
	}
}

func TestMaterializeCatalogRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	metadata := `{"provider":"removed","repo":"core/odcrew","source":"api"}`
	_, err := MaterializeCatalog(config.Config{}, []storage.ProjectRecord{{ID: "odcrew", MetadataJSON: &metadata}})
	if err == nil || !strings.Contains(err.Error(), `unknown provider "removed"`) {
		t.Fatalf("MaterializeCatalog() error = %v, want unknown provider", err)
	}
}

func TestMaterializeCatalogAllowsDuplicateReposAcrossProviders(t *testing.T) {
	t.Parallel()

	githubMetadata := `{"repo":"nexu-io/looper","source":"config"}`
	forgejoMetadata := `{"provider":"forgejo-main","repo":"NEXU-IO/LOOPER","source":"api"}`
	global := config.Config{Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}}

	got, err := MaterializeCatalog(global, []storage.ProjectRecord{
		{ID: "github", MetadataJSON: &githubMetadata},
		{ID: "forgejo", MetadataJSON: &forgejoMetadata},
	})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("MaterializeCatalog() projects = %d, want 2", len(got))
	}
}

func TestMaterializeCatalogRejectsDuplicateRepoWithinProvider(t *testing.T) {
	t.Parallel()

	first := `{"provider":"forgejo-main","repo":"nexu-io/looper"}`
	second := `{"provider":"forgejo-main","repo":"NEXU-IO/LOOPER"}`
	global := config.Config{Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.test"}}}
	_, err := MaterializeCatalog(global, []storage.ProjectRecord{{ID: "one", MetadataJSON: &first}, {ID: "two", MetadataJSON: &second}})
	if err == nil || !strings.Contains(err.Error(), `duplicates active project "one"`) {
		t.Fatalf("MaterializeCatalog() error = %v, want same-provider duplicate rejection", err)
	}
}

func TestMaterializeCatalogAppliesAndValidatesForgejoRoleProfile(t *testing.T) {
	t.Parallel()

	global, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	global.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}
	global.Roles.Coordinator.Enabled = true
	global.Roles.Coordinator.Dependencies.Enabled = true
	metadata := `{"provider":"forgejo-main","repo":"core/odcrew","source":"api"}`
	got, err := MaterializeCatalog(global, []storage.ProjectRecord{{ID: "odcrew", MetadataJSON: &metadata}})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	global.Projects = got
	triggers := config.ProjectRoleConfigs(global, "odcrew").Reviewer.Discovery.Triggers
	if !triggers.RequireReviewRequest || len(triggers.Labels) != 0 {
		t.Fatalf("materialized reviewer triggers = %#v, want native Forgejo review-request profile", triggers)
	}
	coordinator := config.ProjectRoleConfigs(global, "odcrew").Coordinator
	if coordinator.Enabled || coordinator.Dependencies.Enabled {
		t.Fatalf("materialized coordinator = %#v, want Forgejo coordinator and dependency gates disabled", coordinator)
	}

	incompatibleMetadata := `{"provider":"forgejo-main","repo":"core/odcrew","roles":{"reviewer":{"autoMerge":{"enabled":true}}},"source":"api"}`
	_, err = MaterializeCatalog(global, []storage.ProjectRecord{{ID: "odcrew", MetadataJSON: &incompatibleMetadata}})
	if err == nil || !strings.Contains(err.Error(), "autoMerge.enabled") {
		t.Fatalf("MaterializeCatalog() error = %v, want incompatible Forgejo role rejection", err)
	}
}

func TestConfiguredProjectMetadataRoundTripsRuntimePolicy(t *testing.T) {
	t.Parallel()

	project := config.ProjectRefConfig{
		ID:       "odcrew",
		Name:     "ODCrew",
		Provider: "forgejo-main",
		Repo:     "core/odcrew",
		RepoPath: "/repos/odcrew",
		Path:     "nested/path",
		Network:  config.ProjectNetworkConfig{Mode: config.NetworkModeRouted},
		Webhook:  config.ProjectWebhookConfig{Mode: config.WebhookModeTunnel},
		Roles:    &config.PartialRoleConfigs{},
	}
	repo := project.Repo
	metadata, err := buildProjectMetadataJSON(nil, project, &repo)
	if err != nil {
		t.Fatalf("buildProjectMetadataJSON() error = %v", err)
	}
	global := config.Config{Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}}}
	got, err := MaterializeCatalog(global, []storage.ProjectRecord{{
		ID: project.ID, Name: project.Name, RepoPath: project.RepoPath, MetadataJSON: &metadata,
	}})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(MaterializeCatalog()) = %d, want 1", len(got))
	}
	materialized := got[0]
	if materialized.Provider != project.Provider || materialized.Repo != project.Repo || materialized.Path != project.Path {
		t.Fatalf("materialized binding = %#v, want %#v", materialized, project)
	}
	if materialized.Network.Mode != project.Network.Mode || materialized.Webhook.Mode != project.Webhook.Mode || materialized.Roles == nil {
		t.Fatalf("materialized policy = %#v, want persisted project policy", materialized)
	}
}

func TestCatalogViewCapturesCoherentGeneration(t *testing.T) {
	t.Parallel()

	vendor := config.AgentVendorCodex
	cfg := config.Config{
		Agent:     config.AgentConfig{Vendor: &vendor, Params: map[string]any{"nested": map[string]any{"value": "original"}}},
		Providers: []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo}},
		Projects:  []config.ProjectRefConfig{{ID: "import-input"}},
	}
	catalog := NewCatalog(cfg)
	catalog.Publish([]config.ProjectRefConfig{{ID: "database", Repo: "core/database", Provider: "forgejo-main", Roles: &config.PartialRoleConfigs{}}})

	view := catalog.View()

	catalog.Publish([]config.ProjectRefConfig{{ID: "replaced"}})
	newModel := "replaced"
	catalog.PublishGlobals(config.Config{Agent: config.AgentConfig{Model: &newModel}})

	project, ok := view.Project("database")
	if !ok || project.Project.ID != "database" {
		t.Fatalf("view.Project(\"database\") = (%#v, %v), want database project", project, ok)
	}
	if project.Provider.ID != "forgejo-main" {
		t.Fatalf("view.Project(\"database\").Provider = %#v, want forgejo-main", project.Provider)
	}

	rolePolicy := view.RolePolicy("database")
	if rolePolicy.ProjectID != "database" {
		t.Fatalf("view.RolePolicy(\"database\").ProjectID = %q, want database", rolePolicy.ProjectID)
	}

	if view.generation.Agent.Model != nil && *view.generation.Agent.Model == "replaced" {
		t.Fatalf("view observed a later global publication")
	}
	if view.generation.Agent.Params["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("view generation was mutated by publication")
	}
}

func TestCatalogViewProjectsAreDetachedAndImmutable(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Projects: []config.ProjectRefConfig{{
			ID: "demo", Repo: "nexu-io/looper", Roles: &config.PartialRoleConfigs{
				Reviewer: &config.PartialReviewerRoleConfig{Discovery: &config.PartialReviewerRoleDiscoveryConfig{AutoDiscovery: boolPtr(true)}},
			},
		}},
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{AutoDiscovery: true},
			},
		},
	}
	catalog := NewCatalog(cfg)

	view := catalog.View()
	project, ok := view.Project("demo")
	if !ok {
		t.Fatal("view.Project(\"demo\") not found")
	}
	project.Project.Repo = "mutated/repo"
	project.Roles.Reviewer.Discovery.AutoDiscovery = false
	project.Roles.Reviewer.Discovery.Triggers.Labels = []string{"mutated"}

	got, ok := catalog.View().Project("demo")
	if !ok {
		t.Fatal("catalog.View().Project(\"demo\") not found")
	}
	if got.Project.Repo != "nexu-io/looper" {
		t.Fatalf("view Project.Repo = %q, want original", got.Project.Repo)
	}
	if !got.Roles.Reviewer.Discovery.AutoDiscovery {
		t.Fatalf("view role policy was mutated through returned view")
	}
	if len(got.Roles.Reviewer.Discovery.Triggers.Labels) != 0 {
		t.Fatalf("view role labels were mutated through returned view")
	}
}

func TestCatalogViewRolePolicyMergesProjectOverrides(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Projects: []config.ProjectRefConfig{{
			ID: "demo", Repo: "nexu-io/looper", Roles: &config.PartialRoleConfigs{
				Reviewer: &config.PartialReviewerRoleConfig{Discovery: &config.PartialReviewerRoleDiscoveryConfig{
					AutoDiscovery: boolPtr(false),
					Triggers: &config.PartialReviewerRoleTriggersConfig{
						Labels: &[]string{"project-review"},
					},
				}},
			},
		}},
		Roles: config.RoleConfigs{
			Reviewer: config.ReviewerRoleConfig{
				Discovery: config.ReviewerRoleDiscoveryConfig{
					AutoDiscovery: true,
					Triggers: config.ReviewerRoleTriggersConfig{
						Labels:        []string{"review"},
						LabelMode:     config.LabelModeAll,
						IncludeDrafts: true,
					},
				},
			},
		},
	}
	catalog := NewCatalog(cfg)

	view := catalog.View()
	policy := view.RolePolicy("demo")
	if policy.Roles.Reviewer.Discovery.AutoDiscovery {
		t.Fatalf("project role override was not applied: autoDiscovery = true")
	}
	if len(policy.Roles.Reviewer.Discovery.Triggers.Labels) != 1 || policy.Roles.Reviewer.Discovery.Triggers.Labels[0] != "project-review" {
		t.Fatalf("project role labels = %v, want project override", policy.Roles.Reviewer.Discovery.Triggers.Labels)
	}
	if policy.Roles.Reviewer.Discovery.Triggers.LabelMode != config.LabelModeAll {
		t.Fatalf("global role field was not inherited: labelMode = %q", policy.Roles.Reviewer.Discovery.Triggers.LabelMode)
	}
	if !policy.Roles.Reviewer.Discovery.Triggers.IncludeDrafts {
		t.Fatalf("global role field was not inherited: includeDrafts = false")
	}
}

func TestCatalogViewProviderPolicy(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Providers: []config.ProviderConfig{{
			ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.test",
		}},
		Projects: []config.ProjectRefConfig{{
			ID: "demo", Repo: "NEXU-IO/LOOPER", Provider: "forgejo-main",
		}},
	}
	catalog := NewCatalog(cfg)

	view := catalog.View()
	policy, ok := view.ProviderPolicy("demo")
	if !ok {
		t.Fatal("view.ProviderPolicy(\"demo\") not found")
	}
	if policy.Provider.ID != "forgejo-main" || policy.ProviderKind != config.ProviderKindForgejo {
		t.Fatalf("view.ProviderPolicy(\"demo\") = %#v, want forgejo-main", policy)
	}
	if policy.Provider.BaseURL != "https://code.example.test" {
		t.Fatalf("provider base url was not preserved")
	}
}

func TestCatalogViewProviderPolicyRequiresRepositoryBinding(t *testing.T) {
	t.Parallel()

	catalog := NewCatalog(config.Config{
		Projects: []config.ProjectRefConfig{{ID: "demo", Repo: "  "}},
	})

	policy, ok := catalog.View().ProviderPolicy("demo")
	if ok || policy != (ProviderPolicyView{}) {
		t.Fatalf("view.ProviderPolicy(\"demo\") = (%#v, %v), want empty policy, false", policy, ok)
	}
}

func TestCatalogViewProviderByRemoteHost(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		Providers: []config.ProviderConfig{{
			ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://code.example.test",
		}},
	}
	catalog := NewCatalog(cfg)

	view := catalog.View()
	policy, ok := view.ProviderByRemoteHost("ssh.code.example.test")
	if !ok {
		t.Fatal("view.ProviderByRemoteHost(\"ssh.code.example.test\") not found")
	}
	if policy.Provider.ID != "forgejo-main" || policy.ProviderKind != config.ProviderKindForgejo {
		t.Fatalf("view.ProviderByRemoteHost = %#v, want forgejo-main", policy)
	}
}

func TestOperationViewFromConfigDetachesCodingRolePolicy(t *testing.T) {
	t.Parallel()

	profile := "fast"
	input := config.Config{Roles: config.RoleConfigs{Coding: map[string]config.CodingRoleConfig{
		"auditor": {
			Discovery: config.RoleDiscoveryConfig{Labels: []string{"audit"}},
			Agent:     &config.RoleAgentConfig{Profile: &profile},
		},
	}}}
	view := OperationViewFromConfig(input)

	input.Roles.Coding["auditor"].Discovery.Labels[0] = "mutated-input"
	*input.Roles.Coding["auditor"].Agent.Profile = "mutated-input"
	first := view.RolePolicy("")
	first.Roles.Coding["auditor"].Discovery.Labels[0] = "mutated-view"
	*first.Roles.Coding["auditor"].Agent.Profile = "mutated-view"

	got := view.RolePolicy("").Roles.Coding["auditor"]
	if len(got.Discovery.Labels) != 1 || got.Discovery.Labels[0] != "audit" {
		t.Fatalf("coding role labels = %v, want detached audit label", got.Discovery.Labels)
	}
	if got.Agent == nil || got.Agent.Profile == nil || *got.Agent.Profile != "fast" {
		t.Fatalf("coding role agent = %#v, want detached fast profile", got.Agent)
	}

	policy := RolePolicyView{Roles: config.RoleConfigs{
		Coding: map[string]config.CodingRoleConfig{
			config.CodingRoleWorker: {Discovery: config.RoleDiscoveryConfig{Enabled: true}},
		},
		Worker: config.WorkerRoleConfig{AutoDiscovery: false},
	}}
	if !policy.RoleAutoDiscovery(config.CodingRoleWorker) {
		t.Fatal("RoleAutoDiscovery(worker) ignored canonical coding registry")
	}
}

func boolPtr(v bool) *bool { return &v }
