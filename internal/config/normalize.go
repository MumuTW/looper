package config

import (
	"fmt"
	"net/url"
	"strings"
)

func Normalize(cwd string, partials ...PartialConfig) (Config, error) {
	config, err := DefaultConfig(cwd)
	if err != nil {
		return Config{}, err
	}
	defaultCodingRoles := CodingRolesFromLegacy(config.Roles)

	normalizedLayers := make([]PartialConfig, 0, len(partials))
	for _, partial := range partials {
		if issues := validateLegacyProjectInstructionRoleKeys(partial); len(issues) > 0 {
			return Config{}, &ConfigValidationError{Issues: issues}
		}
		if issues := validateProjectCodingRoleSections(partial); len(issues) > 0 {
			return Config{}, &ConfigValidationError{Issues: issues}
		}
		if issues := validateDeprecatedGatekeeperTrust(partial); len(issues) > 0 {
			return Config{}, &ConfigValidationError{Issues: issues}
		}
		normalized := normalizeLayerPartial(clonePartialConfig(partial))
		mergeConfig(&config, normalized)
		normalizedLayers = append(normalizedLayers, normalized)
	}

	// Build the canonical role registry last from its default projection plus
	// every layer's role fields. Within a layer, legacy named fields seed the
	// shared role settings and roles.coding.* wins. Across layers, the normal
	// config precedence holds: a later legacy env or CLI override must be able
	// to replace an earlier file-backed roles.coding.* value.
	authored, authoredInstructions, codingModelCanonical, issues := collectAuthoredCodingRoles(normalizedLayers...)
	if len(issues) > 0 {
		return Config{}, &ConfigValidationError{Issues: issues}
	}
	codingRoles, issues := resolveCodingRoles(defaultCodingRoles, authored)
	if len(issues) > 0 {
		return Config{}, &ConfigValidationError{Issues: issues}
	}
	for name := range authoredInstructions {
		validateCustomCodingRoleInstruction("roles.coding."+name+".instructions", codingRoles[name].Instructions, 0, &issues)
	}
	if len(issues) > 0 {
		return Config{}, &ConfigValidationError{Issues: issues}
	}
	config.Roles.Coding = codingRoles
	config.Roles.codingModelCanonical = codingModelCanonical

	// Store server.baseUrl in its canonical form so every consumer reads the
	// same validated representation. Invalid values are kept verbatim for
	// Validate to report.
	if config.Server.BaseURL != nil {
		if canonical, err := CanonicalizeServerBaseURL(*config.Server.BaseURL); err == nil {
			config.Server.BaseURL = &canonical
		}
	}

	return config, nil
}

// validateDeprecatedGatekeeperTrust keeps the parse-only compatibility field
// strict about the values that were historically loadable.  The field has no
// runtime authority, but silently accepting "auto" would make an unsupported
// merge authority look like a valid configuration and then fall back to
// observe-only behavior.
func validateDeprecatedGatekeeperTrust(partial PartialConfig) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	validate := func(value *string, path string) {
		if value == nil {
			return
		}
		switch strings.ToLower(strings.TrimSpace(*value)) {
		case "observe", "advise":
		default:
			issues = append(issues, ValidationIssue{Path: path, Message: "must be one of: observe, advise"})
		}
	}
	if partial.Roles != nil {
		validateDeprecatedGatekeeperRoleTrust(partial.Roles.Gatekeeper, "roles.gatekeeper.trust", validate)
	}
	if partial.Projects != nil {
		for index, project := range *partial.Projects {
			if project.Roles != nil {
				validateDeprecatedGatekeeperRoleTrust(project.Roles.Gatekeeper, fmt.Sprintf("projects[%d].roles.gatekeeper.trust", index), validate)
			}
		}
	}
	return issues
}

func validateDeprecatedGatekeeperRoleTrust(role *DeprecatedGatekeeperRoleConfig, path string, validate func(*string, string)) {
	if role == nil {
		return
	}
	validate(role.Trust, path)
}

// validateProjectCodingRoleSections rejects roles.coding.* under a project:
// the coding-role registry is global, so a project-scoped section would be
// configured but inert. Project overrides of the shipped roles keep using the
// legacy named sections (projects[].roles.planner and friends).
func validateProjectCodingRoleSections(partial PartialConfig) []ValidationIssue {
	if partial.Projects == nil {
		return nil
	}

	issues := make([]ValidationIssue, 0)
	for index, project := range *partial.Projects {
		validateProjectCodingRoleOverrides(project.Roles, fmt.Sprintf("projects[%d].roles", index), &issues)
	}

	return issues
}

func validateProjectCodingRoleOverrides(roles *PartialRoleConfigs, prefix string, issues *[]ValidationIssue) {
	if roles == nil {
		return
	}
	if len(roles.Coding) > 0 {
		*issues = append(*issues, ValidationIssue{
			Path:    prefix + ".coding",
			Message: "coding roles are global-only; author roles.coding.* at the top level",
		})
	}
	if roles.Escalator != nil {
		*issues = append(*issues, ValidationIssue{
			Path:    prefix + ".escalator",
			Message: "escalator is global-only because one digest aggregates all active projects; author roles.escalator at the top level",
		})
	}
}

func CanonicalizePartialForMigration(partial PartialConfig) PartialConfig {
	normalized := normalizeLayerPartial(clonePartialConfig(partial))
	normalized.LegacyReviewer = nil
	if normalized.Agent != nil && normalized.Agent.Timeouts != nil {
		normalized.Agent.Timeouts.PlannerSeconds = nil
		normalized.Agent.Timeouts.WorkerSeconds = nil
		normalized.Agent.Timeouts.ReviewerSeconds = nil
		normalized.Agent.Timeouts.FixerSeconds = nil
	}

	if normalized.Defaults != nil {
		normalized.Defaults.AllowAutoApprove = nil
		normalized.Defaults.FixAllPullRequests = nil
	}

	if normalized.Roles != nil && normalized.Roles.Reviewer != nil {
		normalized.Roles.Reviewer.AutoDiscovery = nil
		normalized.Roles.Reviewer.Triggers = nil
		normalized.Roles.Reviewer.SpecReview = nil
	}

	if normalized.Projects != nil {
		projects := *normalized.Projects
		for i := range projects {
			projects[i].Path = ""
			projects[i].Instructions = nil
			if projects[i].Roles != nil && projects[i].Roles.Reviewer != nil {
				projects[i].Roles.Reviewer.AutoDiscovery = nil
				projects[i].Roles.Reviewer.Triggers = nil
				projects[i].Roles.Reviewer.SpecReview = nil
			}
		}
		normalized.Projects = &projects
	}

	return normalized
}

func normalizeLayerPartial(partial PartialConfig) PartialConfig {
	normalized := partial
	if normalized.Agent != nil && normalized.Agent.Timeouts != nil {
		agent := *normalized.Agent
		timeouts := *agent.Timeouts
		agent.Timeouts = &timeouts
		normalized.Agent = &agent
		if timeouts.PlannerMaxRuntimeSeconds == nil {
			timeouts.PlannerMaxRuntimeSeconds = timeouts.PlannerSeconds
		}
		if timeouts.WorkerMaxRuntimeSeconds == nil {
			timeouts.WorkerMaxRuntimeSeconds = timeouts.WorkerSeconds
		}
		if timeouts.ReviewerMaxRuntimeSeconds == nil {
			timeouts.ReviewerMaxRuntimeSeconds = timeouts.ReviewerSeconds
		}
		if timeouts.FixerMaxRuntimeSeconds == nil {
			timeouts.FixerMaxRuntimeSeconds = timeouts.FixerSeconds
		}
	}

	if normalized.LegacyReviewer != nil {
		reviewer := ensureReviewerRoleConfig(&normalized)
		reviewer.Behavior = mergePartialReviewerConfigWithCanonicalPriority(reviewer.Behavior, normalized.LegacyReviewer)
	}

	if normalized.Roles != nil && normalized.Roles.Reviewer != nil {
		normalizeReviewerRoleLegacyShape(normalized.Roles.Reviewer)
	}
	canonicalizePartialRoleAgentBindings(normalized.Roles)
	if normalized.Projects != nil {
		projects := *normalized.Projects
		for i := range projects {
			if projects[i].Provider != nil {
				provider := strings.TrimSpace(*projects[i].Provider)
				projects[i].Provider = &provider
			}
			if projects[i].Repo != nil {
				repo := strings.TrimSpace(*projects[i].Repo)
				projects[i].Repo = &repo
			}
			if projects[i].RepoPath == "" {
				projects[i].RepoPath = projects[i].Path
			}
			projects[i].Roles = mergeLegacyProjectInstructionsIntoRoles(projects[i].Roles, projects[i].Instructions)
			if projects[i].Roles != nil && projects[i].Roles.Reviewer != nil {
				normalizeReviewerRoleLegacyShape(projects[i].Roles.Reviewer)
			}
			canonicalizePartialRoleAgentBindings(projects[i].Roles)
		}
		normalized.Projects = &projects
	}
	if normalized.Providers != nil {
		providers := cloneProviderConfigs(*normalized.Providers)
		partials := make([]PartialProviderConfig, len(providers))
		for i, provider := range providers {
			partials[i] = PartialProviderConfig{
				ID:       provider.ID,
				Kind:     &provider.Kind,
				BaseURL:  &provider.BaseURL,
				GHPath:   provider.GHPath,
				TokenEnv: provider.TokenEnv,
			}
		}
		normalized.Providers = &partials
	}

	if normalized.Defaults != nil {
		if normalized.Defaults.AllowAutoApprove != nil {
			reviewEvents := ensureReviewerReviewEventsConfig(&normalized)
			if reviewEvents.Clean == nil {
				event := ReviewerReviewEventComment
				if *normalized.Defaults.AllowAutoApprove {
					event = ReviewerReviewEventApprove
				}
				reviewEvents.Clean = &event
			}
		}
		if normalized.Defaults.FixAllPullRequests != nil {
			triggers := ensureFixerRoleTriggersConfig(&normalized)
			if triggers.AuthorFilter == nil {
				authorFilter := FixerAuthorFilterCurrentUser
				if *normalized.Defaults.FixAllPullRequests {
					authorFilter = FixerAuthorFilterAny
				}
				triggers.AuthorFilter = &authorFilter
			}
		}
	}

	return normalized
}

