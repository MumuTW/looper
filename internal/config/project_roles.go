package config

// ProjectRoleConfigs returns the effective role configuration for a project.
// Global role configuration is used as the base and projects[id].roles may
// override supported role fields. Unknown project IDs fall back to global roles.
func ProjectRoleConfigs(cfg Config, projectID string) RoleConfigs {
	roles := cfg.Roles
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil {
		return roles
	}
	if project.Roles != nil {
		mergeRoleConfigs(&roles, *project.Roles)
	}
	return roles
}

// ProjectProviderKind resolves the task-source provider kind for a project by
// id (github / forgejo / plane). Unknown ids fall back to the GitHub default.
func ProjectProviderKind(cfg Config, projectID string) ProviderKind {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil {
		return ProviderKindGitHub
	}
	return resolvedProjectProviderKind(cfg, *project)
}

// ProjectProductOwner resolves a project's product owner (plan §8.3) — who to
// @-mention when a feature lands without a product spec. Unknown ids return an
// empty owner (no one to ping).
func ProjectProductOwner(cfg Config, projectID string) ProductOwnerConfig {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil || project.ProductOwner == nil {
		return ProductOwnerConfig{}
	}
	return *project.ProductOwner
}

// ProjectDesignOwner resolves the project's design-decision authority. PlaneID is
// used for answers; FeishuOpenID targets the notification only.
func ProjectDesignOwner(cfg Config, projectID string) FeishuActorConfig {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil || project.DesignOwner == nil {
		return FeishuActorConfig{}
	}
	return *project.DesignOwner
}

// ProjectQA resolves a project's QA validator open_id — who the shepherd @-mentions
// when a PR carries `needs-validation` and isn't `validated` yet. Project-level.
// Unknown ids / unset config return an empty open_id (no one to ping).
func ProjectQA(cfg Config, projectID string) string {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil || project.QA == nil {
		return ""
	}
	return project.QA.FeishuOpenID
}

// ProjectOwner resolves this deploy's looper owner open_id — who the shepherd
// @-mentions to merge once a PR is approved, green, clean and (if needed) validated.
// Per-deploy config. Unknown ids / unset config return an empty open_id.
func ProjectOwner(cfg Config, projectID string) string {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil || project.Owner == nil {
		return ""
	}
	return project.Owner.FeishuOpenID
}

// ProjectOwnerActor resolves both identities for this deploy's owner. Keep
// ProjectOwner for callers that only need the Feishu open_id.
func ProjectOwnerActor(cfg Config, projectID string) FeishuActorConfig {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil || project.Owner == nil {
		return FeishuActorConfig{}
	}
	return *project.Owner
}

func ProjectRoleAutoDiscoveryEnabled(cfg Config, projectID, role string) bool {
	roles := ProjectRoleConfigs(cfg, projectID)
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

func AnyProjectRoleAutoDiscoveryEnabled(cfg Config, role string) bool {
	if ProjectRoleAutoDiscoveryEnabled(cfg, "", role) {
		return true
	}
	for _, project := range cfg.Projects {
		if ProjectRoleAutoDiscoveryEnabled(cfg, project.ID, role) {
			return true
		}
	}
	return false
}
