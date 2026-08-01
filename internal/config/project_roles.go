package config

// ProjectRoleConfigs returns the effective role configuration for a project.
// Global role configuration is used as the base and projects[id].roles may
// override supported role fields. Unknown project IDs fall back to global roles.
//
// Project-level agent bindings are never applied (ADR-0012): agent vendor/model
// resolution is global-only. Even if a project partial carries agent fields in
// memory, they are stripped before merge.
func ProjectRoleConfigs(cfg Config, projectID string) RoleConfigs {
	roles := cfg.Roles
	if roles.Planner.Escalation != nil {
		cloned := *roles.Planner.Escalation
		roles.Planner.Escalation = &cloned
	}
	project := findConfiguredProject(cfg.Projects, projectID)
	if project == nil {
		return roles
	}
	if project.Roles != nil {
		stripped := stripRoleAgentBindings(*project.Roles)
		mergeRoleConfigs(&roles, stripped)
		roles.Coding = projectCodingRoleConfigs(cfg.Roles, stripped)
	}
	return roles
}

// projectCodingRoleConfigs preserves legacy project role overrides after the
// global registry became the runtime authority. projects[].roles.coding is
// rejected at load time, so this translates only the existing compatible
// named overrides onto a copy of the global registry.
func projectCodingRoleConfigs(global RoleConfigs, partial PartialRoleConfigs) map[string]CodingRoleConfig {
	registry := cloneCodingRoleRegistry(EffectiveCodingRoles(global))
	for name, override := range legacyCodingRoleOverrides(partial) {
		base, ok := registry[name]
		if !ok {
			continue
		}
		registry[name] = applyPartialCodingRoleConfig(base, override)
	}
	return registry
}

func cloneCodingRoleRegistry(registry map[string]CodingRoleConfig) map[string]CodingRoleConfig {
	cloned := make(map[string]CodingRoleConfig, len(registry))
	for name, role := range registry {
		cloned[name] = cloneCodingRoleConfig(role)
	}
	return cloned
}

// legacyCodingRoleOverrides translates the shared fields of legacy named role
// sections into registry overlays. Agent bindings reflect the supplied input;
// project callers pass stripRoleAgentBindings output, so their global-only
// bindings remain excluded.
func legacyCodingRoleOverrides(roles PartialRoleConfigs) map[string]PartialCodingRoleConfig {
	overrides := make(map[string]PartialCodingRoleConfig, 4)
	if roles.Planner != nil {
		overrides[CodingRolePlanner] = PartialCodingRoleConfig{
			Instructions: roles.Planner.Instructions,
			Agent:        roles.Planner.Agent,
			Discovery:    partialIssueCodingDiscovery(roles.Planner.AutoDiscovery, roles.Planner.Triggers),
		}
	}
	if roles.Worker != nil {
		overrides[CodingRoleWorker] = PartialCodingRoleConfig{
			Instructions: roles.Worker.Instructions,
			Agent:        roles.Worker.Agent,
			Discovery:    partialIssueCodingDiscovery(roles.Worker.AutoDiscovery, roles.Worker.Triggers),
		}
	}
	if roles.Reviewer != nil {
		partial := roles.Reviewer
		overrides[CodingRoleReviewer] = PartialCodingRoleConfig{
			Instructions: partial.Instructions,
			Agent:        partial.Agent,
			Discovery:    partialReviewerCodingDiscovery(partial.Discovery),
		}
	}
	if roles.Fixer != nil {
		overrides[CodingRoleFixer] = PartialCodingRoleConfig{
			Instructions: roles.Fixer.Instructions,
			Agent:        roles.Fixer.Agent,
			Discovery:    partialFixerCodingDiscovery(roles.Fixer.AutoDiscovery, roles.Fixer.Triggers),
		}
	}
	return overrides
}

func partialIssueCodingDiscovery(enabled *bool, triggers *PartialIssueRoleTriggersConfig) *PartialRoleDiscoveryConfig {
	if enabled == nil && triggers == nil {
		return nil
	}
	discovery := &PartialRoleDiscoveryConfig{Enabled: enabled}
	if triggers != nil {
		discovery.Labels = triggers.Labels
		discovery.LabelMode = triggers.LabelMode
		discovery.RequireAssigneeCurrentUser = triggers.RequireAssigneeCurrentUser
	}
	return discovery
}