func validateLegacyProjectInstructionRoleKeys(partial PartialConfig) []ValidationIssue {
	if partial.Projects == nil {
		return nil
	}

	issues := make([]ValidationIssue, 0)
	for index, project := range *partial.Projects {
		for role := range project.Instructions {
			if isValidInstructionRole(role) || role == "sweeper" {
				continue
			}
			issues = append(issues, ValidationIssue{
				Path:    fmt.Sprintf("projects[%d].instructions.%s", index, role),
				Message: "role must be one of: planner, worker, reviewer, fixer",
			})
		}
	}

	return issues
}

func normalizeReviewerRoleLegacyShape(reviewer *PartialReviewerRoleConfig) {
	if reviewer == nil {
		return
	}
	reviewer.Discovery = mergePartialReviewerRoleDiscoveryWithCanonicalPriority(reviewer.Discovery, reviewer.AutoDiscovery, reviewer.Triggers, reviewer.SpecReview)
}

func mergePartialReviewerConfigWithCanonicalPriority(canonical *PartialReviewerConfig, legacy *PartialReviewerConfig) *PartialReviewerConfig {
	if canonical == nil {
		return legacy
	}
	if legacy == nil {
		return canonical
	}
	if canonical.Loop == nil {
		canonical.Loop = legacy.Loop
	}
	if canonical.Retry == nil {
		canonical.Retry = legacy.Retry
	}
	if canonical.Scope == nil {
		canonical.Scope = legacy.Scope
	}
	if canonical.PublishMode == nil {
		canonical.PublishMode = legacy.PublishMode
	}
	if canonical.ReviewEvents == nil {
		canonical.ReviewEvents = legacy.ReviewEvents
	} else if legacy.ReviewEvents != nil {
		if canonical.ReviewEvents.Clean == nil {
			canonical.ReviewEvents.Clean = legacy.ReviewEvents.Clean
		}
		if canonical.ReviewEvents.Blocking == nil {
			canonical.ReviewEvents.Blocking = legacy.ReviewEvents.Blocking
		}
		if canonical.ReviewEvents.Clean == nil && canonical.ReviewEvents.Blocking == nil {
			canonical.ReviewEvents = legacy.ReviewEvents
		}
	}
	if canonical.DetectDuplicateFindings == nil {
		canonical.DetectDuplicateFindings = legacy.DetectDuplicateFindings
	}
	if canonical.DedupeFindings == nil {
		canonical.DedupeFindings = legacy.DedupeFindings
	}
	if canonical.NativeResume == nil {
		canonical.NativeResume = legacy.NativeResume
	}
	if canonical.ThreadResolution == nil {
		canonical.ThreadResolution = legacy.ThreadResolution
	}
	return canonical
}

func mergePartialReviewerRoleDiscoveryWithCanonicalPriority(canonical *PartialReviewerRoleDiscoveryConfig, legacyAutoDiscovery *bool, legacyTriggers *PartialReviewerRoleTriggersConfig, legacySpecReview *PartialReviewerSpecReviewConfig) *PartialReviewerRoleDiscoveryConfig {
	if canonical == nil && legacyAutoDiscovery == nil && legacyTriggers == nil && legacySpecReview == nil {
		return nil
	}
	if canonical == nil {
		canonical = &PartialReviewerRoleDiscoveryConfig{}
	}
	if canonical.AutoDiscovery == nil {
		canonical.AutoDiscovery = legacyAutoDiscovery
	}
	if canonical.Triggers == nil {
		canonical.Triggers = legacyTriggers
	} else if legacyTriggers != nil {
		if canonical.Triggers.IncludeDrafts == nil {
			canonical.Triggers.IncludeDrafts = legacyTriggers.IncludeDrafts
		}
		if canonical.Triggers.RequireReviewRequest == nil {
			canonical.Triggers.RequireReviewRequest = legacyTriggers.RequireReviewRequest
		}
		if canonical.Triggers.EnableSelfReview == nil {
			canonical.Triggers.EnableSelfReview = legacyTriggers.EnableSelfReview
		}
		if canonical.Triggers.Labels == nil {
			canonical.Triggers.Labels = legacyTriggers.Labels
		}
		if canonical.Triggers.LabelMode == nil {
			canonical.Triggers.LabelMode = legacyTriggers.LabelMode
		}
	}
	if canonical.SpecReview == nil {
		canonical.SpecReview = legacySpecReview
	} else if legacySpecReview != nil {
		if canonical.SpecReview.IncludeReviewingLabel == nil {
			canonical.SpecReview.IncludeReviewingLabel = legacySpecReview.IncludeReviewingLabel
		}
		if canonical.SpecReview.ReviewingLabel == nil {
			canonical.SpecReview.ReviewingLabel = legacySpecReview.ReviewingLabel
		}
	}
	return canonical
}

func mergeConfig(config *Config, partial PartialConfig) {
	if partial.Server != nil {
		mergeServerConfig(&config.Server, *partial.Server)
	}

	if partial.Storage != nil {
		mergeStorageConfig(&config.Storage, *partial.Storage)
	}

	if partial.Scheduler != nil {
		mergeSchedulerConfig(&config.Scheduler, *partial.Scheduler)
	}

	if partial.Webhook != nil {
		mergeWebhookConfig(&config.Webhook, *partial.Webhook)
	}

	if partial.Agent != nil {
		mergeAgentConfig(&config.Agent, *partial.Agent)
	}

	if partial.Logging != nil {
		mergeLoggingConfig(&config.Logging, *partial.Logging)
	}

	if partial.Notifications != nil {
		mergeNotificationConfig(&config.Notifications, *partial.Notifications)
	}

	if partial.Disclosure != nil {
		mergeDisclosureConfig(&config.Disclosure, *partial.Disclosure)
	}

	if partial.Tools != nil {
		mergeToolPathsConfig(&config.Tools, *partial.Tools)
	}

	if partial.Daemon != nil {
		mergeDaemonConfig(&config.Daemon, *partial.Daemon)
	}

	if partial.Package != nil {
		mergePackageConfig(&config.Package, *partial.Package)
	}

	if partial.Defaults != nil {
		mergeDefaultsConfig(&config.Defaults, *partial.Defaults)
	}

	if partial.LegacyReviewer != nil {
		mergeReviewerConfig(&config.Roles.Reviewer.Behavior, *partial.LegacyReviewer)
	}

	if partial.Instructions != nil {
		mergeInstructionsConfig(&config.Instructions, *partial.Instructions)
	}

	if partial.HITL != nil {
		if partial.HITL.Enabled != nil {
			config.HITL.Enabled = *partial.HITL.Enabled
		}
		if partial.HITL.AnswerTransport != nil {
			config.HITL.AnswerTransport = strings.TrimSpace(*partial.HITL.AnswerTransport)
		}
		if gh := partial.HITL.GitHub; gh != nil {
			if config.HITL.GitHub == nil {
				config.HITL.GitHub = &HITLGitHubConfig{}
			}
			if gh.AwaitingLabel != nil {
				config.HITL.GitHub.AwaitingLabel = strings.TrimSpace(*gh.AwaitingLabel)
			}
			if gh.MentionLogins != nil {
				config.HITL.GitHub.MentionLogins = append([]string(nil), (*gh.MentionLogins)...)
			}
			if gh.AnswerAuthors != nil {
				config.HITL.GitHub.AnswerAuthors = append([]string(nil), (*gh.AnswerAuthors)...)
			}
		}
		if fs := partial.HITL.Feishu; fs != nil {
			if config.HITL.Feishu == nil {
				config.HITL.Feishu = &HITLFeishuConfig{}
			}
			if fs.Inbound != nil {
				config.HITL.Feishu.Inbound = strings.TrimSpace(*fs.Inbound)
			}
			if fs.EventInboxURLEnv != nil {
				config.HITL.Feishu.EventInboxURLEnv = strings.TrimSpace(*fs.EventInboxURLEnv)
			}
			if fs.EventInboxTokenEnv != nil {
				config.HITL.Feishu.EventInboxTokenEnv = strings.TrimSpace(*fs.EventInboxTokenEnv)
			}
		}
	}

	if partial.Intake != nil {
		if tg := partial.Intake.Telegram; tg != nil {
			if config.Intake.Telegram == nil {
				config.Intake.Telegram = &TelegramIntakeConfig{}
			}
			if tg.Enabled != nil {
				config.Intake.Telegram.Enabled = *tg.Enabled
			}
			if tg.BotTokenEnv != nil {
				config.Intake.Telegram.BotTokenEnv = strings.TrimSpace(*tg.BotTokenEnv)
			}
			if tg.AllowedUserIDs != nil {
				config.Intake.Telegram.AllowedUserIDs = append([]int64(nil), (*tg.AllowedUserIDs)...)
			}
			if tg.DefaultProjectID != nil {
				config.Intake.Telegram.DefaultProjectID = strings.TrimSpace(*tg.DefaultProjectID)
			}
		}
	}

	if partial.Roles != nil {
		mergeRoleConfigs(&config.Roles, *partial.Roles)
	}

	if partial.Projects != nil {
		config.Projects = cloneProjects(*partial.Projects)
	}

	if partial.Providers != nil {
		config.Providers = cloneProviderConfigs(*partial.Providers)
	}
}

func resolvedProjectProviderKind(config Config, project ProjectRefConfig) ProviderKind {
	providerID := strings.TrimSpace(project.Provider)
	if providerID == "" {
		return ProviderKindGitHub
	}
	for _, provider := range config.Providers {
		if provider.ID == providerID {
			return provider.Kind
		}
	}
	return ""
}

func ResolvedProjectProviderKind(config Config, project ProjectRefConfig) ProviderKind {
	return resolvedProjectProviderKind(config, project)
}

func normalizeProviderConfig(provider *ProviderConfig) {
	provider.ID = strings.TrimSpace(provider.ID)
	provider.BaseURL = normalizeBaseURL(provider.BaseURL)
	if provider.GHPath != nil {
		provider.GHPath = stringPtr(strings.TrimSpace(*provider.GHPath))
	}
	if provider.TokenEnv != nil {
		provider.TokenEnv = stringPtr(strings.TrimSpace(*provider.TokenEnv))
	}
}

