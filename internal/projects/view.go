package projects

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// OperationView is an operation-scoped, immutable projection of one coherent
// configuration generation. It is captured from a Catalog once per operation and
// exposes narrow views for Project, Role policy, and Provider policy without
// returning the full configuration to callers.
//
// All values returned from an OperationView are detached from the Catalog. A
// view holds one generation for its lifetime, so projections made from it are
// coherent and cannot observe a later publication.
type OperationView struct {
	generation config.Config
}

// View returns an OperationView for the currently published generation.
func (c *Catalog) View() OperationView {
	return OperationView{generation: c.Snapshot()}
}

// OperationViewFromConfig returns an OperationView from an already-detached
// configuration value. It is useful for tests and focused consumers that build
// policy input without a live Catalog.
func OperationViewFromConfig(cfg config.Config) OperationView {
	return OperationView{generation: cloneCatalogConfig(cfg)}
}

// ProjectView is an operation-scoped, detached view of a single project and its
// effective role and provider policy.
type ProjectView struct {
	Project  config.ProjectRefConfig
	Roles    config.RoleConfigs
	Provider config.ProviderConfig
	Identity config.RepositoryIdentity
}

// RolePolicyView is an operation-scoped, detached view of the effective role
// configuration for a project. Unknown project IDs fall back to global roles.
type RolePolicyView struct {
	ProjectID string
	Roles     config.RoleConfigs
}

// RoleAutoDiscovery reports whether the named role has discovery enabled in this
// role policy view. Supported roles are planner, worker, reviewer, fixer, and
// coordinator.
func (r RolePolicyView) RoleAutoDiscovery(role string) bool {
	return roleAutoDiscovery(r.Roles, role)
}

// AnyProjectRoleAutoDiscovery reports whether any project (or the global role
// policy) has the named role's discovery enabled.
func (v OperationView) AnyProjectRoleAutoDiscovery(role string) bool {
	if roleAutoDiscovery(config.ProjectRoleConfigs(v.generation, ""), role) {
		return true
	}
	for _, project := range v.generation.Projects {
		if roleAutoDiscovery(config.ProjectRoleConfigs(v.generation, project.ID), role) {
			return true
		}
	}
	return false
}

func roleAutoDiscovery(roles config.RoleConfigs, role string) bool {
	role = strings.TrimSpace(role)
	if coding, ok := roles.Coding[role]; ok {
		return coding.Discovery.Enabled
	}
	switch role {
	case "coordinator":
		return roles.Coordinator.Enabled
	case "planner":
		return roles.Planner.AutoDiscovery
	case "reviewer":
		return roles.Reviewer.Discovery.AutoDiscovery
	case "fixer":
		return roles.Fixer.AutoDiscovery
	case "worker":
		return roles.Worker.AutoDiscovery
	default:
		return false
	}
}

// Project returns the ProjectView for projectID. The second result is true when
// the project is part of the published generation, false otherwise. The role
// policy and provider fields are always populated, using global fallbacks when
// the project is unknown.
func (v OperationView) Project(projectID string) (ProjectView, bool) {
	var project config.ProjectRefConfig
	found := false
	for i := range v.generation.Projects {
		if v.generation.Projects[i].ID == projectID {
			project = v.generation.Projects[i]
			found = true
			break
		}
	}
	project = cloneProjectRefConfig(project)

	provider, _ := providerByID(v.generation, project.Provider)
	identity, _ := config.ProjectRepositoryIdentity(v.generation, project)

	roles := config.ProjectRoleConfigs(v.generation, projectID)

	return ProjectView{
		Project:  project,
		Roles:    cloneRoleConfigs(roles),
		Provider: cloneProviderConfig(provider),
		Identity: identity,
	}, found
}

// RolePolicy returns the effective role configuration for projectID. Unknown
// project IDs fall back to the generation's global role configuration.
func (v OperationView) RolePolicy(projectID string) RolePolicyView {
	roles := config.ProjectRoleConfigs(v.generation, projectID)
	return RolePolicyView{
		ProjectID: projectID,
		Roles:     cloneRoleConfigs(roles),
	}
}

func providerByID(cfg config.Config, providerID string) (config.ProviderConfig, bool) {
	providerID = strings.TrimSpace(providerID)
	for _, provider := range cfg.Providers {
		if provider.ID == providerID {
			return provider, true
		}
	}
	return config.ProviderConfig{}, false
}

func cloneProjectRefConfig(source config.ProjectRefConfig) config.ProjectRefConfig {
	encoded, err := json.Marshal(source)
	if err != nil {
		panic(fmt.Sprintf("clone project view: %v", err))
	}
	var cloned config.ProjectRefConfig
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		panic(fmt.Sprintf("clone project view: %v", err))
	}
	return cloned
}

func cloneProviderConfig(source config.ProviderConfig) config.ProviderConfig {
	encoded, err := json.Marshal(source)
	if err != nil {
		panic(fmt.Sprintf("clone provider view: %v", err))
	}
	var cloned config.ProviderConfig
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		panic(fmt.Sprintf("clone provider view: %v", err))
	}
	return cloned
}

func cloneRoleConfigs(source config.RoleConfigs) config.RoleConfigs {
	encoded, err := json.Marshal(source)
	if err != nil {
		panic(fmt.Sprintf("clone role policy view: %v", err))
	}
	var cloned config.RoleConfigs
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		panic(fmt.Sprintf("clone role policy view: %v", err))
	}
	if len(source.Coding) > 0 {
		cloned.Coding = make(map[string]config.CodingRoleConfig, len(source.Coding))
		for name, role := range source.Coding {
			cloned.Coding[name] = cloneCodingRoleConfig(role)
		}
	}
	return cloned
}

func cloneCodingRoleConfig(source config.CodingRoleConfig) config.CodingRoleConfig {
	cloned := source
	cloned.Discovery.Labels = append([]string(nil), source.Discovery.Labels...)
	if source.Agent != nil {
		agent := config.RoleAgentConfig{}
		if source.Agent.Profile != nil {
			profile := *source.Agent.Profile
			agent.Profile = &profile
		}
		if source.Agent.Vendor != nil {
			vendor := *source.Agent.Vendor
			agent.Vendor = &vendor
		}
		if source.Agent.Model != nil {
			model := *source.Agent.Model
			agent.Model = &model
		}
		cloned.Agent = &agent
	}
	return cloned
}