func partialReviewerCodingDiscovery(discovery *PartialReviewerRoleDiscoveryConfig) *PartialRoleDiscoveryConfig {
	if discovery == nil {
		return nil
	}
	result := &PartialRoleDiscoveryConfig{Enabled: discovery.AutoDiscovery}
	if discovery.Triggers != nil {
		result.IncludeDrafts = discovery.Triggers.IncludeDrafts
		result.RequireReviewRequest = discovery.Triggers.RequireReviewRequest
		result.EnableSelfReview = discovery.Triggers.EnableSelfReview
		result.Labels = discovery.Triggers.Labels
		result.LabelMode = discovery.Triggers.LabelMode
	}
	return result
}

func partialFixerCodingDiscovery(enabled *bool, triggers *PartialFixerRoleTriggersConfig) *PartialRoleDiscoveryConfig {
	if enabled == nil && triggers == nil {
		return nil
	}
	discovery := &PartialRoleDiscoveryConfig{Enabled: enabled}
	if triggers != nil {
		discovery.IncludeDrafts = triggers.IncludeDrafts
		if triggers.AuthorFilter != nil {
			filter := AuthorFilter(*triggers.AuthorFilter)
			discovery.AuthorFilter = &filter
		}
		discovery.Labels = triggers.Labels
		discovery.LabelMode = triggers.LabelMode
	}
	return discovery
}

// stripRoleAgentBindings returns a copy of partial with Agent nilled on coding roles.
func stripRoleAgentBindings(partial PartialRoleConfigs) PartialRoleConfigs {
	stripped := partial
	if partial.Planner != nil {
		planner := *partial.Planner
		planner.Agent = nil
		stripped.Planner = &planner
	}
	if partial.Worker != nil {
		worker := *partial.Worker
		worker.Agent = nil
		stripped.Worker = &worker
	}
	if partial.Reviewer != nil {
		reviewer := *partial.Reviewer
		reviewer.Agent = nil
		stripped.Reviewer = &reviewer
	}
	if partial.Fixer != nil {
		fixer := *partial.Fixer
		fixer.Agent = nil
		stripped.Fixer = &fixer
	}
	return stripped
}

// ProjectCodingRoleConfig returns the effective canonical registry entry for a
// project, including compatible project-level legacy overrides.
func ProjectCodingRoleConfig(cfg Config, projectID, role string) (CodingRoleConfig, bool) {
	roles := ProjectRoleConfigs(cfg, projectID)
	entry, ok := EffectiveCodingRoles(roles)[role]
	return entry, ok && isCodingRole(role)
}

func ProjectRoleAutoDiscoveryEnabled(cfg Config, projectID, role string) bool {
	roles := ProjectRoleConfigs(cfg, projectID)
	if coding, ok := EffectiveCodingRoles(roles)[role]; ok && isCodingRole(role) {
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

// MarkReadyReviewerUnreachableProjects names the projects where coordinator
// mark-ready is enabled but the local reviewer can never claim what it
// publishes.
//
// Taking a draft out of draft emits ready_for_review, which wakes the reviewer
// lane — but the reviewer rejects a Pull Request authored by the account it
// runs as unless enableSelfReview is set, and GitHub will not let anyone
// request review from a Pull Request's own author. So in a single-daemon
// configuration with the default enableSelfReview = false, mark-ready publishes
// drafts that nothing then reviews. The repair is either a distinct reviewer
// identity (a routed reviewer runs under its own login and is unaffected) or
// enableSelfReview, and neither is something to change silently on the
// operator's behalf.
func MarkReadyReviewerUnreachableProjects(cfg Config) []string {
	unreachable := []string(nil)
	for _, project := range cfg.Projects {
		roles := ProjectRoleConfigs(cfg, project.ID)
		if !roles.Coordinator.MarkReady.Enabled {
			continue
		}
		reviewer, ok := ProjectCodingRoleConfig(cfg, project.ID, CodingRoleReviewer)
		canClaimWithoutRequest := ok && reviewer.Discovery.Enabled && reviewer.Discovery.EnableSelfReview &&
			!reviewer.Discovery.RequireReviewRequest && len(reviewer.Discovery.Labels) == 0
		if !ok || canClaimWithoutRequest {
			continue
		}
		unreachable = append(unreachable, project.ID)
	}
	return unreachable
}