func normalizeBaseURL(value string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return trimmed
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

func mergeServerConfig(config *ServerConfig, partial PartialServerConfig) {
	if partial.Host != nil {
		config.Host = *partial.Host
	}

	if partial.Port != nil {
		config.Port = *partial.Port
	}

	if partial.BaseURL != nil {
		config.BaseURL = stringPtr(*partial.BaseURL)
	}

	if partial.AuthMode != nil {
		config.AuthMode = *partial.AuthMode
	}

	if partial.LocalToken != nil {
		config.LocalToken = stringPtr(*partial.LocalToken)
	}
}

func mergeStorageConfig(config *StorageConfig, partial PartialStorageConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}

	if partial.DBPath != nil {
		config.DBPath = *partial.DBPath
	}

	if partial.BackupDir != nil {
		config.BackupDir = stringPtr(*partial.BackupDir)
	}
}

func mergeSchedulerConfig(config *SchedulerConfig, partial PartialSchedulerConfig) {
	if partial.PollIntervalSeconds != nil {
		config.PollIntervalSeconds = *partial.PollIntervalSeconds
	}

	if partial.MaxConcurrentRuns != nil {
		config.MaxConcurrentRuns = *partial.MaxConcurrentRuns
	}

	if partial.RetryMaxAttempts != nil {
		config.RetryMaxAttempts = *partial.RetryMaxAttempts
	}

	if partial.ConsecutiveFailureThreshold != nil {
		config.ConsecutiveFailureThreshold = *partial.ConsecutiveFailureThreshold
	}

	if partial.RetryBaseDelayMS != nil {
		config.RetryBaseDelayMS = *partial.RetryBaseDelayMS
	}

	if partial.SlowLaneWarnThresholdMS != nil {
		config.SlowLaneWarnThresholdMS = *partial.SlowLaneWarnThresholdMS
	}

	if partial.DiscoveryCacheTTLSeconds != nil {
		config.DiscoveryCacheTTLSeconds = *partial.DiscoveryCacheTTLSeconds
	}
}

func mergeWebhookConfig(config *WebhookConfig, partial PartialWebhookConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}

	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}

	if partial.ListenPort != nil {
		config.ListenPort = *partial.ListenPort
	}

	if partial.PublicBaseURL != nil {
		config.PublicBaseURL = *partial.PublicBaseURL
	}

	if partial.FallbackPollIntervalSeconds != nil {
		config.FallbackPollIntervalSeconds = *partial.FallbackPollIntervalSeconds
	}
}

func mergeAgentConfig(config *AgentConfig, partial PartialAgentConfig) {
	if partial.Vendor != nil {
		vendor := *partial.Vendor
		config.Vendor = &vendor
	}

	if partial.Model != nil {
		config.Model = stringPtr(*partial.Model)
	}
	if partial.ReasoningEffort != nil {
		config.ReasoningEffort = normalizeReasoningEffortPtr(partial.ReasoningEffort)
	}

	if partial.Profiles != nil {
		config.Profiles = mergeAgentProfiles(config.Profiles, partial.Profiles)
	}

	if partial.Params != nil {
		config.Params = mergeAnyMap(config.Params, partial.Params)
	}

	if partial.Env != nil {
		config.Env = mergeStringMap(config.Env, partial.Env)
	}

	if partial.Timeouts != nil {
		mergeAgentTimeoutConfig(&config.Timeouts, *partial.Timeouts)
	}

	if partial.NativeResume != nil && partial.NativeResume.Enabled != nil {
		config.NativeResume.Enabled = *partial.NativeResume.Enabled
	}
}

func mergeAgentProfiles(base map[string]AgentBindingConfig, override map[string]AgentBindingConfig) map[string]AgentBindingConfig {
	if override == nil {
		return base
	}
	merged := make(map[string]AgentBindingConfig, len(base)+len(override))
	for id, binding := range base {
		merged[id] = cloneAgentBindingConfig(binding)
	}
	for id, binding := range override {
		existing := merged[id]
		if binding.Vendor != nil {
			vendor := *binding.Vendor
			existing.Vendor = &vendor
		}
		if binding.Model != nil {
			existing.Model = stringPtr(*binding.Model)
		}
		if binding.ReasoningEffort != nil {
			existing.ReasoningEffort = normalizeReasoningEffortPtr(binding.ReasoningEffort)
		}
		merged[id] = existing
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func cloneAgentBindingConfig(binding AgentBindingConfig) AgentBindingConfig {
	cloned := AgentBindingConfig{}
	if binding.Vendor != nil {
		vendor := *binding.Vendor
		cloned.Vendor = &vendor
	}
	if binding.Model != nil {
		cloned.Model = stringPtr(*binding.Model)
	}
	if binding.ReasoningEffort != nil {
		effort := *binding.ReasoningEffort
		cloned.ReasoningEffort = &effort
	}
	return cloned
}

func mergeRoleAgentConfig(config **RoleAgentConfig, partial *RoleAgentConfig) {
	if partial == nil {
		return
	}
	if *config == nil {
		*config = &RoleAgentConfig{}
	}
	if partial.Profile != nil {
		(*config).Profile = stringPtr(*partial.Profile)
	}
	if partial.Vendor != nil {
		vendor := *partial.Vendor
		(*config).Vendor = &vendor
	}
	if partial.Model != nil {
		(*config).Model = stringPtr(*partial.Model)
	}
	if partial.ReasoningEffort != nil {
		(*config).ReasoningEffort = normalizeReasoningEffortPtr(partial.ReasoningEffort)
	}
	if isEmptyRoleAgentConfig(*config) {
		*config = nil
	}
}

// isEmptyRoleAgentConfig reports whether a role agent binding has no semantic fields.
// Empty/whitespace profile text is non-semantic; a non-nil empty model is intentional
// suppression and keeps the binding non-empty.
func isEmptyRoleAgentConfig(agent *RoleAgentConfig) bool {
	if agent == nil {
		return true
	}
	profileEmpty := agent.Profile == nil || strings.TrimSpace(*agent.Profile) == ""
	return profileEmpty && agent.Vendor == nil && agent.Model == nil && agent.ReasoningEffort == nil
}

// canonicalizePartialRoleAgentBindings nils empty agent objects on coding roles
// (including project role partials) so `{}` does not linger as a non-nil pointer.
func canonicalizePartialRoleAgentBindings(roles *PartialRoleConfigs) {
	if roles == nil {
		return
	}
	for name, role := range roles.Coding {
		if isEmptyRoleAgentConfig(role.Agent) {
			role.Agent = nil
			roles.Coding[name] = role
		}
	}
	if roles.Planner != nil && isEmptyRoleAgentConfig(roles.Planner.Agent) {
		roles.Planner.Agent = nil
	}
	if roles.Worker != nil && isEmptyRoleAgentConfig(roles.Worker.Agent) {
		roles.Worker.Agent = nil
	}
	if roles.Reviewer != nil && isEmptyRoleAgentConfig(roles.Reviewer.Agent) {
		roles.Reviewer.Agent = nil
	}
	if roles.Fixer != nil && isEmptyRoleAgentConfig(roles.Fixer.Agent) {
		roles.Fixer.Agent = nil
	}
}

func cloneRoleAgentConfig(agent *RoleAgentConfig) *RoleAgentConfig {
	if agent == nil {
		return nil
	}
	cloned := &RoleAgentConfig{
		Profile:         cloneStringPtr(agent.Profile),
		Model:           cloneStringPtr(agent.Model),
		ReasoningEffort: cloneReasoningEffortPtr(agent.ReasoningEffort),
	}
	if agent.Vendor != nil {
		vendor := *agent.Vendor
		cloned.Vendor = &vendor
	}
	return cloned
}

func cloneReasoningEffortPtr(effort *ReasoningEffort) *ReasoningEffort {
	if effort == nil {
		return nil
	}
	cloned := *effort
	return &cloned
}

func normalizeReasoningEffortPtr(effort *ReasoningEffort) *ReasoningEffort {
	if effort == nil {
		return nil
	}
	if canonical, ok := ParseReasoningEffort(string(*effort)); ok {
		return &canonical
	}
	// Preserve invalid values so validation can report the authored path rather
	// than silently dropping a configuration error during normalization.
	return cloneReasoningEffortPtr(effort)
}

func mergeAgentTimeoutConfig(config *AgentTimeoutConfig, partial PartialAgentTimeoutConfig) {
	if partial.PlannerSeconds != nil {
		config.PlannerSeconds = *partial.PlannerSeconds
		config.PlannerMaxRuntimeSeconds = *partial.PlannerSeconds
	}
	if partial.WorkerSeconds != nil {
		config.WorkerSeconds = *partial.WorkerSeconds
		config.WorkerMaxRuntimeSeconds = *partial.WorkerSeconds
	}
	if partial.ReviewerSeconds != nil {
		config.ReviewerSeconds = *partial.ReviewerSeconds
		config.ReviewerMaxRuntimeSeconds = *partial.ReviewerSeconds
	}
	if partial.FixerSeconds != nil {
		config.FixerSeconds = *partial.FixerSeconds
		config.FixerMaxRuntimeSeconds = *partial.FixerSeconds
	}
	if partial.PlannerIdleTimeoutSeconds != nil {
		config.PlannerIdleTimeoutSeconds = *partial.PlannerIdleTimeoutSeconds
	}
	if partial.PlannerMaxRuntimeSeconds != nil {
		config.PlannerMaxRuntimeSeconds = *partial.PlannerMaxRuntimeSeconds
		config.PlannerSeconds = *partial.PlannerMaxRuntimeSeconds
	}
	if partial.WorkerIdleTimeoutSeconds != nil {
		config.WorkerIdleTimeoutSeconds = *partial.WorkerIdleTimeoutSeconds
	}
	if partial.WorkerMaxRuntimeSeconds != nil {
		config.WorkerMaxRuntimeSeconds = *partial.WorkerMaxRuntimeSeconds
		config.WorkerSeconds = *partial.WorkerMaxRuntimeSeconds
	}
	if partial.ReviewerIdleTimeoutSeconds != nil {
		config.ReviewerIdleTimeoutSeconds = *partial.ReviewerIdleTimeoutSeconds
	}
	if partial.ReviewerMaxRuntimeSeconds != nil {
		config.ReviewerMaxRuntimeSeconds = *partial.ReviewerMaxRuntimeSeconds
		config.ReviewerSeconds = *partial.ReviewerMaxRuntimeSeconds
	}
	if partial.FixerIdleTimeoutSeconds != nil {
		config.FixerIdleTimeoutSeconds = *partial.FixerIdleTimeoutSeconds
	}
	if partial.FixerMaxRuntimeSeconds != nil {
		config.FixerMaxRuntimeSeconds = *partial.FixerMaxRuntimeSeconds
		config.FixerSeconds = *partial.FixerMaxRuntimeSeconds
	}
}

func mergeLoggingConfig(config *LoggingConfig, partial PartialLoggingConfig) {
	if partial.Level != nil {
		config.Level = *partial.Level
	}

	if partial.MaxSizeMB != nil {
		config.MaxSizeMB = *partial.MaxSizeMB
	}

	if partial.MaxFiles != nil {
		config.MaxFiles = *partial.MaxFiles
	}
}

func mergeNotificationConfig(config *NotificationConfig, partial PartialNotificationConfig) {
	if partial.InApp != nil {
		config.InApp = *partial.InApp
	}

	if partial.Osascript != nil {
		mergeOsascriptNotificationConfig(&config.Osascript, *partial.Osascript)
	}

	if partial.Webhook != nil {
		mergeWebhookNotificationConfig(&config.Webhook, *partial.Webhook)
	}
}

func mergeWebhookNotificationConfig(config *WebhookNotificationConfig, partial PartialWebhookNotificationConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}

	if partial.URLEnv != nil {
		config.URLEnv = *partial.URLEnv
	}

	if partial.Format != nil {
		config.Format = *partial.Format
	}

	if partial.Levels != nil {
		config.Levels = cloneSoundLevels(*partial.Levels)
	}

	if partial.ThrottleWindowSeconds != nil {
		config.ThrottleWindowSeconds = *partial.ThrottleWindowSeconds
	}

	if partial.Mode != nil {
		config.Mode = strings.TrimSpace(*partial.Mode)
	}

	if partial.AppIDEnv != nil {
		config.AppIDEnv = strings.TrimSpace(*partial.AppIDEnv)
	}

	if partial.AppSecretEnv != nil {
		config.AppSecretEnv = strings.TrimSpace(*partial.AppSecretEnv)
	}

	if partial.ChatID != nil {
		config.ChatID = strings.TrimSpace(*partial.ChatID)
	}

	if partial.VerificationTokenEnv != nil {
		config.VerificationTokenEnv = strings.TrimSpace(*partial.VerificationTokenEnv)
	}

	if partial.MentionOpenIds != nil {
		ids := make([]string, 0, len(*partial.MentionOpenIds))
		for _, id := range *partial.MentionOpenIds {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				ids = append(ids, trimmed)
			}
		}
		config.MentionOpenIds = ids
	}
}

func mergeOsascriptNotificationConfig(config *OsascriptNotificationConfig, partial PartialOsascriptNotificationConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}

	if partial.SoundForLevels != nil {
		config.SoundForLevels = cloneSoundLevels(*partial.SoundForLevels)
	}

	if partial.ThrottleWindowSeconds != nil {
		config.ThrottleWindowSeconds = *partial.ThrottleWindowSeconds
	}
}

func mergeDisclosureConfig(config *DisclosureConfig, partial PartialDisclosureConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}

	if partial.IncludeAgent != nil {
		config.IncludeAgent = *partial.IncludeAgent
	}

	if partial.IncludeOS != nil {
		config.IncludeOS = *partial.IncludeOS
	}

	if partial.Channels != nil {
		mergeDisclosureChannelsConfig(&config.Channels, *partial.Channels)
	}
}

func mergeDisclosureChannelsConfig(config *DisclosureChannelsConfig, partial PartialDisclosureChannelsConfig) {
	if partial.GitCommit != nil {
		config.GitCommit = *partial.GitCommit
	}
	if partial.PullRequest != nil {
		config.PullRequest = *partial.PullRequest
	}
	if partial.IssueComment != nil {
		config.IssueComment = *partial.IssueComment
	}
	if partial.ReviewComment != nil {
		config.ReviewComment = *partial.ReviewComment
	}
	if partial.InlineCommentVisible != nil {
		config.InlineCommentVisible = *partial.InlineCommentVisible
	}
}

func mergeToolPathsConfig(config *ToolPathsConfig, partial PartialToolPathsConfig) {
	if partial.GitPath != nil {
		config.GitPath = stringPtr(*partial.GitPath)
	}

	if partial.GHPath != nil {
		config.GHPath = stringPtr(*partial.GHPath)
	}

	if partial.LooperPath != nil {
		config.LooperPath = stringPtr(*partial.LooperPath)
	}

	if partial.OsascriptPath != nil {
		config.OsascriptPath = stringPtr(*partial.OsascriptPath)
	}
}

func mergeDaemonConfig(config *DaemonConfig, partial PartialDaemonConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}

	if partial.RestartPolicy != nil {
		config.RestartPolicy = *partial.RestartPolicy
	}

	if partial.RestartThrottleSeconds != nil {
		config.RestartThrottleSeconds = *partial.RestartThrottleSeconds
	}

	if partial.PlistPath != nil {
		config.PlistPath = stringPtr(*partial.PlistPath)
	}

	if partial.LogDir != nil {
		config.LogDir = *partial.LogDir
	}

	if partial.ShutdownTimeoutMS != nil {
		config.ShutdownTimeoutMS = *partial.ShutdownTimeoutMS
	}

	if partial.WorkingDirectory != nil {
		config.WorkingDirectory = *partial.WorkingDirectory
	}

	if partial.Environment != nil {
		config.Environment = mergeStringMap(config.Environment, partial.Environment)
	}

	if partial.ResourceGuard != nil {
		mergeResourceGuardConfig(&config.ResourceGuard, *partial.ResourceGuard)
	}
	if partial.WorktreeCleanup != nil {
		mergeWorktreeCleanupConfig(&config.WorktreeCleanup, *partial.WorktreeCleanup)
	}
}

func mergeResourceGuardConfig(config *ResourceGuardConfig, partial PartialResourceGuardConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.MinDiskFreePercent != nil {
		config.MinDiskFreePercent = *partial.MinDiskFreePercent
	}
	if partial.MinDiskFreeGB != nil {
		config.MinDiskFreeGB = *partial.MinDiskFreeGB
	}
	if partial.MaxLoadPerCPU != nil {
		config.MaxLoadPerCPU = *partial.MaxLoadPerCPU
	}
}

func mergeWorktreeCleanupConfig(config *WorktreeCleanupConfig, partial PartialWorktreeCleanupConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.Interval != nil {
		config.Interval = *partial.Interval
	}
	if partial.RetentionDays != nil {
		config.RetentionDays = *partial.RetentionDays
	}
	if partial.MaxPerTick != nil {
		config.MaxPerTick = *partial.MaxPerTick
	}
	if partial.IncludeOrphans != nil {
		config.IncludeOrphans = *partial.IncludeOrphans
	}
	if partial.DryRun != nil {
		config.DryRun = *partial.DryRun
	}
}

func mergePackageConfig(config *PackageConfig, partial PartialPackageConfig) {
	if partial.Distribution != nil {
		config.Distribution = *partial.Distribution
	}

	if partial.AutoMigrateOnStartup != nil {
		config.AutoMigrateOnStartup = *partial.AutoMigrateOnStartup
	}

	if partial.RequireBackupBeforeMigrate != nil {
		config.RequireBackupBeforeMigrate = *partial.RequireBackupBeforeMigrate
	}
}

func mergeDefaultsConfig(config *DefaultsConfig, partial PartialDefaultsConfig) {
	if partial.BaseBranch != nil {
		config.BaseBranch = *partial.BaseBranch
	}

	if partial.AllowAutoCommit != nil {
		config.AllowAutoCommit = *partial.AllowAutoCommit
	}

	if partial.AllowAutoPush != nil {
		config.AllowAutoPush = *partial.AllowAutoPush
	}

	if partial.AllowAutoApprove != nil {
		config.AllowAutoApprove = *partial.AllowAutoApprove
	}

	if partial.AllowAutoMerge != nil {
		config.AllowAutoMerge = *partial.AllowAutoMerge
	}

	if partial.AllowRiskyFixes != nil {
		config.AllowRiskyFixes = *partial.AllowRiskyFixes
	}

	if partial.FixAllPullRequests != nil {
		config.FixAllPullRequests = *partial.FixAllPullRequests
	}

	if partial.OpenPRStrategy != nil {
		config.OpenPRStrategy = *partial.OpenPRStrategy
	}

	if partial.AddSnapshotMode != nil {
		config.AddSnapshotMode = *partial.AddSnapshotMode
	}

	if partial.ValidationCommands != nil {
		config.ValidationCommands = append([]string(nil), (*partial.ValidationCommands)...)
	}
}

func mergeReviewerConfig(config *ReviewerConfig, partial PartialReviewerConfig) {
	if partial.Loop != nil {
		mergeReviewerLoopConfig(&config.Loop, *partial.Loop)
	}
	if partial.Convergence != nil {
		if config.Convergence == nil {
			defaults := DefaultReviewerConvergenceConfig()
			config.Convergence = &defaults
		}
		mergeReviewerConvergenceConfig(config.Convergence, *partial.Convergence)
	}
	if partial.Retry != nil {
		mergeReviewerRetryConfig(&config.Retry, *partial.Retry)
	}
	if partial.Scope != nil {
		config.Scope = *partial.Scope
	}
	if partial.PublishMode != nil {
		config.PublishMode = *partial.PublishMode
	}
	if partial.ReviewEvents != nil {
		mergeReviewerReviewEventsConfig(&config.ReviewEvents, *partial.ReviewEvents)
	}
	if partial.DetectDuplicateFindings != nil {
		config.DetectDuplicateFindings = *partial.DetectDuplicateFindings
	} else if partial.DedupeFindings != nil {
		config.DetectDuplicateFindings = *partial.DedupeFindings
	}
	if partial.NativeResume != nil {
		mergeReviewerNativeResumeConfig(&config.NativeResume, *partial.NativeResume)
	}
	if partial.ThreadResolution != nil {
		mergeReviewerThreadResolutionConfig(&config.ThreadResolution, *partial.ThreadResolution)
	}
}

func mergeReviewerConvergenceConfig(config *ReviewerConvergenceConfig, partial PartialReviewerConvergenceConfig) {
	if partial.MaxConsecutiveUnproductive != nil {
		config.MaxConsecutiveUnproductive = *partial.MaxConsecutiveUnproductive
	}
	if partial.MaxFixerAttemptsPerItem != nil {
		config.MaxFixerAttemptsPerItem = *partial.MaxFixerAttemptsPerItem
	}
	if partial.MaxTotalRounds != nil {
		config.MaxTotalRounds = *partial.MaxTotalRounds
	}
	if partial.SeverityFloor != nil {
		config.SeverityFloor = *partial.SeverityFloor
	}
}

func mergeReviewerRetryConfig(config *ReviewerRetryConfig, partial PartialReviewerRetryConfig) {
	if partial.EnhancedTransientClassification != nil {
		config.EnhancedTransientClassification = *partial.EnhancedTransientClassification
	}
	if partial.ExtraTransientErrorPatterns != nil {
		config.ExtraTransientErrorPatterns = append([]string(nil), (*partial.ExtraTransientErrorPatterns)...)
	}
	if partial.RecoverExistingMatchedFailures != nil {
		config.RecoverExistingMatchedFailures = *partial.RecoverExistingMatchedFailures
	}
	if partial.AutoRecoveryMaxAttempts != nil {
		config.AutoRecoveryMaxAttempts = *partial.AutoRecoveryMaxAttempts
	}
	if partial.MaxDelayMS != nil {
		config.MaxDelayMS = *partial.MaxDelayMS
	}
	*config = NormalizeReviewerRetryConfig(*config)
}

func mergeReviewerNativeResumeConfig(config *ReviewerNativeResumeConfig, partial PartialReviewerNativeResumeConfig) {
	if partial.OnHeadChange != nil {
		config.OnHeadChange = *partial.OnHeadChange
	}
	if partial.ReReviewPromptOnHeadChange != nil {
		config.ReReviewPromptOnHeadChange = *partial.ReReviewPromptOnHeadChange
	}
}

func mergeReviewerThreadResolutionConfig(config *ReviewerThreadResolutionConfig, partial PartialReviewerThreadResolutionConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}
	if partial.Scope != nil {
		config.Scope = *partial.Scope
	}
	if partial.AutoResolve != nil {
		config.AutoResolve = *partial.AutoResolve
	}
	if partial.RequireAuditComment != nil {
		config.RequireAuditComment = *partial.RequireAuditComment
	}
	if partial.RequireNewHeadSinceThread != nil {
		config.RequireNewHeadSinceThread = *partial.RequireNewHeadSinceThread
	}
	if partial.RequireCurrentReviewRequest != nil {
		config.RequireCurrentReviewRequest = *partial.RequireCurrentReviewRequest
	}
	if partial.MaxThreadsPerRun != nil {
		config.MaxThreadsPerRun = *partial.MaxThreadsPerRun
	}
}

func mergeReviewerReviewEventsConfig(config *ReviewerReviewEventsConfig, partial PartialReviewerReviewEventsConfig) {
	if partial.Clean != nil {
		config.Clean = *partial.Clean
	}
	if partial.Blocking != nil {
		config.Blocking = *partial.Blocking
	}
}

func mergeReviewerLoopConfig(config *ReviewerLoopConfig, partial PartialReviewerLoopConfig) {
	if partial.EnabledByDefault != nil {
		config.EnabledByDefault = *partial.EnabledByDefault
	}
	if partial.QuietPeriodSeconds != nil {
		config.QuietPeriodSeconds = *partial.QuietPeriodSeconds
	}
	if partial.MinPublishIntervalSeconds != nil {
		config.MinPublishIntervalSeconds = *partial.MinPublishIntervalSeconds
	}
	if partial.MaxIterationsPerPR != nil {
		config.MaxIterationsPerPR = *partial.MaxIterationsPerPR
	}
	if partial.MaxIterationsPerHead != nil {
		config.MaxIterationsPerHead = *partial.MaxIterationsPerHead
	}
	if partial.MaxWallClockSeconds != nil {
		config.MaxWallClockSeconds = *partial.MaxWallClockSeconds
	}
	if partial.MaxConsecutiveFailures != nil {
		config.MaxConsecutiveFailures = *partial.MaxConsecutiveFailures
	}
	if partial.MaxAgentExecutionsPerPR != nil {
		config.MaxAgentExecutionsPerPR = *partial.MaxAgentExecutionsPerPR
	}
	if partial.StopOnApproved != nil {
		config.StopOnApproved = *partial.StopOnApproved
	}
	if partial.StopOnReadyLabel != nil {
		config.StopOnReadyLabel = *partial.StopOnReadyLabel
	}
	if partial.StopOnIdenticalOutput != nil {
		config.StopOnIdenticalOutput = *partial.StopOnIdenticalOutput
	}
}

func mergeInstructionsConfig(config *InstructionsConfig, partial PartialInstructionsConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.MaxBytes != nil {
		config.MaxBytes = *partial.MaxBytes
	}
}

func mergeRoleConfigs(config *RoleConfigs, partial PartialRoleConfigs) {
	if partial.Triager != nil {
		mergeTriagerRoleConfig(&config.Triager, *partial.Triager)
	}
	if partial.Coordinator != nil {
		mergeCoordinatorRoleConfig(&config.Coordinator, *partial.Coordinator)
	}
	if partial.Planner != nil {
		mergePlannerRoleConfig(&config.Planner, *partial.Planner)
	}
	if partial.Reviewer != nil {
		mergeReviewerRoleConfig(&config.Reviewer, *partial.Reviewer)
	}
	if partial.Fixer != nil {
		mergeFixerRoleConfig(&config.Fixer, *partial.Fixer)
	}
	if partial.Worker != nil {
		mergeWorkerRoleConfig(&config.Worker, *partial.Worker)
	}
	if partial.Deployer != nil {
		mergeDeployerRoleConfig(&config.Deployer, *partial.Deployer)
	}
}

func mergeTriagerRoleConfig(config *TriagerRoleConfig, partial PartialTriagerRoleConfig) {
	if partial.Preset != nil {
		config.Preset = TriagerPreset(strings.TrimSpace(string(*partial.Preset)))
	}
	if partial.Classify != nil {
		config.Classify = *partial.Classify
	}
	if partial.AuthorTiers != nil {
		if config.AuthorTiers == nil {
			config.AuthorTiers = map[string]TriagerAdmissionOutcome{}
		}
		for tier, outcome := range *partial.AuthorTiers {
			config.AuthorTiers[tier] = TriagerAdmissionOutcome(strings.TrimSpace(string(outcome)))
		}
	}
	if partial.Legacy != nil {
		mergeTriagerLegacyPolicyConfig(&config.Legacy, *partial.Legacy)
	}
}

func mergeTriagerLegacyPolicyConfig(config *TriagerLegacyPolicyConfig, partial PartialTriagerLegacyPolicyConfig) {
	if partial.AutoRouteConfidence != nil {
		config.AutoRouteConfidence = *partial.AutoRouteConfidence
	}
	if partial.MaxAutoRouteRisk != nil {
		config.MaxAutoRouteRisk = strings.TrimSpace(*partial.MaxAutoRouteRisk)
	}
	if partial.RequireInScope != nil {
		config.RequireInScope = *partial.RequireInScope
	}
	if partial.RequireNoMissingInformation != nil {
		config.RequireNoMissingInformation = *partial.RequireNoMissingInformation
	}
	if partial.RequirePlanner != nil {
		config.RequirePlanner = *partial.RequirePlanner
	}
	if partial.RequireRationale != nil {
		config.RequireRationale = *partial.RequireRationale
	}
}

func mergeCoordinatorRoleConfig(config *CoordinatorRoleConfig, partial PartialCoordinatorRoleConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.PollInterval != nil {
		config.PollInterval = *partial.PollInterval
	}
	if partial.Triage != nil {
		mergeCoordinatorTriageConfig(&config.Triage, *partial.Triage)
	}
	if partial.Dispatch != nil {
		mergeCoordinatorDispatchConfig(&config.Dispatch, *partial.Dispatch)
	}
	if partial.Dependencies != nil {
		mergeCoordinatorDependenciesConfig(&config.Dependencies, *partial.Dependencies)
	}
	if partial.MergeWatch != nil {
		mergeCoordinatorMergeWatchConfig(&config.MergeWatch, *partial.MergeWatch)
	}
	if partial.MarkReady != nil {
		mergeCoordinatorMarkReadyConfig(&config.MarkReady, *partial.MarkReady)
	}
	if partial.PostMergeDigest != nil {
		mergeCoordinatorPostMergeDigestConfig(&config.PostMergeDigest, *partial.PostMergeDigest)
	}
}

func mergeCoordinatorPostMergeDigestConfig(config **CoordinatorPostMergeDigestConfig, partial PartialCoordinatorPostMergeDigestConfig) {
	if *config == nil {
		*config = &CoordinatorPostMergeDigestConfig{}
	}
	if partial.Enabled != nil {
		(*config).Enabled = *partial.Enabled
	}
	if partial.Schedule != nil {
		(*config).Schedule = strings.TrimSpace(*partial.Schedule)
	}
	if partial.Timezone != nil {
		(*config).Timezone = strings.TrimSpace(*partial.Timezone)
	}
	if partial.IncludeEmpty != nil {
		(*config).IncludeEmpty = *partial.IncludeEmpty
	}
	if partial.MaxItems != nil {
		(*config).MaxItems = *partial.MaxItems
	}
}

func mergeCoordinatorTriageConfig(config *CoordinatorTriageConfig, partial PartialCoordinatorTriageConfig) {
	if partial.TriagedLabel != nil {
		config.TriagedLabel = *partial.TriagedLabel
	}
	if partial.MaxIssueAgeDays != nil {
		config.MaxIssueAgeDays = *partial.MaxIssueAgeDays
	}
	if partial.MaxPerTick != nil {
		config.MaxPerTick = *partial.MaxPerTick
	}
	if partial.Disposition != nil {
		mergeCoordinatorTriageDispositionConfig(&config.Disposition, *partial.Disposition)
	}
}

func mergeCoordinatorTriageDispositionConfig(config *CoordinatorTriageDispositionConfig, partial PartialCoordinatorTriageDispositionConfig) {
	if partial.OutOfScopeLabel != nil {
		config.OutOfScopeLabel = *partial.OutOfScopeLabel
	}
	if partial.UnclearLabel != nil {
		config.UnclearLabel = *partial.UnclearLabel
	}
	if partial.ReTriageOnAuthorReply != nil {
		config.ReTriageOnAuthorReply = *partial.ReTriageOnAuthorReply
	}
}

func mergeCoordinatorDispatchConfig(config *CoordinatorDispatchConfig, partial PartialCoordinatorDispatchConfig) {
	if partial.Mode != nil {
		config.Mode = *partial.Mode
	}
	if partial.HumanGate != nil {
		mergeCoordinatorDispatchHumanGateConfig(&config.HumanGate, *partial.HumanGate)
	}
	if partial.Autonomous != nil {
		mergeCoordinatorDispatchAutonomousConfig(&config.Autonomous, *partial.Autonomous)
	}
	if partial.AssignTo != nil {
		config.AssignTo = *partial.AssignTo
	}
}

func mergeCoordinatorDispatchHumanGateConfig(config *CoordinatorDispatchHumanGateConfig, partial PartialCoordinatorDispatchHumanGateConfig) {
	if partial.SlashCommands != nil {
		config.SlashCommands = cloneStrings(*partial.SlashCommands)
	}
	if partial.AllowedUsers != nil {
		config.AllowedUsers = cloneStrings(*partial.AllowedUsers)
	}
}

func mergeCoordinatorDispatchAutonomousConfig(config *CoordinatorDispatchAutonomousConfig, partial PartialCoordinatorDispatchAutonomousConfig) {
	if partial.DelayMinutes != nil {
		config.DelayMinutes = *partial.DelayMinutes
	}
	if partial.HoldLabel != nil {
		config.HoldLabel = *partial.HoldLabel
	}
}

func mergeCoordinatorDependenciesConfig(config *CoordinatorDependenciesConfig, partial PartialCoordinatorDependenciesConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.APITimeoutSeconds != nil {
		config.APITimeoutSeconds = *partial.APITimeoutSeconds
	}
	if partial.APIRetryAttempts != nil {
		config.APIRetryAttempts = *partial.APIRetryAttempts
	}
}

func mergeCoordinatorMergeWatchConfig(config *CoordinatorMergeWatchConfig, partial PartialCoordinatorMergeWatchConfig) {
	if partial.TransientRetries != nil {
		config.TransientRetries = *partial.TransientRetries
	}
	if partial.MaxIndeterminateDuration != nil {
		config.MaxIndeterminateDuration = *partial.MaxIndeterminateDuration
	}
}

func mergeCoordinatorMarkReadyConfig(config *CoordinatorMarkReadyConfig, partial PartialCoordinatorMarkReadyConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.Scope != nil {
		config.Scope = CoordinatorMarkReadyScope(strings.TrimSpace(string(*partial.Scope)))
	}
}

func mergePlannerRoleConfig(config *PlannerRoleConfig, partial PartialPlannerRoleConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeIssueRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.Escalation != nil {
		if config.Escalation == nil {
			config.Escalation = &PlannerEscalationConfig{}
		}
		mergePlannerEscalationConfig(config.Escalation, *partial.Escalation)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
	if partial.Agent != nil {
		mergeRoleAgentConfig(&config.Agent, partial.Agent)
	}
}

func mergePlannerEscalationConfig(config *PlannerEscalationConfig, partial PartialPlannerEscalationConfig) {
	if partial.MaxEstimatedFiles != nil {
		config.MaxEstimatedFiles = *partial.MaxEstimatedFiles
	}
	if partial.MaxEstimatedPackages != nil {
		config.MaxEstimatedPackages = *partial.MaxEstimatedPackages
	}
	if partial.PublicAPI != nil {
		config.PublicAPI = *partial.PublicAPI
	}
	if partial.Config != nil {
		config.Config = *partial.Config
	}
	if partial.CLI != nil {
		config.CLI = *partial.CLI
	}
	if partial.Storage != nil {
		config.Storage = *partial.Storage
	}
	if partial.WireFormat != nil {
		config.WireFormat = *partial.WireFormat
	}
	if partial.ADRConflict != nil {
		config.ADRConflict = *partial.ADRConflict
	}
	if partial.AuthorityDecision != nil {
		config.AuthorityDecision = *partial.AuthorityDecision
	}
}

func mergeWorkerRoleConfig(config *WorkerRoleConfig, partial PartialWorkerRoleConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeIssueRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
	if partial.Agent != nil {
		mergeRoleAgentConfig(&config.Agent, partial.Agent)
	}
}

func mergeReviewerRoleConfig(config *ReviewerRoleConfig, partial PartialReviewerRoleConfig) {
	if partial.AutoDiscovery != nil || partial.Triggers != nil || partial.SpecReview != nil {
		mergeReviewerRoleDiscoveryConfig(&config.Discovery, PartialReviewerRoleDiscoveryConfig{
			AutoDiscovery: partial.AutoDiscovery,
			Triggers:      partial.Triggers,
			SpecReview:    partial.SpecReview,
		})
	}
	if partial.Discovery != nil {
		mergeReviewerRoleDiscoveryConfig(&config.Discovery, *partial.Discovery)
	}
	if partial.Behavior != nil {
		mergeReviewerConfig(&config.Behavior, *partial.Behavior)
	}
	if partial.AutoMerge != nil {
		mergeReviewerAutoMergeConfig(&config.AutoMerge, *partial.AutoMerge)
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
	if partial.Agent != nil {
		mergeRoleAgentConfig(&config.Agent, partial.Agent)
	}
}

func mergeReviewerAutoMergeConfig(config *ReviewerAutoMergeConfig, partial PartialReviewerAutoMergeConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.Strategy != nil {
		config.Strategy = *partial.Strategy
	}
	if partial.RequireBranchProtection != nil {
		config.RequireBranchProtection = *partial.RequireBranchProtection
	}
	if partial.TransientRetries != nil {
		config.TransientRetries = *partial.TransientRetries
	}
	if partial.Scope != nil {
		config.Scope = *partial.Scope
	}
}

func mergeReviewerRoleDiscoveryConfig(config *ReviewerRoleDiscoveryConfig, partial PartialReviewerRoleDiscoveryConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeReviewerRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.SpecReview != nil {
		mergeReviewerSpecReviewConfig(&config.SpecReview, *partial.SpecReview)
	}
}

func mergeFixerRoleConfig(config *FixerRoleConfig, partial PartialFixerRoleConfig) {
	if partial.AutoDiscovery != nil {
		config.AutoDiscovery = *partial.AutoDiscovery
	}
	if partial.Triggers != nil {
		mergeFixerRoleTriggersConfig(&config.Triggers, *partial.Triggers)
	}
	if partial.Regeneration != nil {
		regeneration := config.Regeneration
		if regeneration == nil {
			// A materialized section must inherit the same safe default as an
			// omitted section.  Partial config is field-wise: an empty table is
			// not an explicit request to turn branch deletion off.
			regeneration = &FixerRegenerationConfig{DeleteBranch: true}
		} else {
			cloned := *regeneration
			regeneration = &cloned
		}
		if partial.Regeneration.DeleteBranch != nil {
			regeneration.DeleteBranch = *partial.Regeneration.DeleteBranch
		}
		config.Regeneration = regeneration
	}
	if partial.Instructions != nil {
		config.Instructions = *partial.Instructions
	}
	if partial.Agent != nil {
		mergeRoleAgentConfig(&config.Agent, partial.Agent)
	}
}

func mergeIssueRoleTriggersConfig(config *IssueRoleTriggersConfig, partial PartialIssueRoleTriggersConfig) {
	if partial.Labels != nil {
		config.Labels = cloneStrings(*partial.Labels)
	}
	if partial.LabelMode != nil {
		config.LabelMode = *partial.LabelMode
	}
	if partial.RequireAssigneeCurrentUser != nil {
		config.RequireAssigneeCurrentUser = *partial.RequireAssigneeCurrentUser
	}
}

func mergeReviewerRoleTriggersConfig(config *ReviewerRoleTriggersConfig, partial PartialReviewerRoleTriggersConfig) {
	if partial.IncludeDrafts != nil {
		config.IncludeDrafts = *partial.IncludeDrafts
	}
	if partial.RequireReviewRequest != nil {
		config.RequireReviewRequest = *partial.RequireReviewRequest
	}
	if partial.EnableSelfReview != nil {
		config.EnableSelfReview = *partial.EnableSelfReview
	}
	if partial.Labels != nil {
		config.Labels = cloneStrings(*partial.Labels)
	}
	if partial.LabelMode != nil {
		config.LabelMode = *partial.LabelMode
	}
}

func mergeReviewerSpecReviewConfig(config *ReviewerSpecReviewConfig, partial PartialReviewerSpecReviewConfig) {
	if partial.IncludeReviewingLabel != nil {
		config.IncludeReviewingLabel = *partial.IncludeReviewingLabel
	}
	if partial.ReviewingLabel != nil {
		config.ReviewingLabel = *partial.ReviewingLabel
	}
}

func mergeFixerRoleTriggersConfig(config *FixerRoleTriggersConfig, partial PartialFixerRoleTriggersConfig) {
	if partial.IncludeDrafts != nil {
		config.IncludeDrafts = *partial.IncludeDrafts
	}
	if partial.AuthorFilter != nil {
		config.AuthorFilter = *partial.AuthorFilter
	}
	if partial.Labels != nil {
		config.Labels = cloneStrings(*partial.Labels)
	}
	if partial.LabelMode != nil {
		config.LabelMode = *partial.LabelMode
	}
}

func mergeAnyMap(base map[string]any, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		merged[key] = cloneAnyValue(value)
	}

	for key, value := range override {
		if baseValue, ok := merged[key]; ok {
			merged[key] = mergeAnyValue(baseValue, value)
			continue
		}

		merged[key] = cloneAnyValue(value)
	}

	return merged
}

func mergeAnyValue(base any, override any) any {
	baseMap, baseIsMap := base.(map[string]any)
	overrideMap, overrideIsMap := override.(map[string]any)
	if baseIsMap && overrideIsMap {
		return mergeAnyMap(baseMap, overrideMap)
	}

	return cloneAnyValue(override)
}

func mergeStringMap(base map[string]string, override map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}

	for key, value := range override {
		merged[key] = value
	}

	return merged
}

func cloneAnyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return mergeAnyMap(nil, typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneAnyValue(item)
		}

		return cloned
	default:
		return typed
	}
}

func cloneSoundLevels(levels []NotificationSoundLevel) []NotificationSoundLevel {
	if levels == nil {
		return nil
	}

	cloned := make([]NotificationSoundLevel, len(levels))
	copy(cloned, levels)
	return cloned
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func clonePartialConfig(partial PartialConfig) PartialConfig {
	cloned := partial
	if partial.Agent != nil {
		cloned.Agent = clonePartialAgentConfig(partial.Agent)
	}
	if partial.Defaults != nil {
		defaults := *partial.Defaults
		cloned.Defaults = &defaults
	}
	if partial.HITL != nil {
		hitl := *partial.HITL
		if partial.HITL.Enabled != nil {
			enabled := *partial.HITL.Enabled
			hitl.Enabled = &enabled
		}
		cloned.HITL = &hitl
	}
	if partial.Intake != nil {
		intake := *partial.Intake
		if partial.Intake.Telegram != nil {
			telegram := *partial.Intake.Telegram
			if telegram.Enabled != nil {
				enabled := *telegram.Enabled
				telegram.Enabled = &enabled
			}
			if telegram.AllowedUserIDs != nil {
				ids := append([]int64(nil), (*telegram.AllowedUserIDs)...)
				telegram.AllowedUserIDs = &ids
			}
			intake.Telegram = &telegram
		}
		cloned.Intake = &intake
	}
	if partial.LegacyReviewer != nil {
		cloned.LegacyReviewer = clonePartialReviewerConfig(partial.LegacyReviewer)
	}
	if partial.Roles != nil {
		cloned.Roles = clonePartialRoleConfigs(partial.Roles)
	}
	if partial.Projects != nil {
		projects := clonePartialProjects(*partial.Projects)
		cloned.Projects = &projects
	}
	if partial.Providers != nil {
		providers := make([]PartialProviderConfig, len(*partial.Providers))
		copy(providers, *partial.Providers)
		for i := range providers {
			providers[i].Kind = cloneProviderKindPtr(providers[i].Kind)
			providers[i].BaseURL = cloneStringPtr(providers[i].BaseURL)
			providers[i].GHPath = cloneStringPtr(providers[i].GHPath)
			providers[i].TokenEnv = cloneStringPtr(providers[i].TokenEnv)
		}
		cloned.Providers = &providers
	}
	return cloned
}

func clonePartialProjects(projects []PartialProjectRefConfig) []PartialProjectRefConfig {
	if projects == nil {
		return nil
	}
	cloned := make([]PartialProjectRefConfig, len(projects))
	for index, project := range projects {
		cloned[index] = PartialProjectRefConfig{
			ID:              project.ID,
			Name:            project.Name,
			PersonalProject: cloneBoolPtr(project.PersonalProject),
			Provider:        cloneStringPtr(project.Provider),
			Repo:            cloneStringPtr(project.Repo),
			RepoPath:        project.RepoPath,
			Path:            project.Path,
			BaseBranch:      cloneStringPtr(project.BaseBranch),
			WorktreeRoot:    cloneStringPtr(project.WorktreeRoot),
			Webhook:         clonePartialProjectWebhookConfig(project.Webhook),
			Validation:      clonePartialProjectValidationConfig(project.Validation),
			Instructions:    cloneStringMap(project.Instructions),
			Roles:           clonePartialRoleConfigs(project.Roles),
		}
	}
	return cloned
}

func clonePartialProjectWebhookConfig(config *PartialProjectWebhookConfig) *PartialProjectWebhookConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	return &cloned
}

func clonePartialProjectValidationConfig(config *PartialProjectValidationConfig) *PartialProjectValidationConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	if config.Commands != nil {
		commands := append([]string(nil), (*config.Commands)...)
		cloned.Commands = &commands
	}
	return &cloned
}

func clonePartialReviewerConfig(config *PartialReviewerConfig) *PartialReviewerConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	if config.Loop != nil {
		loop := *config.Loop
		cloned.Loop = &loop
	}
	if config.Convergence != nil {
		convergence := *config.Convergence
		cloned.Convergence = &convergence
	}
	if config.Retry != nil {
		retry := *config.Retry
		if config.Retry.ExtraTransientErrorPatterns != nil {
			patterns := append([]string(nil), (*config.Retry.ExtraTransientErrorPatterns)...)
			retry.ExtraTransientErrorPatterns = &patterns
		}
		cloned.Retry = &retry
	}
	if config.ReviewEvents != nil {
		reviewEvents := *config.ReviewEvents
		cloned.ReviewEvents = &reviewEvents
	}
	if config.NativeResume != nil {
		nativeResume := *config.NativeResume
		cloned.NativeResume = &nativeResume
	}
	if config.ThreadResolution != nil {
		threadResolution := *config.ThreadResolution
		cloned.ThreadResolution = &threadResolution
	}
	return &cloned
}

func cloneProjects(projects []PartialProjectRefConfig) []ProjectRefConfig {
	if projects == nil {
		return nil
	}

	cloned := make([]ProjectRefConfig, len(projects))
	for index, project := range projects {
		roles := mergeLegacyProjectInstructionsIntoRoles(clonePartialRoleConfigs(project.Roles), project.Instructions)
		canonicalizePartialRoleAgentBindings(roles)
		repoPath := firstNonEmpty(project.RepoPath, project.Path)

		cloned[index] = ProjectRefConfig{
			ID:              project.ID,
			Name:            project.Name,
			PersonalProject: project.PersonalProject != nil && *project.PersonalProject,
			RepoPath:        repoPath,
			Path:            project.Path,
			Network:         ProjectNetworkConfig{Mode: NetworkModeOff},
			Roles:           roles,
		}
		if project.Validation != nil {
			validation := &ProjectValidationConfig{}
			if project.Validation.Commands != nil {
				validation.Commands = append([]string(nil), (*project.Validation.Commands)...)
			}
			if project.Validation.OptOut != nil {
				validation.OptOut = *project.Validation.OptOut
			}
			cloned[index].Validation = validation
		}
		if project.Provider != nil {
			cloned[index].Provider = strings.TrimSpace(*project.Provider)
		}
		if project.Repo != nil {
			cloned[index].Repo = strings.TrimSpace(*project.Repo)
		}
		if project.Webhook != nil && project.Webhook.Mode != nil {
			cloned[index].Webhook.Mode = *project.Webhook.Mode
		}

		if project.BaseBranch != nil {
			cloned[index].BaseBranch = stringPtr(*project.BaseBranch)
		}

		if project.WorktreeRoot != nil {
			cloned[index].WorktreeRoot = stringPtr(*project.WorktreeRoot)
		}
	}

	return cloned
}

func cloneProviderKindPtr(value *ProviderKind) *ProviderKind {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneProviderConfigs(providers []PartialProviderConfig) []ProviderConfig {
	if providers == nil {
		return nil
	}
	cloned := make([]ProviderConfig, len(providers))
	for index, provider := range providers {
		kind := ProviderKindGitHub
		if provider.Kind != nil {
			kind = *provider.Kind
		}
		cloned[index] = ProviderConfig{
			ID:       strings.TrimSpace(provider.ID),
			Kind:     kind,
			GHPath:   cloneStringPtr(provider.GHPath),
			TokenEnv: cloneStringPtr(provider.TokenEnv),
		}
		if provider.BaseURL != nil {
			cloned[index].BaseURL = normalizeBaseURL(*provider.BaseURL)
		}
		normalizeProviderConfig(&cloned[index])
	}
	return cloned
}

func mergeLegacyProjectInstructionsIntoRoles(roles *PartialRoleConfigs, instructions map[string]string) *PartialRoleConfigs {
	if len(instructions) == 0 {
		return roles
	}
	if roles == nil {
		roles = &PartialRoleConfigs{}
	}
	for role, text := range instructions {
		switch role {
		case "planner":
			if roles.Planner == nil {
				roles.Planner = &PartialPlannerRoleConfig{}
			}
			if roles.Planner.Instructions == nil {
				roles.Planner.Instructions = stringPtr(text)
			}
		case "worker":
			if roles.Worker == nil {
				roles.Worker = &PartialWorkerRoleConfig{}
			}
			if roles.Worker.Instructions == nil {
				roles.Worker.Instructions = stringPtr(text)
			}
		case "reviewer":
			if roles.Reviewer == nil {
				roles.Reviewer = &PartialReviewerRoleConfig{}
			}
			if roles.Reviewer.Instructions == nil {
				roles.Reviewer.Instructions = stringPtr(text)
			}
		case "fixer":
			if roles.Fixer == nil {
				roles.Fixer = &PartialFixerRoleConfig{}
			}
			if roles.Fixer.Instructions == nil {
				roles.Fixer.Instructions = stringPtr(text)
			}
		}
	}
	return roles
}

func clonePartialAgentConfig(agent *PartialAgentConfig) *PartialAgentConfig {
	if agent == nil {
		return nil
	}
	cloned := *agent
	cloned.Vendor = nil
	if agent.Vendor != nil {
		vendor := *agent.Vendor
		cloned.Vendor = &vendor
	}
	cloned.Model = cloneStringPtr(agent.Model)
	cloned.ReasoningEffort = cloneReasoningEffortPtr(agent.ReasoningEffort)
	cloned.Profiles = cloneAgentProfiles(agent.Profiles)
	if agent.Params != nil {
		cloned.Params = mergeAnyMap(nil, agent.Params)
	}
	if agent.Env != nil {
		cloned.Env = cloneStringMap(agent.Env)
	}
	if agent.Timeouts != nil {
		timeouts := *agent.Timeouts
		cloned.Timeouts = &timeouts
	}
	if agent.NativeResume != nil {
		nativeResume := *agent.NativeResume
		cloned.NativeResume = &nativeResume
	}
	return &cloned
}

func cloneAgentProfiles(profiles map[string]AgentBindingConfig) map[string]AgentBindingConfig {
	if profiles == nil {
		return nil
	}
	cloned := make(map[string]AgentBindingConfig, len(profiles))
	for id, binding := range profiles {
		cloned[id] = cloneAgentBindingConfig(binding)
	}
	return cloned
}

// MergeDeployerRoleConfig applies a project's deployer overrides onto a copy of
// the global role, so a caller needing only this role avoids cloning every other.
func MergeDeployerRoleConfig(config *DeployerRoleConfig, partial PartialDeployerRoleConfig) {
	mergeDeployerRoleConfig(config, partial)
}

func mergeDeployerRoleConfig(config *DeployerRoleConfig, partial PartialDeployerRoleConfig) {
	if partial.Enabled != nil {
		config.Enabled = *partial.Enabled
	}
	if partial.Command != nil {
		config.Command = strings.TrimSpace(*partial.Command)
	}
	if partial.TimeoutSeconds != nil {
		config.TimeoutSeconds = *partial.TimeoutSeconds
	}
	if partial.Environment != nil {
		environment := make(map[string]string, len(*partial.Environment))
		for key, value := range *partial.Environment {
			environment[key] = value
		}
		config.Environment = environment
	}
}

func clonePartialRoleConfigs(configs *PartialRoleConfigs) *PartialRoleConfigs {
	if configs == nil {
		return nil
	}
	cloned := PartialRoleConfigs{}
	if configs.Coding != nil {
		cloned.Coding = make(map[string]PartialCodingRoleConfig, len(configs.Coding))
		for name, role := range configs.Coding {
			clonedRole := role
			if role.Priority != nil {
				priority := *role.Priority
				clonedRole.Priority = &priority
			}
			clonedRole.Instructions = cloneStringPtr(role.Instructions)
			clonedRole.Agent = cloneRoleAgentConfig(role.Agent)
			clonedRole.Discovery = clonePartialRoleDiscoveryConfig(role.Discovery)
			cloned.Coding[name] = clonedRole
		}
	}
	if configs.Planner != nil {
		planner := *configs.Planner
		if configs.Planner.Triggers != nil {
			triggers := *configs.Planner.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			planner.Triggers = &triggers
		}
		planner.Agent = cloneRoleAgentConfig(configs.Planner.Agent)
		cloned.Planner = &planner
	}
	if configs.Triager != nil {
		triager := *configs.Triager
		if configs.Triager.AuthorTiers != nil {
			overrides := make(map[string]TriagerAdmissionOutcome, len(*configs.Triager.AuthorTiers))
			for tier, outcome := range *configs.Triager.AuthorTiers {
				overrides[tier] = outcome
			}
			triager.AuthorTiers = &overrides
		}
		if configs.Triager.Legacy != nil {
			legacy := *configs.Triager.Legacy
			triager.Legacy = &legacy
		}
		cloned.Triager = &triager
	}
	if configs.Worker != nil {
		worker := *configs.Worker
		if configs.Worker.Triggers != nil {
			triggers := *configs.Worker.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			worker.Triggers = &triggers
		}
		worker.Agent = cloneRoleAgentConfig(configs.Worker.Agent)
		cloned.Worker = &worker
	}
	if configs.Deployer != nil {
		deployerRole := *configs.Deployer
		if configs.Deployer.Enabled != nil {
			enabled := *configs.Deployer.Enabled
			deployerRole.Enabled = &enabled
		}
		if configs.Deployer.Command != nil {
			command := *configs.Deployer.Command
			deployerRole.Command = &command
		}
		if configs.Deployer.TimeoutSeconds != nil {
			timeout := *configs.Deployer.TimeoutSeconds
			deployerRole.TimeoutSeconds = &timeout
		}
		if configs.Deployer.Environment != nil {
			environment := make(map[string]string, len(*configs.Deployer.Environment))
			for key, value := range *configs.Deployer.Environment {
				environment[key] = value
			}
			deployerRole.Environment = &environment
		}
		cloned.Deployer = &deployerRole
	}
	if configs.Coordinator != nil {
		coordinator := *configs.Coordinator
		if configs.Coordinator.Triage != nil {
			triage := *configs.Coordinator.Triage
			if configs.Coordinator.Triage.Disposition != nil {
				disposition := *configs.Coordinator.Triage.Disposition
				triage.Disposition = &disposition
			}
			coordinator.Triage = &triage
		}
		if configs.Coordinator.Dispatch != nil {
			dispatch := *configs.Coordinator.Dispatch
			if configs.Coordinator.Dispatch.HumanGate != nil {
				humanGate := *configs.Coordinator.Dispatch.HumanGate
				if humanGate.SlashCommands != nil {
					slashCommands := cloneStrings(*humanGate.SlashCommands)
					humanGate.SlashCommands = &slashCommands
				}
				if humanGate.AllowedUsers != nil {
					allowedUsers := cloneStrings(*humanGate.AllowedUsers)
					humanGate.AllowedUsers = &allowedUsers
				}
				dispatch.HumanGate = &humanGate
			}
			if configs.Coordinator.Dispatch.Autonomous != nil {
				autonomous := *configs.Coordinator.Dispatch.Autonomous
				dispatch.Autonomous = &autonomous
			}
			coordinator.Dispatch = &dispatch
		}
		if configs.Coordinator.MarkReady != nil {
			markReady := *configs.Coordinator.MarkReady
			if configs.Coordinator.MarkReady.Enabled != nil {
				enabled := *configs.Coordinator.MarkReady.Enabled
				markReady.Enabled = &enabled
			}
			if configs.Coordinator.MarkReady.Scope != nil {
				scope := *configs.Coordinator.MarkReady.Scope
				markReady.Scope = &scope
			}
			coordinator.MarkReady = &markReady
		}
		cloned.Coordinator = &coordinator
	}
	if configs.Reviewer != nil {
		reviewer := *configs.Reviewer
		if configs.Reviewer.Discovery != nil {
			discovery := *configs.Reviewer.Discovery
			if configs.Reviewer.Discovery.Triggers != nil {
				triggers := *configs.Reviewer.Discovery.Triggers
				if triggers.Labels != nil {
					labels := cloneStrings(*triggers.Labels)
					triggers.Labels = &labels
				}
				discovery.Triggers = &triggers
			}
			if configs.Reviewer.Discovery.SpecReview != nil {
				specReview := *configs.Reviewer.Discovery.SpecReview
				discovery.SpecReview = &specReview
			}
			reviewer.Discovery = &discovery
		}
		if configs.Reviewer.Triggers != nil {
			triggers := *configs.Reviewer.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			reviewer.Triggers = &triggers
		}
		if configs.Reviewer.SpecReview != nil {
			specReview := *configs.Reviewer.SpecReview
			reviewer.SpecReview = &specReview
		}
		if configs.Reviewer.Behavior != nil {
			behavior := *configs.Reviewer.Behavior
			if configs.Reviewer.Behavior.Loop != nil {
				loop := *configs.Reviewer.Behavior.Loop
				behavior.Loop = &loop
			}
			if configs.Reviewer.Behavior.Retry != nil {
				retry := *configs.Reviewer.Behavior.Retry
				if configs.Reviewer.Behavior.Retry.ExtraTransientErrorPatterns != nil {
					patterns := append([]string(nil), (*configs.Reviewer.Behavior.Retry.ExtraTransientErrorPatterns)...)
					retry.ExtraTransientErrorPatterns = &patterns
				}
				behavior.Retry = &retry
			}
			if configs.Reviewer.Behavior.ReviewEvents != nil {
				reviewEvents := *configs.Reviewer.Behavior.ReviewEvents
				behavior.ReviewEvents = &reviewEvents
			}
			if configs.Reviewer.Behavior.NativeResume != nil {
				nativeResume := *configs.Reviewer.Behavior.NativeResume
				behavior.NativeResume = &nativeResume
			}
			if configs.Reviewer.Behavior.ThreadResolution != nil {
				threadResolution := *configs.Reviewer.Behavior.ThreadResolution
				behavior.ThreadResolution = &threadResolution
			}
			reviewer.Behavior = &behavior
		}
		reviewer.Agent = cloneRoleAgentConfig(configs.Reviewer.Agent)
		cloned.Reviewer = &reviewer
	}
	if configs.Fixer != nil {
		fixer := *configs.Fixer
		if configs.Fixer.Triggers != nil {
			triggers := *configs.Fixer.Triggers
			if triggers.Labels != nil {
				labels := cloneStrings(*triggers.Labels)
				triggers.Labels = &labels
			}
			fixer.Triggers = &triggers
		}
		if configs.Fixer.Regeneration != nil {
			regeneration := *configs.Fixer.Regeneration
			fixer.Regeneration = &PartialFixerRegenerationConfig{}
			if regeneration.DeleteBranch != nil {
				deleteBranch := *regeneration.DeleteBranch
				fixer.Regeneration.DeleteBranch = &deleteBranch
			}
		}
		fixer.Agent = cloneRoleAgentConfig(configs.Fixer.Agent)
		cloned.Fixer = &fixer
	}
	return &cloned
}

func clonePartialGatekeeperDiffBudget(budget *PartialGatekeeperDiffBudget) *PartialGatekeeperDiffBudget {
	if budget == nil {
		return nil
	}
	cloned := *budget
	if budget.MaxChangedFiles != nil {
		maxChangedFiles := *budget.MaxChangedFiles
		cloned.MaxChangedFiles = &maxChangedFiles
	}
	if budget.MaxDeletions != nil {
		maxDeletions := *budget.MaxDeletions
		cloned.MaxDeletions = &maxDeletions
	}
	return &cloned
}

func clonePartialRoleDiscoveryConfig(config *PartialRoleDiscoveryConfig) *PartialRoleDiscoveryConfig {
	if config == nil {
		return nil
	}
	cloned := *config
	if config.Enabled != nil {
		value := *config.Enabled
		cloned.Enabled = &value
	}
	if config.Source != nil {
		value := *config.Source
		cloned.Source = &value
	}
	if config.Labels != nil {
		values := cloneStrings(*config.Labels)
		cloned.Labels = &values
	}
	if config.LabelMode != nil {
		value := *config.LabelMode
		cloned.LabelMode = &value
	}
	if config.RequireAssigneeCurrentUser != nil {
		value := *config.RequireAssigneeCurrentUser
		cloned.RequireAssigneeCurrentUser = &value
	}
	if config.IncludeDrafts != nil {
		value := *config.IncludeDrafts
		cloned.IncludeDrafts = &value
	}
	if config.AuthorFilter != nil {
		value := *config.AuthorFilter
		cloned.AuthorFilter = &value
	}
	if config.RequireReviewRequest != nil {
		value := *config.RequireReviewRequest
		cloned.RequireReviewRequest = &value
	}
	if config.EnableSelfReview != nil {
		value := *config.EnableSelfReview
		cloned.EnableSelfReview = &value
	}
	return &cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
