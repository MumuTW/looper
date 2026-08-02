package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var networkNodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var agentProfileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type ValidationIssue struct {
	Path    string
	Message string
}

type ConfigValidationError struct {
	Issues []ValidationIssue
}

type ValidateOptions struct {
	DefaultWorktreeRoot string
}

func (err *ConfigValidationError) Error() string {
	if err == nil || len(err.Issues) == 0 {
		return "config validation failed"
	}

	details := make([]string, 0, len(err.Issues))
	for _, issue := range err.Issues {
		details = append(details, strings.TrimSpace(issue.Path+" "+issue.Message))
	}
	return "config validation failed: " + strings.Join(details, "; ")
}

func Validate(config Config) error {
	return ValidateWithOptions(config, ValidateOptions{})
}

func ValidateWithOptions(config Config, options ValidateOptions) error {
	issues := make([]ValidationIssue, 0)

	validateCoreConfig(config, &issues)
	validatePlannerEscalation(config.Roles.Planner.Escalation, "roles.planner.escalation", &issues)

	if config.Roles.Reviewer.Behavior.Loop.QuietPeriodSeconds < 0 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.loop.quietPeriodSeconds", Message: "must be an integer >= 0"})
	}
	if config.Roles.Reviewer.Behavior.Loop.MinPublishIntervalSeconds < 0 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.loop.minPublishIntervalSeconds", Message: "must be an integer >= 0"})
	}
	validateReviewerConvergence(config.Roles.Reviewer.Behavior.Convergence, "roles.reviewer.behavior.convergence", &issues)
	if config.Roles.Reviewer.Behavior.Retry.AutoRecoveryMaxAttempts < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.retry.autoRecoveryMaxAttempts", Message: "must be a positive integer"})
	}
	if config.Roles.Reviewer.Behavior.Retry.MaxDelayMS < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.retry.maxDelayMs", Message: "must be a positive integer"})
	}
	for index, pattern := range config.Roles.Reviewer.Behavior.Retry.ExtraTransientErrorPatterns {
		if strings.TrimSpace(pattern) == "" {
			issues = append(issues, ValidationIssue{Path: fmt.Sprintf("roles.reviewer.behavior.retry.extraTransientErrorPatterns[%d]", index), Message: "must be a non-empty string"})
		}
	}
	if !isValidReviewerScope(config.Roles.Reviewer.Behavior.Scope) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.scope", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerScopeFullPR, ReviewerScopeChangedFiles, ReviewerScopeChangedRanges)})
	}
	if config.Roles.Reviewer.Behavior.PublishMode != ReviewerPublishModeSingleReview {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.publishMode", Message: fmt.Sprintf("must be %s", ReviewerPublishModeSingleReview)})
	}
	if !isValidReviewerThreadResolutionMode(config.Roles.Reviewer.Behavior.ThreadResolution.Mode) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.mode", Message: fmt.Sprintf("must be one of: %s, %s, %s, %s", ReviewerThreadResolutionModeReportOnly, ReviewerThreadResolutionModeCommentOnly, ReviewerThreadResolutionModeSuggestResolution, ReviewerThreadResolutionModeResolveObjective)})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.Scope != ReviewerThreadResolutionScopeLooperAuthoredOnly {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.scope", Message: fmt.Sprintf("must be %s", ReviewerThreadResolutionScopeLooperAuthoredOnly)})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.AutoResolve != ReviewerThreadResolutionAutoResolveObjectiveOnly {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.autoResolve", Message: fmt.Sprintf("must be %s", ReviewerThreadResolutionAutoResolveObjectiveOnly)})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.MaxThreadsPerRun < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.maxThreadsPerRun", Message: "must be a positive integer"})
	}
	if config.Roles.Reviewer.Behavior.ThreadResolution.Mode == ReviewerThreadResolutionModeResolveObjective && !config.Roles.Reviewer.Behavior.ThreadResolution.RequireAuditComment {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.threadResolution.requireAuditComment", Message: "must be true when mode is resolve_objective"})
	}
	if !isValidReviewerAutoMergeStrategy(config.Roles.Reviewer.AutoMerge.Strategy) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.autoMerge.strategy", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerAutoMergeStrategySquash, ReviewerAutoMergeStrategyMerge, ReviewerAutoMergeStrategyRebase)})
	}
	if config.Roles.Reviewer.AutoMerge.TransientRetries < 1 {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.autoMerge.transientRetries", Message: "must be a positive integer"})
	}
	if config.Roles.Reviewer.AutoMerge.Scope != ReviewerAutoMergeScopeLooperOnly {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.autoMerge.scope", Message: fmt.Sprintf("must be %s", ReviewerAutoMergeScopeLooperOnly)})
	}
	if config.Roles.Reviewer.Behavior.ReviewEvents.Clean != ReviewerReviewEventComment && config.Roles.Reviewer.Behavior.ReviewEvents.Clean != ReviewerReviewEventApprove {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.reviewEvents.clean", Message: fmt.Sprintf("must be one of: %s, %s", ReviewerReviewEventComment, ReviewerReviewEventApprove)})
	}
	if config.Roles.Reviewer.Behavior.ReviewEvents.Blocking != ReviewerReviewEventComment && config.Roles.Reviewer.Behavior.ReviewEvents.Blocking != ReviewerReviewEventRequestChanges {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.behavior.reviewEvents.blocking", Message: fmt.Sprintf("must be one of: %s, %s", ReviewerReviewEventComment, ReviewerReviewEventRequestChanges)})
	}

	validateInstructions(config, &issues)
	validateCoordinatorRoleConfig(config.Roles.Coordinator, "roles.coordinator", &issues)
	validateCodingRoleRegistry(config, &issues)
	if config.Roles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel && strings.TrimSpace(config.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel) == "" {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.discovery.specReview.reviewingLabel", Message: "must be a non-empty string when includeReviewingLabel is true"})
	} else if config.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel != strings.TrimSpace(config.Roles.Reviewer.Discovery.SpecReview.ReviewingLabel) {
		issues = append(issues, ValidationIssue{Path: "roles.reviewer.discovery.specReview.reviewingLabel", Message: "must not contain leading or trailing whitespace"})
	}

	providerIDs := make(map[string]ProviderKind, len(config.Providers))
	for index, provider := range config.Providers {
		prefix := fmt.Sprintf("providers[%d]", index)
		if strings.TrimSpace(provider.ID) == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: "must be a non-empty string"})
		} else if _, exists := providerIDs[provider.ID]; exists {
			issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: fmt.Sprintf("duplicate provider id: %s", provider.ID)})
		} else {
			providerIDs[provider.ID] = provider.Kind
		}
		if reason, removed := removedProviderKinds[provider.Kind]; removed {
			issues = append(issues, ValidationIssue{Path: prefix + ".kind", Message: fmt.Sprintf("provider kind %q is no longer supported: %s", provider.Kind, reason)})
		} else if !isValidProviderKind(provider.Kind) {
			issues = append(issues, ValidationIssue{Path: prefix + ".kind", Message: fmt.Sprintf("must be: %s", ProviderKindGitHub)})
		} else if message, ok := unsupportedProviderBaseURL(provider.BaseURL); !ok {
			issues = append(issues, ValidationIssue{Path: prefix + ".baseUrl", Message: message})
		}
	}

	projectIDs := make(map[string]struct{}, len(config.Projects))
	projectRepos := make(map[string]int, len(config.Projects))
	projectRepoPaths := make(map[string]int, len(config.Projects))
	for index, project := range config.Projects {
		prefix := fmt.Sprintf("projects[%d]", index)
		if strings.TrimSpace(project.Provider) != "" {
			if _, exists := providerIDs[project.Provider]; !exists {
				issues = append(issues, ValidationIssue{Path: prefix + ".provider", Message: fmt.Sprintf("references unknown provider id: %s", project.Provider)})
			}
		}

		if project.ID == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: "must be a non-empty string"})
		} else if !isValidConfiguredProjectID(project.ID) {
			issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: getConfigProjectIDValidationMessage()})
		} else {
			if _, exists := projectIDs[project.ID]; exists {
				issues = append(issues, ValidationIssue{Path: prefix + ".id", Message: fmt.Sprintf("duplicate project id: %s", project.ID)})
			} else {
				projectIDs[project.ID] = struct{}{}
			}
		}

		if project.Name == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".name", Message: "must be a non-empty string"})
		}

		if project.RepoPath == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".repoPath", Message: "must be a non-empty path"})
		} else {
			cleanRepoPath := filepath.Clean(project.RepoPath)
			if previousIndex, exists := projectRepoPaths[cleanRepoPath]; exists {
				issues = append(issues, ValidationIssue{Path: prefix + ".repoPath", Message: fmt.Sprintf("duplicates projects[%d].repoPath: %s", previousIndex, project.RepoPath)})
			} else {
				projectRepoPaths[cleanRepoPath] = index
			}
		}
		if strings.TrimSpace(project.Provider) != "" && strings.TrimSpace(project.Repo) == "" {
			// A provider binding names the remote a project's automation targets.
			// Without repo the project materializes with no remote and every lane
			// silently skips it, so reject it here instead of starting inert.
			issues = append(issues, ValidationIssue{Path: prefix + ".repo", Message: "is required for a project bound to an explicit provider"})
		}
		if strings.TrimSpace(project.Repo) != "" {
			identity, resolved := ProjectRepositoryIdentity(config, project)
			if previousIndex, exists := projectRepos[identity.Key()]; resolved && exists {
				issues = append(issues, ValidationIssue{Path: prefix + ".repo", Message: fmt.Sprintf("duplicates projects[%d].repo: %s", previousIndex, project.Repo)})
			} else if resolved {
				projectRepos[identity.Key()] = index
			}
		}
		if project.Path != "" && project.RepoPath != "" && project.Path != project.RepoPath {
			issues = append(issues, ValidationIssue{Path: prefix + ".path", Message: "must match repoPath when both path and repoPath are set"})
		}
		if !isValidWebhookModeOrEmpty(project.Webhook.Mode) {
			issues = append(issues, ValidationIssue{Path: prefix + ".webhook.mode", Message: fmt.Sprintf("must be one of: %s, %s", WebhookModeGHForward, WebhookModeTunnel)})
		}
		if !isValidNetworkMode(project.Network.Mode) {
			issues = append(issues, ValidationIssue{Path: prefix + ".network.mode", Message: fmt.Sprintf("must be one of: %s, %s", NetworkModeOff, NetworkModeRouted)})
		}
		if config.Webhook.Enabled && webhookModeRequiresTunnelConfig(config, &project) {
			validateWebhookTunnelConfig(config.Webhook, "webhook", &issues)
		}
		validateProjectValidationConfig(config, project, prefix, false, &issues)

		validateProjectRoleOverrides(project.Roles, prefix+".roles", config.Instructions.MaxBytes, &issues)
		validateProjectRoleAgentBindings(project.Roles, prefix+".roles", &issues)
		effectiveProjectRoles := ProjectRoleConfigs(config, project.ID)
		for _, roleInstruction := range roleInstructions(effectiveProjectRoles) {
			if !projectRoleInstructionsConfigured(project.Roles, roleInstruction.role) {
				continue
			}
			path := fmt.Sprintf("%s.roles.%s.instructions", prefix, roleInstruction.role)
			validateInstructionText(path, roleInstruction.role, roleInstruction.text, config.Instructions.MaxBytes, &issues)
		}
		if effectiveProjectRoles.Reviewer.Discovery.SpecReview.IncludeReviewingLabel && strings.TrimSpace(effectiveProjectRoles.Reviewer.Discovery.SpecReview.ReviewingLabel) == "" {
			issues = append(issues, ValidationIssue{Path: prefix + ".roles.reviewer.discovery.specReview.reviewingLabel", Message: "must be a non-empty string when includeReviewingLabel is true"})
		}
		if project.Roles != nil && project.Roles.Coordinator != nil {
			validateCoordinatorRoleConfig(effectiveProjectRoles.Coordinator, prefix+".roles.coordinator", &issues)
		}
		// The effective per-project mode needs its own check: the global check
		// above cannot see a project override, and the removed summary_comment
		// implementation would otherwise fall through to single_review silently.
		if effectiveProjectRoles.Reviewer.Behavior.PublishMode != ReviewerPublishModeSingleReview {
			issues = append(issues, ValidationIssue{Path: prefix + ".roles.reviewer.behavior.publishMode", Message: fmt.Sprintf("must be %s", ReviewerPublishModeSingleReview)})
		}
		if normalizeNetworkMode(project.Network.Mode) == NetworkModeRouted {
			validateRoutedProjectPrerequisites(config, effectiveProjectRoles, prefix, &issues)
		}
	}

	if len(issues) > 0 {
		return &ConfigValidationError{Issues: issues}
	}

	defaultWorktreeRoot := options.DefaultWorktreeRoot
	if defaultWorktreeRoot == "" {
		resolvedDefaultWorktreeRoot, err := DefaultWorktreeRoot()
		if err != nil {
			return fmt.Errorf("determine default worktree root: %w", err)
		}

		defaultWorktreeRoot = resolvedDefaultWorktreeRoot
	}

	ensureWritablePath(config.Storage.DBPath, writablePathFileParent, &issues, "storage.dbPath")
	ensureWritablePath(config.Daemon.LogDir, writablePathDirectory, &issues, "daemon.logDir")
	ensureWritablePath(config.Daemon.WorkingDirectory, writablePathDirectory, &issues, "daemon.workingDirectory")
	ensureWritablePath(defaultWorktreeRoot, writablePathDirectory, &issues, "defaults.worktreeRoot")

	if len(issues) > 0 {
		return &ConfigValidationError{Issues: issues}
	}

	return nil
}

// validateCoreConfig preserves the validation and issue ordering for settings
// that apply before role, provider, and project validation.
func validateCoreConfig(config Config, issues *[]ValidationIssue) {
	validateServerConfig(config.Server, issues)
	validateStorageConfig(config.Storage, issues)
	validateSchedulerConfig(config.Scheduler, issues)
	validateWebhookConfig(config, issues)
	validateAgentConfig(config, issues)
	validateLoggingAndNotificationConfig(config, issues)
	validateHITLConfig(config.HITL, issues)
	validateGatekeeperRoleConfig(config.Roles.Gatekeeper, "roles.gatekeeper", config.Roles.Reviewer.AutoMerge.Enabled, issues)
	validateAuditorRoleConfig(config.Roles.Auditor, "roles.auditor", issues)
	validateDeployerRoleConfig(config.Roles.Deployer, "roles.deployer", issues)
	validateEscalatorRoleConfig(config.Roles.Escalator, "roles.escalator", issues)
	for i, project := range config.Projects {
		if project.Roles == nil || project.Roles.Deployer == nil {
			continue
		}
		role := config.Roles.Deployer
		MergeDeployerRoleConfig(&role, *project.Roles.Deployer)
		validateDeployerRoleConfig(role, fmt.Sprintf("projects[%d].roles.deployer", i), issues)
	}
	for i, project := range config.Projects {
		if project.Roles == nil || project.Roles.Auditor == nil {
			continue
		}
		role := config.Roles.Auditor
		if project.Roles.Auditor.Enabled != nil {
			role.Enabled = *project.Roles.Auditor.Enabled
		}
		if project.Roles.Auditor.WindowMinutes != nil {
			role.WindowMinutes = *project.Roles.Auditor.WindowMinutes
		}
		validateAuditorRoleConfig(role, fmt.Sprintf("projects[%d].roles.auditor", i), issues)
	}
	for i, project := range config.Projects {
		if project.Roles == nil || project.Roles.Gatekeeper == nil || (project.Roles.Gatekeeper.Trust == nil && project.Roles.Gatekeeper.DiffBudget == nil) {
			continue
		}
		reviewerAutoMerge := config.Roles.Reviewer.AutoMerge.Enabled
		if project.Roles.Reviewer != nil && project.Roles.Reviewer.AutoMerge != nil && project.Roles.Reviewer.AutoMerge.Enabled != nil {
			reviewerAutoMerge = *project.Roles.Reviewer.AutoMerge.Enabled
		}
		if project.Roles.Gatekeeper.Trust != nil {
			validateGatekeeperRoleConfig(
				GatekeeperRoleConfig{Trust: *project.Roles.Gatekeeper.Trust},
				fmt.Sprintf("projects[%d].roles.gatekeeper", i), reviewerAutoMerge, issues)
		}
		validatePartialGatekeeperDiffBudget(
			project.Roles.Gatekeeper.DiffBudget,
			fmt.Sprintf("projects[%d].roles.gatekeeper.diffBudget", i), issues)
	}
	validateIntakeConfig(config, issues)
	validateDaemonConfig(config.Daemon, issues)
	validatePackageAndDefaultsConfig(config, issues)
}

func validateEscalatorRoleConfig(role EscalatorRoleConfig, path string, issues *[]ValidationIssue) {
	if role.CadenceSeconds < 60 {
		*issues = append(*issues, ValidationIssue{Path: path + ".cadenceSeconds", Message: "must be an integer >= 60"})
	}
	if role.RetryAttemptThreshold < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".retryAttemptThreshold", Message: "must be an integer >= 1"})
	}
	if role.UnroutedAfterSeconds < 60 {
		*issues = append(*issues, ValidationIssue{Path: path + ".unroutedAfterSeconds", Message: "must be an integer >= 60"})
	}
	if role.StaleHeadAfterSeconds < 60 {
		*issues = append(*issues, ValidationIssue{Path: path + ".staleHeadAfterSeconds", Message: "must be an integer >= 60"})
	}
	if role.MaxItems < 1 || role.MaxItems > 5000 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxItems", Message: "must be an integer between 1 and 5000"})
	}
}

func validateAuditorRoleConfig(auditor AuditorRoleConfig, path string, issues *[]ValidationIssue) {
	if auditor.Enabled && auditor.WindowMinutes <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".windowMinutes", Message: "must be a positive integer when auditor is enabled"})
	}
}

func validateServerConfig(server ServerConfig, issues *[]ValidationIssue) {
	if server.Host == "" {
		*issues = append(*issues, ValidationIssue{Path: "server.host", Message: "must be a non-empty string"})
	}
	if server.Port < 1 || server.Port > 65535 {
		*issues = append(*issues, ValidationIssue{Path: "server.port", Message: "must be an integer between 1 and 65535"})
	}
	if !isValidAuthMode(server.AuthMode) {
		*issues = append(*issues, ValidationIssue{Path: "server.authMode", Message: fmt.Sprintf("must be one of: %s, %s", AuthModeNone, AuthModeLocalToken)})
	}
	// Token-less mode is limited to literal loopback binds; remote clients behind
	// a loopback proxy cannot be distinguished safely from direct local clients.
	if server.AuthMode == AuthModeNone && server.Host != "" && !isLoopbackBindHost(server.Host) {
		*issues = append(*issues, ValidationIssue{Path: "server.authMode", Message: "none is allowed only when server.host is localhost or a loopback IP; use local-token for wildcard, LAN, public, proxy, or custom-hostname binds"})
	}
	if server.AuthMode == AuthModeLocalToken && isNilOrEmptyString(server.LocalToken) {
		*issues = append(*issues, ValidationIssue{Path: "server.localToken", Message: "is required when authMode is local-token"})
	}
	if server.BaseURL != nil && strings.TrimSpace(*server.BaseURL) != "" {
		canonical, err := CanonicalizeServerBaseURL(*server.BaseURL)
		if err != nil {
			*issues = append(*issues, ValidationIssue{Path: "server.baseUrl", Message: err.Error()})
		} else {
			// The canonical form is the single authority consumers (CLI dialing,
			// browser Host/Origin allowlisting) read. The load pipeline stores
			// it via Normalize, but configs constructed directly bypass that
			// step and Validate only computes canonical without storing it, so
			// a raw spelling like "https://daemon.example:0443" would pass here
			// while allowedAuthorities records port "0443" and the browser omits
			// the default :443. Require the stored value to already be canonical
			// at this boundary so direct-config paths cannot diverge.
			if *server.BaseURL != canonical {
				*issues = append(*issues, ValidationIssue{Path: "server.baseUrl", Message: "must be in canonical form; the load pipeline canonicalizes server.baseUrl, but configs constructed directly must set the canonical value (lowercase scheme/host, no trailing slash, default ports omitted) themselves"})
			} else if server.AuthMode == AuthModeNone {
				// A non-loopback advertised authority means a proxy or tunnel
				// fronts the daemon; the token-less loopback trust model does not
				// survive that hop (a local proxy can deliver remote requests with
				// a loopback peer address and the configured public Host/Origin).
				if parsed, parseErr := url.Parse(canonical); parseErr == nil && !isLoopbackBindHost(parsed.Hostname()) {
					*issues = append(*issues, ValidationIssue{Path: "server.authMode", Message: "none is allowed only when server.baseUrl advertises a loopback authority; use local-token when a proxy, tunnel, or public hostname fronts the daemon"})
				}
			}
		}
	}
}

func validateStorageConfig(storage StorageConfig, issues *[]ValidationIssue) {
	if storage.Mode != "sqlite" {
		*issues = append(*issues, ValidationIssue{Path: "storage.mode", Message: "must be sqlite"})
	}
	if storage.DBPath == "" {
		*issues = append(*issues, ValidationIssue{Path: "storage.dbPath", Message: "must be a non-empty path"})
	}
}

func validateSchedulerConfig(scheduler SchedulerConfig, issues *[]ValidationIssue) {
	if scheduler.PollIntervalSeconds < 10 {
		*issues = append(*issues, ValidationIssue{Path: "scheduler.pollIntervalSeconds", Message: "must be an integer >= 10"})
	}
	if scheduler.MaxConcurrentRuns < 1 {
		*issues = append(*issues, ValidationIssue{Path: "scheduler.maxConcurrentRuns", Message: "must be a positive integer"})
	}
	if scheduler.RetryMaxAttempts == 0 || scheduler.RetryMaxAttempts < -1 {
		*issues = append(*issues, ValidationIssue{Path: "scheduler.retryMaxAttempts", Message: "must be -1 or a positive integer"})
	}
	if scheduler.ConsecutiveFailureThreshold < 1 {
		*issues = append(*issues, ValidationIssue{Path: "scheduler.consecutiveFailureThreshold", Message: "must be a positive integer"})
	}
	if scheduler.RetryBaseDelayMS < 1 {
		*issues = append(*issues, ValidationIssue{Path: "scheduler.retryBaseDelayMs", Message: "must be a positive integer"})
	}
	if scheduler.SlowLaneWarnThresholdMS < 1 {
		*issues = append(*issues, ValidationIssue{Path: "scheduler.slowLaneWarnThresholdMs", Message: "must be a positive integer"})
	}
	if scheduler.DiscoveryCacheTTLSeconds < 0 {
		*issues = append(*issues, ValidationIssue{Path: "scheduler.discoveryCacheTtlSeconds", Message: "must be an integer >= 0"})
	}
}

func validateWebhookConfig(config Config, issues *[]ValidationIssue) {
	if config.Webhook.FallbackPollIntervalSeconds < 60 {
		*issues = append(*issues, ValidationIssue{Path: "webhook.fallbackPollIntervalSeconds", Message: "must be an integer >= 60"})
	}
	if !isValidWebhookMode(config.Webhook.Mode) {
		*issues = append(*issues, ValidationIssue{Path: "webhook.mode", Message: fmt.Sprintf("must be one of: %s, %s", WebhookModeGHForward, WebhookModeTunnel)})
	}
	if config.Webhook.Enabled && webhookModeRequiresTunnelConfig(config, nil) {
		validateWebhookTunnelConfig(config.Webhook, "webhook", issues)
	}
}

func validateAgentConfig(config Config, issues *[]ValidationIssue) {
	if config.Agent.Vendor != nil && !isValidAgentVendor(*config.Agent.Vendor) {
		*issues = append(*issues, ValidationIssue{Path: "agent.vendor", Message: agentVendorValidationMessage()})
	}
	validateAgentProfiles(config.Agent.Profiles, issues)
	validateEnvironmentNames(config.Agent.Env, "agent.env", issues)
	validateAgentTimeouts(config.Agent.Timeouts, "agent.timeouts", issues)
}

func validateLoggingAndNotificationConfig(config Config, issues *[]ValidationIssue) {
	if !isValidLogLevel(config.Logging.Level) {
		*issues = append(*issues, ValidationIssue{Path: "logging.level", Message: fmt.Sprintf("must be one of: %s, %s, %s, %s", LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError)})
	}
	if config.Logging.MaxSizeMB < 1 {
		*issues = append(*issues, ValidationIssue{Path: "logging.maxSizeMB", Message: "must be a positive integer"})
	}
	if config.Logging.MaxFiles < 1 {
		*issues = append(*issues, ValidationIssue{Path: "logging.maxFiles", Message: "must be a positive integer"})
	}
	if config.Notifications.Osascript.ThrottleWindowSeconds < 1 {
		*issues = append(*issues, ValidationIssue{Path: "notifications.osascript.throttleWindowSeconds", Message: "must be a positive integer"})
	}
	for _, level := range config.Notifications.Osascript.SoundForLevels {
		if !isValidNotificationSoundLevel(level) {
			*issues = append(*issues, ValidationIssue{Path: "notifications.osascript.soundForLevels", Message: fmt.Sprintf("contains unsupported value: %s", level)})
		}
	}
	switch strings.TrimSpace(config.Notifications.Webhook.Mode) {
	case "", "webhook":
	case "app":
		if config.Notifications.Webhook.Enabled {
			if strings.TrimSpace(config.Notifications.Webhook.AppIDEnv) == "" {
				*issues = append(*issues, ValidationIssue{Path: "notifications.webhook.appIdEnv", Message: "is required when notifications.webhook.mode is app"})
			}
			if strings.TrimSpace(config.Notifications.Webhook.AppSecretEnv) == "" {
				*issues = append(*issues, ValidationIssue{Path: "notifications.webhook.appSecretEnv", Message: "is required when notifications.webhook.mode is app"})
			}
			if strings.TrimSpace(config.Notifications.Webhook.ChatID) == "" {
				*issues = append(*issues, ValidationIssue{Path: "notifications.webhook.chatId", Message: "is required when notifications.webhook.mode is app"})
			}
		}
	default:
		*issues = append(*issues, ValidationIssue{Path: "notifications.webhook.mode", Message: "must be one of: webhook, app"})
	}
}

// validateIntakeConfig fails startup rather than runtime. An intake bot whose
// default project does not exist accepts every message and then rejects it, so
// the mistake is only visible after someone has typed a request and lost it.
func validateIntakeConfig(config Config, issues *[]ValidationIssue) {
	tg := config.Intake.Telegram
	if tg == nil || !tg.Enabled {
		return
	}
	if strings.TrimSpace(tg.BotTokenEnv) == "" {
		*issues = append(*issues, ValidationIssue{Path: "intake.telegram.botTokenEnv", Message: "is required when intake.telegram.enabled is true"})
	} else if !environmentNamePattern.MatchString(strings.TrimSpace(tg.BotTokenEnv)) {
		*issues = append(*issues, ValidationIssue{Path: "intake.telegram.botTokenEnv", Message: "must be a valid environment-variable name, not a token value"})
	}
	if len(tg.AllowedUserIDs) == 0 {
		*issues = append(*issues, ValidationIssue{Path: "intake.telegram.allowedUserIds", Message: "must list at least one Telegram user id when intake.telegram.enabled is true"})
	}
	for i, id := range tg.AllowedUserIDs {
		// Telegram user ids are positive. Negative values are group/channel chat
		// ids, which people routinely collect during setup and paste here; such a
		// value would silently reject every real sender.
		if id <= 0 {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("intake.telegram.allowedUserIds[%d]", i), Message: "must be a positive Telegram user id (a negative value is a chat id, not a user id)"})
		}
	}
	defaultProject := strings.TrimSpace(tg.DefaultProjectID)
	if defaultProject == "" {
		*issues = append(*issues, ValidationIssue{Path: "intake.telegram.defaultProjectId", Message: "is required when intake.telegram.enabled is true"})
		return
	}
	for _, project := range config.Projects {
		if strings.EqualFold(strings.TrimSpace(project.ID), defaultProject) {
			return
		}
	}
	*issues = append(*issues, ValidationIssue{Path: "intake.telegram.defaultProjectId", Message: fmt.Sprintf("must name a configured project; %q is not in projects[]", defaultProject)})
}

// validateGatekeeperRoleConfig rejects a trust level Looper cannot honour.
//
// "auto" is rejected rather than accepted-and-ignored on purpose: a merge
// authority that silently behaves one level below what the operator configured
// is the worst possible failure for this setting.
// validateDeployerRoleConfig fails startup rather than at deploy time. A project
// configured to deploy but unable to is otherwise only discovered on the first
// merge, which is the worst moment to learn it.
func validateDeployerRoleConfig(deployerRole DeployerRoleConfig, path string, issues *[]ValidationIssue) {
	if !deployerRole.Enabled {
		return
	}
	if strings.TrimSpace(deployerRole.Command) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".command", Message: "is required when the deployer is enabled"})
	}
	if deployerRole.TimeoutSeconds < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".timeoutSeconds", Message: "must not be negative"})
	}
	validateEnvironmentNames(deployerRole.Environment, path+".environment", issues)
}

func validateGatekeeperRoleConfig(gatekeeper GatekeeperRoleConfig, path string, reviewerAutoMerge bool, issues *[]ValidationIssue) {
	validateGatekeeperDiffBudget(gatekeeper.DiffBudget, path+".diffBudget", issues)
	switch GatekeeperTrustLevel(strings.ToLower(strings.TrimSpace(string(gatekeeper.Trust)))) {
	case "", GatekeeperTrustObserve, GatekeeperTrustAdvise:
	case GatekeeperTrustAuto:
		// Two merge authorities acting on the same pull request is not a
		// configuration anyone can reason about: whichever wins the race decides,
		// and Reviewer's path checks a strictly narrower set of gates.
		if reviewerAutoMerge {
			*issues = append(*issues, ValidationIssue{
				Path:    path + ".trust",
				Message: fmt.Sprintf("%q cannot be combined with roles.reviewer.autoMerge.enabled: disable one, and prefer Gatekeeper because it also gates on unresolved review threads and requested changes", GatekeeperTrustAuto),
			})
		}
	default:
		*issues = append(*issues, ValidationIssue{
			Path:    path + ".trust",
			Message: fmt.Sprintf("must be one of: %s, %s, %s", GatekeeperTrustObserve, GatekeeperTrustAdvise, GatekeeperTrustAuto),
		})
	}
}

func validateGatekeeperDiffBudget(budget *GatekeeperDiffBudget, path string, issues *[]ValidationIssue) {
	if budget == nil {
		return
	}
	if budget.MaxChangedFiles < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxChangedFiles", Message: "must be zero or a positive integer"})
	}
	if budget.MaxDeletions < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxDeletions", Message: "must be zero or a positive integer"})
	}
}

func validatePartialGatekeeperDiffBudget(budget *PartialGatekeeperDiffBudget, path string, issues *[]ValidationIssue) {
	if budget == nil {
		return
	}
	if budget.MaxChangedFiles != nil && *budget.MaxChangedFiles < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxChangedFiles", Message: "must be zero or a positive integer"})
	}
	if budget.MaxDeletions != nil && *budget.MaxDeletions < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxDeletions", Message: "must be zero or a positive integer"})
	}
}

func validateHITLConfig(hitl HITLConfig, issues *[]ValidationIssue) {
	switch strings.ToLower(strings.TrimSpace(hitl.AnswerTransport)) {
	case "", "github", "respond":
	case "feishu":
		if hitl.Feishu == nil {
			*issues = append(*issues, ValidationIssue{Path: "hitl.feishu", Message: "is required when hitl.answerTransport is feishu"})
			return
		}
		if !strings.EqualFold(strings.TrimSpace(hitl.Feishu.Inbound), "cf-inbox") {
			*issues = append(*issues, ValidationIssue{Path: "hitl.feishu.inbound", Message: "must be cf-inbox when hitl.answerTransport is feishu"})
		}
		if strings.TrimSpace(hitl.Feishu.EventInboxURLEnv) == "" {
			*issues = append(*issues, ValidationIssue{Path: "hitl.feishu.eventInboxUrlEnv", Message: "is required when hitl.answerTransport is feishu"})
		}
		if strings.TrimSpace(hitl.Feishu.EventInboxTokenEnv) == "" {
			*issues = append(*issues, ValidationIssue{Path: "hitl.feishu.eventInboxTokenEnv", Message: "is required when hitl.answerTransport is feishu"})
		}
	default:
		*issues = append(*issues, ValidationIssue{Path: "hitl.answerTransport", Message: "must be one of: github, feishu, respond"})
	}
}

func validateDaemonConfig(daemon DaemonConfig, issues *[]ValidationIssue) {
	if !isValidDaemonMode(daemon.Mode) {
		*issues = append(*issues, ValidationIssue{Path: "daemon.mode", Message: fmt.Sprintf("must be one of: %s, %s, %s", DaemonModeForeground, DaemonModeLaunchd, DaemonModeSystemd)})
	}
	validateEnvironmentNames(daemon.Environment, "daemon.environment", issues)
	if !isValidDaemonRestartPolicy(daemon.RestartPolicy) {
		*issues = append(*issues, ValidationIssue{Path: "daemon.restartPolicy", Message: fmt.Sprintf("must be one of: %s, %s, %s", DaemonRestartNever, DaemonRestartOnFailure, DaemonRestartAlways)})
	}
	if daemon.RestartThrottleSeconds < 1 {
		*issues = append(*issues, ValidationIssue{Path: "daemon.restartThrottleSeconds", Message: "must be a positive integer"})
	}
	if daemon.LogDir == "" {
		*issues = append(*issues, ValidationIssue{Path: "daemon.logDir", Message: "must be a non-empty path"})
	}
	if daemon.ShutdownTimeoutMS < 1 {
		*issues = append(*issues, ValidationIssue{Path: "daemon.shutdownTimeoutMs", Message: "must be a positive integer"})
	}
	if daemon.WorkingDirectory == "" {
		*issues = append(*issues, ValidationIssue{Path: "daemon.workingDirectory", Message: "must be a non-empty path"})
	}
	validateWorktreeCleanupConfig(daemon.WorktreeCleanup, "daemon.worktreeCleanup", issues)
	validateResourceGuardConfig(daemon.ResourceGuard, "daemon.resourceGuard", issues)
}

// ValidateProjectValidationPolicies is the startup/catalog/reload gate. Generic
// config parsing validates any authored policy, but only an authority boundary
// has enough context to require every materialized project to choose commands
// or an explicit opt-out.
func ValidateProjectValidationPolicies(config Config) error {
	if !CodingRoleAgentConfigured(config, CodingRoleWorker) && !CodingRoleAgentConfigured(config, CodingRoleFixer) {
		return nil
	}
	issues := []ValidationIssue{}
	for index, project := range config.Projects {
		validateProjectValidationConfig(config, project, fmt.Sprintf("projects[%d]", index), true, &issues)
	}
	if len(issues) > 0 {
		return &ConfigValidationError{Issues: issues}
	}
	return nil
}

// ValidateProjectValidationPolicy validates an explicitly authored project
// policy independently of whether a coding agent is currently configured.
// Mutation boundaries use this before persistence so enabling Worker or Fixer
// later cannot turn an accepted stored policy into a startup failure.
func ValidateProjectValidationPolicy(validation *ProjectValidationConfig) error {
	issues := []ValidationIssue{}
	validateProjectValidationConfig(Config{}, ProjectRefConfig{Validation: validation}, "project", false, &issues)
	if len(issues) > 0 {
		return &ConfigValidationError{Issues: issues}
	}
	return nil
}

func validateProjectValidationConfig(config Config, project ProjectRefConfig, prefix string, requirePresence bool, issues *[]ValidationIssue) {
	validation := project.Validation
	if validation == nil {
		if len(ResolveValidationCommands(config)) > 0 {
			// The project has no authored policy but inherits the deprecated
			// defaults.validationCommands fallback, so the gate is active:
			// worker/fixer resolve these commands and run with
			// RestrictToolNetwork. The same vendor capability check applies, or
			// an unsupported vendor passes startup and fails at every spawn
			// instead of failing fast here.
			validateValidationVendorSupport(config, project, prefix, issues)
			return
		}
		if requirePresence {
			*issues = append(*issues, ValidationIssue{
				Path:    prefix + ".validation",
				Message: "must configure commands or set optOut=true; defaults.validationCommands is only a legacy migration fallback",
			})
		}
		return
	}

	if validation.OptOut {
		if len(validation.Commands) > 0 {
			*issues = append(*issues, ValidationIssue{Path: prefix + ".validation", Message: "cannot set commands when optOut=true"})
		}
		return
	}
	if len(validation.Commands) == 0 {
		*issues = append(*issues, ValidationIssue{Path: prefix + ".validation.commands", Message: "must contain at least one command or set optOut=true"})
		return
	}
	for index, command := range validation.Commands {
		if strings.TrimSpace(command) == "" {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s.validation.commands[%d]", prefix, index), Message: "must be a non-empty string"})
		}
	}
	validateValidationVendorSupport(config, project, prefix, issues)
}

// validateValidationVendorSupport rejects a project whose validation gate can
// never run: the gate spawns the worker/fixer agent with its tool subprocesses
// denied network access, and a vendor whose CLI cannot express that is refused
// at spawn time on every attempt. Catching it here turns a repeating runtime
// failure into one startup error naming the fix.
//
// Agent bindings are global-only (validateProjectRoleAgentBindings rejects
// projects[].roles.<role>.agent), so the effective vendor comes from
// roles.<role>.agent / agent.vendor and is the same for every project.
func validateValidationVendorSupport(config Config, project ProjectRefConfig, prefix string, issues *[]ValidationIssue) {
	supported := make([]string, 0, len(ToolNetworkDenialVendors()))
	for _, vendor := range ToolNetworkDenialVendors() {
		supported = append(supported, string(vendor))
	}
	for _, role := range []string{CodingRoleWorker, CodingRoleFixer} {
		resolved, ok := ResolveAgent(config, project.ID, role)
		if !ok || VendorSupportsToolNetworkDenial(resolved.Vendor) {
			continue
		}
		*issues = append(*issues, ValidationIssue{
			Path: fmt.Sprintf("%s.validation.commands", prefix),
			Message: fmt.Sprintf(
				"cannot be enforced for project %q with roles.%s.agent.vendor=%s: the validation gate runs the agent with tool network access denied, which only %s support; set roles.%s.agent.vendor to one of them or set %s.validation.optOut=true",
				firstNonEmptyProjectName(project), role, resolved.Vendor, strings.Join(supported, ", "), role, prefix,
			),
		})
	}
}

func firstNonEmptyProjectName(project ProjectRefConfig) string {
	if id := strings.TrimSpace(project.ID); id != "" {
		return id
	}
	if repo := strings.TrimSpace(project.Repo); repo != "" {
		return repo
	}
	return strings.TrimSpace(project.RepoPath)
}

func validatePackageAndDefaultsConfig(config Config, issues *[]ValidationIssue) {
	if strings.TrimSpace(config.Package.Distribution) == "" {
		*issues = append(*issues, ValidationIssue{Path: "package.distribution", Message: "must be a non-empty string"})
	}
	if config.Defaults.BaseBranch == "" {
		*issues = append(*issues, ValidationIssue{Path: "defaults.baseBranch", Message: "must be a non-empty string"})
	}
	if !isValidOpenPRStrategy(config.Defaults.OpenPRStrategy) {
		*issues = append(*issues, ValidationIssue{Path: "defaults.openPrStrategy", Message: fmt.Sprintf("must be one of: %s, %s, %s", OpenPRStrategyAllDone, OpenPRStrategyFirstCommit, OpenPRStrategyManual)})
	}
	if !isValidAddSnapshotMode(config.Defaults.AddSnapshotMode) {
		*issues = append(*issues, ValidationIssue{Path: "defaults.addSnapshotMode", Message: fmt.Sprintf("must be one of: %s, %s, %s", AddSnapshotModeAsync, AddSnapshotModeFull, AddSnapshotModeOff)})
	}
	for index, command := range config.Defaults.ValidationCommands {
		if strings.TrimSpace(command) == "" {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("defaults.validationCommands[%d]", index), Message: "must be a non-empty string"})
		}
	}
}

func isLoopbackBindHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func validateRoutedProjectPrerequisites(config Config, roles RoleConfigs, prefix string, issues *[]ValidationIssue) {
	if !config.Network.Enrolled {
		*issues = append(*issues, ValidationIssue{Path: "network.enrolled", Message: fmt.Sprintf("must be true when %s.network.mode is %s; join a Network or set the project back to %s", prefix, NetworkModeRouted, NetworkModeOff)})
	}
	parsedLoopernetURL, err := url.Parse(strings.TrimSpace(config.Network.LoopernetBaseURL))
	if err != nil || parsedLoopernetURL.Scheme == "" || parsedLoopernetURL.Host == "" {
		*issues = append(*issues, ValidationIssue{Path: "network.loopernetBaseUrl", Message: fmt.Sprintf("must be an absolute URL with a host when %s.network.mode is %s", prefix, NetworkModeRouted)})
	}
	if err := validateNetworkNodeName(config.Network.NodeName); err != nil {
		*issues = append(*issues, ValidationIssue{Path: "network.nodeName", Message: fmt.Sprintf("%v when %s.network.mode is %s", err, prefix, NetworkModeRouted)})
	}
	if config.Network.GitHubUserID < 0 {
		*issues = append(*issues, ValidationIssue{Path: "network.githubUserId", Message: "must be a positive integer when configured"})
	}
	if strings.TrimSpace(config.Network.GitHubLogin) == "" {
		*issues = append(*issues, ValidationIssue{Path: "network.githubLogin", Message: fmt.Sprintf("must be configured when %s.network.mode is %s so routed claims can fall back when numeric GitHub IDs are unavailable", prefix, NetworkModeRouted)})
	}
	registry := EffectiveCodingRoles(roles)
	if planner := registry[CodingRolePlanner]; planner.Discovery.Enabled {
		*issues = append(*issues, ValidationIssue{Path: prefix + ".roles.planner.autoDiscovery", Message: "must be false for routed projects; planner routed execution is not supported yet"})
	}
	if fixer := registry[CodingRoleFixer]; fixer.Discovery.Enabled {
		*issues = append(*issues, ValidationIssue{Path: prefix + ".roles.fixer.autoDiscovery", Message: "must be false for routed projects; fixer routed execution is not supported yet"})
	}
}

func isValidNetworkMode(mode NetworkMode) bool {
	return normalizeNetworkMode(mode) == NetworkModeOff || normalizeNetworkMode(mode) == NetworkModeRouted
}

func isValidProviderKind(kind ProviderKind) bool {
	return kind == ProviderKindGitHub
}

// CanonicalizeServerBaseURL validates value as the daemon's advertised base
// URL and returns its canonical form: lowercase http(s) scheme and host, a
// required host, no userinfo, query, or fragment, and an absolute path with
// no trailing slash and no empty, ".", or ".." segments. Every consumer of
// server.baseUrl (CLI dialing, browser Host/Origin allowlisting, webhook
// endpoint display) reads the canonical form the load pipeline stores, so
// path concatenation and origin comparison cannot diverge.
func CanonicalizeServerBaseURL(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("must be an absolute http(s) URL")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("must be a parseable absolute http(s) URL")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("must use the http or https scheme")
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", errors.New("must include a host")
	}
	// An unbracketed colon in the host is malformed: url.Parse splits the
	// authority on the final colon, so "localhost:80:90" yields host
	// "localhost:80" port "90" and "::1" yields host ":" port "1". Go's
	// transport would then dial invalid targets like "[localhost:80]:90" or
	// "[:]:1", while the stored authority preserves the misparse. Only
	// bracketed IPv6 literals may carry colons.
	if hostname := parsed.Hostname(); strings.Contains(hostname, ":") && !strings.HasPrefix(parsed.Host, "[") {
		return "", errors.New("must not use a colon-bearing host; bracket IPv6 literals as in http://[::1]")
	}
	// Go's transport and browsers send the IDNA/Punycode authority on the
	// wire, so a stored Unicode host would never match Host/Origin checks.
	for _, r := range parsed.Hostname() {
		if r > 127 {
			return "", errors.New("must use an ASCII host; write internationalized domains in their IDNA/punycode form")
		}
	}
	// A wildcard advertised address is not dialable; the CLI's wildcard-to-
	// loopback mapping applies only to the bind host, never to baseUrl.
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && ip.IsUnspecified() {
		return "", errors.New("must not use an unspecified (wildcard) host such as 0.0.0.0 or ::")
	}
	// Numeric spellings like 0, 00, 0x0, or 0.0.0 are IPv4 integers to a
	// browser (which canonicalizes them, 0 included, to dotted-quad) but plain
	// hostnames to Go, so the two sides would disagree about the authority.
	if isNumericIPv4Spelling(parsed.Hostname()) && net.ParseIP(parsed.Hostname()) == nil {
		return "", errors.New("must spell IPv4 hosts in canonical dotted-quad form")
	}
	portNumber := 0
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("must use a port between 1 and 65535")
		}
		portNumber = number
	}
	if parsed.User != nil {
		return "", errors.New("must not include userinfo credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("must not include a query string")
	}
	if parsed.Fragment != "" {
		return "", errors.New("must not include a fragment")
	}

	// The dashboard requests API routes and assets by absolute path, so a
	// path-prefixed advertised URL would work for the CLI but break the
	// browser surface. Reject the prefix until the dashboard is prefix-aware.
	if path := parsed.EscapedPath(); path != "" && path != "/" {
		return "", errors.New("must not include a path; serving the daemon under a path prefix is not supported")
	}

	// Lowercase the host but not an IPv6 zone identifier, which is
	// case-sensitive; url.URL.String restores the zone's %25 escaping that
	// url.Parse decoded out of Host.
	host := parsed.Host
	if zone := strings.Index(host, "%"); zone >= 0 {
		host = strings.ToLower(host[:zone]) + host[zone:]
	} else {
		host = strings.ToLower(host)
	}
	// Rewrite the port from its parsed integer and drop scheme defaults, the
	// way browsers serialize Host and Origin: a spelling like :0443 or an
	// explicit :443 on https would otherwise never match those headers.
	if port := parsed.Port(); port != "" {
		host = strings.TrimSuffix(host, ":"+port)
		isDefault := (scheme == "http" && portNumber == 80) || (scheme == "https" && portNumber == 443)
		if !isDefault {
			host = host + ":" + strconv.Itoa(portNumber)
		}
	}

	return (&url.URL{Scheme: scheme, Host: host}).String(), nil
}

// isNumericIPv4Spelling reports whether every dot-separated label of host is a
// decimal or 0x-prefixed hexadecimal number — the shapes browsers parse as an
// IPv4 integer rather than a DNS name.
func isNumericIPv4Spelling(host string) bool {
	if host == "" {
		return false
	}
	// Browsers strip a trailing dot while canonicalizing numeric hosts, but
	// Go's net.ParseIP rejects the dotted spelling, so "127.0.0.1." would
	// otherwise produce an empty final label here, slip past as a non-numeric
	// DNS name, and diverge from the dial target. Trim the terminal dot(s)
	// before applying the numeric-host check so these spellings are rejected
	// as noncanonical by the caller.
	host = strings.TrimRight(host, ".")
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return false
		}
		digits := strings.ToLower(label)
		isHex := strings.HasPrefix(digits, "0x")
		if isHex {
			digits = strings.TrimPrefix(digits, "0x")
		}
		for _, r := range digits {
			if r >= '0' && r <= '9' {
				continue
			}
			if isHex && r >= 'a' && r <= 'f' {
				continue
			}
			return false
		}
	}
	return true
}

// unsupportedProviderBaseURL accepts a concrete GitHub or GitHub Enterprise
// endpoint. Its host is part of a project's provider-bound repository identity.
func unsupportedProviderBaseURL(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if !isAbsoluteHTTPURL(value) {
		return "must be an absolute http(s) URL", false
	}
	_, err := url.Parse(value)
	if err != nil {
		return "must be an absolute http(s) URL", false
	}
	return "", true
}

func isAbsoluteHTTPURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func normalizeNetworkMode(mode NetworkMode) NetworkMode {
	switch strings.TrimSpace(string(mode)) {
	case "", string(NetworkModeOff):
		return NetworkModeOff
	case string(NetworkModeRouted):
		return NetworkModeRouted
	default:
		return mode
	}
}

func validateNetworkNodeName(nodeName string) error {
	trimmed := strings.TrimSpace(nodeName)
	if trimmed == "" {
		return fmt.Errorf("must be a non-empty string")
	}
	if trimmed != nodeName {
		return fmt.Errorf("must not contain leading or trailing whitespace")
	}
	if strings.Contains(trimmed, ":") {
		return fmt.Errorf("must not contain ':' so it can form looper:target:<node_name>")
	}
	if len(trimmed) > 32 {
		return fmt.Errorf("must be 32 characters or fewer so it can form looper:target:<node_name>")
	}
	if !networkNodeNamePattern.MatchString(trimmed) {
		return fmt.Errorf("must contain only letters, numbers, '.', '_' or '-' so it can form looper:target:<node_name>")
	}
	return nil
}

func validateWebhookTunnelConfig(config WebhookConfig, path string, issues *[]ValidationIssue) {
	if config.ListenPort < 1024 || config.ListenPort > 65535 {
		*issues = append(*issues, ValidationIssue{Path: path + ".listenPort", Message: "must be an integer between 1024 and 65535 when webhook mode is tunnel"})
	}
	parsed, err := url.Parse(config.PublicBaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".publicBaseUrl", Message: "must be a valid https URL with a host when webhook mode is tunnel"})
	}
}

// validateResourceGuardConfig rejects thresholds that would refuse all work.
// A percentage at or above 100 admits nothing, and a negative threshold is
// meaningless; both would halt the scheduler with no obvious cause. Non-finite
// values (NaN, Inf) are rejected explicitly: every range check below is a
// strict comparison that is false for NaN, so validation would otherwise accept
// it, the guard would silently skip the check (NaN > 0 is false), and JSON
// projections such as /config cannot encode the value.
func validateResourceGuardConfig(config ResourceGuardConfig, path string, issues *[]ValidationIssue) {
	if math.IsNaN(config.MinDiskFreePercent) || math.IsInf(config.MinDiskFreePercent, 0) {
		*issues = append(*issues, ValidationIssue{Path: path + ".minDiskFreePercent", Message: "must be a finite number in [0, 100)"})
	} else if config.MinDiskFreePercent < 0 || config.MinDiskFreePercent >= 100 {
		*issues = append(*issues, ValidationIssue{Path: path + ".minDiskFreePercent", Message: "must be a number in [0, 100)"})
	}
	if math.IsNaN(config.MinDiskFreeGB) || math.IsInf(config.MinDiskFreeGB, 0) {
		*issues = append(*issues, ValidationIssue{Path: path + ".minDiskFreeGb", Message: "must be a finite number >= 0"})
	} else if config.MinDiskFreeGB < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".minDiskFreeGb", Message: "must be a number >= 0"})
	}
	if math.IsNaN(config.MaxLoadPerCPU) || math.IsInf(config.MaxLoadPerCPU, 0) {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxLoadPerCpu", Message: "must be a finite number >= 0"})
	} else if config.MaxLoadPerCPU < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxLoadPerCpu", Message: "must be a number >= 0"})
	}
}

func validateWorktreeCleanupConfig(config WorktreeCleanupConfig, path string, issues *[]ValidationIssue) {
	if strings.TrimSpace(config.Interval) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".interval", Message: "must be a non-empty duration string"})
	} else if duration, err := time.ParseDuration(config.Interval); err != nil || duration <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".interval", Message: "must be a positive duration"})
	}
	if config.RetentionDays < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".retentionDays", Message: "must be an integer >= 0"})
	}
	if config.MaxPerTick < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxPerTick", Message: "must be a positive integer"})
	}
}

func webhookModeRequiresTunnelConfig(config Config, project *ProjectRefConfig) bool {
	mode := config.Webhook.Mode
	if project != nil && project.Webhook.Mode != "" {
		mode = project.Webhook.Mode
	}
	return mode == WebhookModeTunnel
}

func validateAgentTimeouts(timeouts AgentTimeoutConfig, path string, issues *[]ValidationIssue) {
	validateAgentTimeoutSeconds(timeouts.PlannerSeconds, path+".plannerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.WorkerSeconds, path+".workerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.ReviewerSeconds, path+".reviewerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.FixerSeconds, path+".fixerSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.PlannerIdleTimeoutSeconds, path+".plannerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.PlannerMaxRuntimeSeconds, path+".plannerMaxRuntimeSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.WorkerIdleTimeoutSeconds, path+".workerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.WorkerMaxRuntimeSeconds, path+".workerMaxRuntimeSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.ReviewerIdleTimeoutSeconds, path+".reviewerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.ReviewerMaxRuntimeSeconds, path+".reviewerMaxRuntimeSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.FixerIdleTimeoutSeconds, path+".fixerIdleTimeoutSeconds", issues)
	validateAgentTimeoutSeconds(timeouts.FixerMaxRuntimeSeconds, path+".fixerMaxRuntimeSeconds", issues)
}

func validateEnvironmentNames(values map[string]string, path string, issues *[]ValidationIssue) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !environmentNamePattern.MatchString(key) {
			*issues = append(*issues, ValidationIssue{Path: path + "." + key, Message: "must be a valid environment-variable name"})
		}
	}
}

func validateAgentTimeoutSeconds(seconds int, path string, issues *[]ValidationIssue) {
	if seconds < 1 {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "must be a positive integer"})
		return
	}

	const maxDuration = time.Duration(1<<63 - 1)
	maxSeconds := int64(maxDuration / time.Second)
	if int64(seconds) > maxSeconds {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "must fit within time.Duration when converted from seconds"})
	}
}

func validateProjectRoleOverrides(roles *PartialRoleConfigs, prefix string, maxInstructionBytes int, issues *[]ValidationIssue) {
	if roles == nil {
		return
	}
	validateProjectCodingRoleOverrides(roles, prefix, issues)
	if roles.Planner != nil {
		validateProjectRoleInstruction(prefix+".planner.instructions", "planner", roles.Planner.Instructions, maxInstructionBytes, issues)
		if roles.Planner.Triggers != nil {
			validateIssueRoleTriggers(partialIssueRoleTriggers(*roles.Planner.Triggers), prefix+".planner.triggers", issues)
		}
		if roles.Planner.Escalation != nil {
			candidate := PlannerEscalationConfig{}
			mergePlannerEscalationConfig(&candidate, *roles.Planner.Escalation)
			validatePlannerEscalation(&candidate, prefix+".planner.escalation", issues)
		}
	}
	if roles.Worker != nil {
		validateProjectRoleInstruction(prefix+".worker.instructions", "worker", roles.Worker.Instructions, maxInstructionBytes, issues)
		if roles.Worker.Triggers != nil {
			validateIssueRoleTriggers(partialIssueRoleTriggers(*roles.Worker.Triggers), prefix+".worker.triggers", issues)
		}
	}
	if roles.Reviewer != nil {
		validateProjectRoleInstruction(prefix+".reviewer.instructions", "reviewer", roles.Reviewer.Instructions, maxInstructionBytes, issues)
		if roles.Reviewer.Behavior != nil && roles.Reviewer.Behavior.Convergence != nil {
			candidate := DefaultReviewerConvergenceConfig()
			mergeReviewerConvergenceConfig(&candidate, *roles.Reviewer.Behavior.Convergence)
			validateReviewerConvergence(&candidate, prefix+".reviewer.behavior.convergence", issues)
		}
		if roles.Reviewer.Discovery != nil {
			if roles.Reviewer.Discovery.Triggers != nil {
				validateReviewerRoleTriggers(partialReviewerRoleTriggers(*roles.Reviewer.Discovery.Triggers), prefix+".reviewer.discovery.triggers", issues)
			}
			if roles.Reviewer.Discovery.SpecReview != nil && roles.Reviewer.Discovery.SpecReview.ReviewingLabel != nil {
				label := *roles.Reviewer.Discovery.SpecReview.ReviewingLabel
				if label != "" && label != strings.TrimSpace(label) {
					*issues = append(*issues, ValidationIssue{Path: prefix + ".reviewer.discovery.specReview.reviewingLabel", Message: "must not contain leading or trailing whitespace"})
				}
			}
		}
		if roles.Reviewer.Triggers != nil {
			validateReviewerRoleTriggers(partialReviewerRoleTriggers(*roles.Reviewer.Triggers), prefix+".reviewer.triggers", issues)
		}
		if roles.Reviewer.SpecReview != nil && roles.Reviewer.SpecReview.ReviewingLabel != nil {
			label := *roles.Reviewer.SpecReview.ReviewingLabel
			if label != "" && label != strings.TrimSpace(label) {
				*issues = append(*issues, ValidationIssue{Path: prefix + ".reviewer.specReview.reviewingLabel", Message: "must not contain leading or trailing whitespace"})
			}
		}
		if roles.Reviewer.AutoMerge != nil {
			validatePartialReviewerAutoMerge(*roles.Reviewer.AutoMerge, prefix+".reviewer.autoMerge", issues)
		}
	}
	if roles.Fixer != nil {
		validateProjectRoleInstruction(prefix+".fixer.instructions", "fixer", roles.Fixer.Instructions, maxInstructionBytes, issues)
		if roles.Fixer.Triggers != nil {
			validateFixerRoleTriggers(partialFixerRoleTriggers(*roles.Fixer.Triggers), prefix+".fixer.triggers", issues)
		}
	}
	if roles.Coordinator != nil {
		if roles.Coordinator.PollInterval != nil && strings.TrimSpace(*roles.Coordinator.PollInterval) == "" {
			*issues = append(*issues, ValidationIssue{Path: prefix + ".coordinator.pollInterval", Message: "must be a non-empty duration string"})
		}
		if roles.Coordinator.MarkReady != nil {
			validatePartialCoordinatorMarkReady(*roles.Coordinator.MarkReady, prefix+".coordinator.markReady", issues)
		}
		if roles.Coordinator.PostMergeDigest != nil {
			*issues = append(*issues, ValidationIssue{Path: prefix + ".coordinator.postMergeDigest", Message: "post-merge digest is global-only; configure it under roles.coordinator.postMergeDigest"})
		}
	}
}

func validatePlannerEscalation(escalation *PlannerEscalationConfig, path string, issues *[]ValidationIssue) {
	if escalation == nil {
		return
	}
	if escalation.MaxEstimatedFiles < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxEstimatedFiles", Message: "must be an integer >= 0"})
	}
	if escalation.MaxEstimatedPackages < 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxEstimatedPackages", Message: "must be an integer >= 0"})
	}
}

func validateReviewerConvergence(convergence *ReviewerConvergenceConfig, path string, issues *[]ValidationIssue) {
	if convergence == nil {
		return
	}
	if convergence.MaxConsecutiveUnproductive < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxConsecutiveUnproductive", Message: "must be a positive integer"})
	}
	if convergence.MaxFixerAttemptsPerItem < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxFixerAttemptsPerItem", Message: "must be a positive integer"})
	}
	if convergence.MaxTotalRounds < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".maxTotalRounds", Message: "must be a positive integer"})
	}
	switch convergence.SeverityFloor {
	case ReviewerSeverityFloorBlocking, ReviewerSeverityFloorNonBlocking, ReviewerSeverityFloorAll:
	default:
		*issues = append(*issues, ValidationIssue{Path: path + ".severityFloor", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerSeverityFloorBlocking, ReviewerSeverityFloorNonBlocking, ReviewerSeverityFloorAll)})
	}
}

func validateProjectRoleInstruction(path, role string, text *string, maxBytes int, issues *[]ValidationIssue) {
	if text == nil {
		return
	}
	validateInstructionText(path, role, *text, maxBytes, issues)
}

func partialIssueRoleTriggers(partial PartialIssueRoleTriggersConfig) IssueRoleTriggersConfig {
	config := IssueRoleTriggersConfig{LabelMode: LabelModeAll}
	mergeIssueRoleTriggersConfig(&config, partial)
	return config
}

func partialReviewerRoleTriggers(partial PartialReviewerRoleTriggersConfig) ReviewerRoleTriggersConfig {
	config := ReviewerRoleTriggersConfig{LabelMode: LabelModeAll}
	mergeReviewerRoleTriggersConfig(&config, partial)
	return config
}

func partialFixerRoleTriggers(partial PartialFixerRoleTriggersConfig) FixerRoleTriggersConfig {
	config := FixerRoleTriggersConfig{AuthorFilter: FixerAuthorFilterCurrentUser, LabelMode: LabelModeAll}
	mergeFixerRoleTriggersConfig(&config, partial)
	return config
}

func validateInstructions(config Config, issues *[]ValidationIssue) {
	if config.Instructions.MaxBytes < 1 {
		*issues = append(*issues, ValidationIssue{Path: "instructions.maxBytes", Message: "must be a positive integer"})
	}
	for _, roleInstruction := range roleInstructions(config.Roles) {
		path := "roles.coding." + roleInstruction.role + ".instructions"
		validateInstructionText(path, roleInstruction.role, roleInstruction.text, config.Instructions.MaxBytes, issues)
		validateAggregateInstructionBytes(path, roleInstruction.text, "", config.Instructions.MaxBytes, issues)
	}
}

type roleInstruction struct {
	role string
	text string
}

func roleInstructions(roles RoleConfigs) []roleInstruction {
	registry := EffectiveCodingRoles(roles)
	return []roleInstruction{
		{role: CodingRolePlanner, text: registry[CodingRolePlanner].Instructions},
		{role: CodingRoleWorker, text: registry[CodingRoleWorker].Instructions},
		{role: CodingRoleReviewer, text: registry[CodingRoleReviewer].Instructions},
		{role: CodingRoleFixer, text: registry[CodingRoleFixer].Instructions},
	}
}

func roleInstructionText(roles RoleConfigs, role string) string {
	entry, ok := EffectiveCodingRoles(roles)[role]
	if !ok || !isCodingRole(role) {
		return ""
	}
	return entry.Instructions
}

func validateInstructionText(path, role, text string, maxBytes int, issues *[]ValidationIssue) {
	if !isValidInstructionRole(role) {
		*issues = append(*issues, ValidationIssue{Path: path, Message: "role must be one of: planner, worker, reviewer, fixer"})
	}
	if maxBytes > 0 && len([]byte(text)) > maxBytes {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("must be at most %d bytes", maxBytes)})
	}
	if protected := protectedInstructionPhrase(text); protected != "" {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("must not attempt to override protected Looper contract %q", protected)})
	}
}

func validateCustomCodingRoleInstruction(path, text string, maxBytes int, issues *[]ValidationIssue) {
	if maxBytes > 0 && len([]byte(text)) > maxBytes {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("must be at most %d bytes", maxBytes)})
	}
	if protected := protectedInstructionPhrase(text); protected != "" {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("must not attempt to override protected Looper contract %q", protected)})
	}
}

func validateAggregateInstructionBytes(path, globalText, projectText string, maxBytes int, issues *[]ValidationIssue) {
	if maxBytes <= 0 {
		return
	}
	bytes := len([]byte(strings.TrimSpace(globalText))) + len([]byte(strings.TrimSpace(projectText)))
	if bytes > maxBytes {
		*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("combined custom instructions for this role must be at most %d bytes", maxBytes)})
	}
}

func isValidInstructionRole(role string) bool {
	switch role {
	case "planner", "worker", "reviewer", "fixer":
		return true
	default:
		return false
	}
}

func protectedInstructionPhrase(text string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(text), " "))
	protected := []string{"systemprompt", "system prompt", "__looper_result__", "completion marker", "git_pr_lifecycle", "summary field", "commits field", "result json", "allowautopush", "allowautoapprove", "allow auto push", "allow auto approve", "auto approve", "auto push", "pr creation policy", "review submission policy", "looper review submit", "review submit capability", "gh pr review", "disclosure stamping", "auth requirement", "permission boundary", "state transition", "state machine", "ignore lifecycle", "override lifecycle", "custom completion"}
	for _, phrase := range protected {
		if strings.Contains(normalized, phrase) {
			return phrase
		}
	}
	return ""
}

type writablePathKind string

const (
	writablePathDirectory  writablePathKind = "directory"
	writablePathFileParent writablePathKind = "file-parent"
	writePermissionMode                     = 0x2
)

func ensureWritablePath(path string, kind writablePathKind, issues *[]ValidationIssue, field string) {
	target := path
	if kind == writablePathFileParent {
		target = filepath.Dir(path)
	}

	writableAnchor := target
	for {
		info, err := os.Stat(writableAnchor)
		if err == nil {
			if !info.IsDir() {
				*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s is not a directory", writableAnchor)})
			}
			break
		}

		if errors.Is(err, syscall.ENOTDIR) {
			parent := filepath.Dir(writableAnchor)
			if parent == writableAnchor {
				*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s cannot be created because no existing parent was found", target)})
				return
			}

			writableAnchor = parent
			continue
		}

		if !os.IsNotExist(err) {
			*issues = append(*issues, ValidationIssue{Path: field, Message: err.Error()})
			return
		}

		parent := filepath.Dir(writableAnchor)
		if parent == writableAnchor {
			*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s cannot be created because no existing parent was found", target)})
			return
		}

		writableAnchor = parent
	}

	if hasIssueForField(*issues, field) {
		return
	}

	if err := syscall.Access(writableAnchor, writePermissionMode); err != nil {
		*issues = append(*issues, ValidationIssue{Path: field, Message: fmt.Sprintf("%s is not writable", writableAnchor)})
	}
}

func hasIssueForField(issues []ValidationIssue, field string) bool {
	for _, issue := range issues {
		if issue.Path == field {
			return true
		}
	}

	return false
}

func getConfigProjectIDValidationMessage() string {
	return "must not contain path separators, dot segments, or be an absolute path"
}

func isNilOrEmptyString(value *string) bool {
	return value == nil || *value == ""
}

func isValidAgentVendor(vendor AgentVendor) bool {
	for _, supported := range supportedAgentVendors {
		if vendor == supported {
			return true
		}
	}
	return false
}

func agentVendorValidationMessage() string {
	values := make([]string, 0, len(supportedAgentVendors))
	for _, vendor := range supportedAgentVendors {
		values = append(values, string(vendor))
	}
	return "must be one of: " + strings.Join(values, ", ")
}

func validateAgentProfiles(profiles map[string]AgentBindingConfig, issues *[]ValidationIssue) {
	if len(profiles) == 0 {
		return
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		path := "agent.profiles." + id
		if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) || !agentProfileIDPattern.MatchString(id) {
			*issues = append(*issues, ValidationIssue{Path: path, Message: "profile id must be non-empty, trimmed, and match [A-Za-z0-9_-]+"})
			continue
		}
		binding := profiles[id]
		if binding.Vendor == nil && binding.Model == nil {
			*issues = append(*issues, ValidationIssue{Path: path, Message: "must set at least one of vendor or model"})
		}
		if binding.Vendor != nil && !isValidAgentVendor(*binding.Vendor) {
			*issues = append(*issues, ValidationIssue{Path: path + ".vendor", Message: agentVendorValidationMessage()})
		}
	}
}

// validateCodingRoleRegistry protects callers that assemble Config directly
// instead of passing source text through Normalize. The compiled runner is the
// authority for accepted registry names and sources; accepting an arbitrary
// map entry would create a lane with no executable consumer.
func validateCodingRoleRegistry(config Config, issues *[]ValidationIssue) {
	registry := EffectiveCodingRoles(config.Roles)
	for _, name := range []string{CodingRolePlanner, CodingRoleWorker, CodingRoleReviewer, CodingRoleFixer, RoleGatekeeper} {
		if _, ok := registry[name]; !ok {
			*issues = append(*issues, ValidationIssue{Path: "roles.coding." + name, Message: "is required in the canonical registry"})
		}
	}
	for _, name := range CodingRoleNames(config.Roles) {
		role := registry[name]
		path := "roles.coding." + name
		if NormalizeRoleName(name) == "" || NormalizeRoleName(name) != name {
			*issues = append(*issues, ValidationIssue{Path: path, Message: "role name must be non-empty, trimmed, and lowercase"})
		}
		if name == RoleGatekeeper {
			if !reflect.DeepEqual(role, compiledGatekeeperRole()) {
				*issues = append(*issues, ValidationIssue{Path: path, Message: "is a compiled-in policy role and cannot be overridden"})
			}
			continue
		}
		expectedSource, runnerBacked := CodingRoleSource(name)
		if !runnerBacked {
			*issues = append(*issues, ValidationIssue{Path: path, Message: "has no compiled runner; roles.coding supports only planner, worker, reviewer, and fixer"})
			continue
		}
		if role.Priority <= 0 {
			*issues = append(*issues, ValidationIssue{Path: path + ".priority", Message: "must be a positive integer"})
		}
		if role.Discovery.Source != expectedSource {
			*issues = append(*issues, ValidationIssue{Path: path + ".discovery.source", Message: "must be " + strconv.Quote(string(expectedSource)) + " for this compiled runner"})
		}
		*issues = append(*issues, ValidateRoleDiscovery(path, role.Discovery)...)
		*issues = append(*issues, validateCodingRoleDiscoveryCommon(path, role.Discovery)...)
		validateRoleAgentBinding(config, path+".agent", role.Agent, issues)
	}
}

func validateRoleAgentBinding(config Config, prefix string, agent *RoleAgentConfig, issues *[]ValidationIssue) {
	if agent == nil {
		return
	}
	if agent.Profile != nil {
		profileID := *agent.Profile
		trimmed := strings.TrimSpace(profileID)
		if trimmed == "" || profileID != trimmed {
			*issues = append(*issues, ValidationIssue{Path: prefix + ".profile", Message: "must be a non-empty trimmed profile id"})
		} else if _, exists := config.Agent.Profiles[trimmed]; !exists {
			*issues = append(*issues, ValidationIssue{Path: prefix + ".profile", Message: fmt.Sprintf("references unknown agent profile: %s", trimmed)})
		}
	}
	if agent.Vendor != nil && !isValidAgentVendor(*agent.Vendor) {
		*issues = append(*issues, ValidationIssue{Path: prefix + ".vendor", Message: agentVendorValidationMessage()})
	}
}

func validateProjectRoleAgentBindings(roles *PartialRoleConfigs, prefix string, issues *[]ValidationIssue) {
	if roles == nil {
		return
	}
	type roleBinding struct {
		role  string
		agent *RoleAgentConfig
	}
	var bindings []roleBinding
	if roles.Planner != nil {
		bindings = append(bindings, roleBinding{role: CodingRolePlanner, agent: roles.Planner.Agent})
	}
	if roles.Worker != nil {
		bindings = append(bindings, roleBinding{role: CodingRoleWorker, agent: roles.Worker.Agent})
	}
	if roles.Reviewer != nil {
		bindings = append(bindings, roleBinding{role: CodingRoleReviewer, agent: roles.Reviewer.Agent})
	}
	if roles.Fixer != nil {
		bindings = append(bindings, roleBinding{role: CodingRoleFixer, agent: roles.Fixer.Agent})
	}
	for _, binding := range bindings {
		if !roleAgentBindingSet(binding.agent) {
			continue
		}
		*issues = append(*issues, ValidationIssue{
			Path:    prefix + "." + binding.role + ".agent",
			Message: "project-level agent bindings are not supported",
		})
	}
}

func roleAgentBindingSet(agent *RoleAgentConfig) bool {
	if agent == nil {
		return false
	}
	return agent.Profile != nil || agent.Vendor != nil || agent.Model != nil
}

func isValidAuthMode(mode AuthMode) bool {
	switch mode {
	case AuthModeNone, AuthModeLocalToken:
		return true
	default:
		return false
	}
}

func isValidDaemonMode(mode DaemonMode) bool {
	switch mode {
	case DaemonModeForeground, DaemonModeLaunchd, DaemonModeSystemd:
		return true
	default:
		return false
	}
}

func isValidDaemonRestartPolicy(policy DaemonRestartPolicy) bool {
	switch policy {
	case DaemonRestartNever, DaemonRestartOnFailure, DaemonRestartAlways:
		return true
	default:
		return false
	}
}

func isValidLogLevel(level LogLevel) bool {
	switch level {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		return true
	default:
		return false
	}
}

func isValidNotificationSoundLevel(level NotificationSoundLevel) bool {
	switch level {
	case NotificationSoundLevelActionRequired, NotificationSoundLevelFailure:
		return true
	default:
		return false
	}
}

func isValidOpenPRStrategy(strategy OpenPRStrategy) bool {
	switch strategy {
	case OpenPRStrategyAllDone, OpenPRStrategyFirstCommit, OpenPRStrategyManual:
		return true
	default:
		return false
	}
}

func isValidAddSnapshotMode(mode AddSnapshotMode) bool {
	switch mode {
	case AddSnapshotModeAsync, AddSnapshotModeFull, AddSnapshotModeOff:
		return true
	default:
		return false
	}
}

func isValidWebhookMode(mode WebhookMode) bool {
	switch mode {
	case WebhookModeGHForward, WebhookModeTunnel:
		return true
	default:
		return false
	}
}

func isValidWebhookModeOrEmpty(mode WebhookMode) bool {
	return mode == "" || isValidWebhookMode(mode)
}

func isValidLabelMode(mode LabelMode) bool {
	switch mode {
	case LabelModeAll, LabelModeAny:
		return true
	default:
		return false
	}
}

func isValidFixerAuthorFilter(filter FixerAuthorFilter) bool {
	switch filter {
	case FixerAuthorFilterCurrentUser, FixerAuthorFilterAny:
		return true
	default:
		return false
	}
}

func isValidReviewerAutoMergeStrategy(strategy ReviewerAutoMergeStrategy) bool {
	switch strategy {
	case ReviewerAutoMergeStrategySquash, ReviewerAutoMergeStrategyMerge, ReviewerAutoMergeStrategyRebase:
		return true
	default:
		return false
	}
}

func validatePartialReviewerAutoMerge(partial PartialReviewerAutoMergeConfig, path string, issues *[]ValidationIssue) {
	if partial.Strategy != nil && !isValidReviewerAutoMergeStrategy(*partial.Strategy) {
		*issues = append(*issues, ValidationIssue{Path: path + ".strategy", Message: fmt.Sprintf("must be one of: %s, %s, %s", ReviewerAutoMergeStrategySquash, ReviewerAutoMergeStrategyMerge, ReviewerAutoMergeStrategyRebase)})
	}
	if partial.TransientRetries != nil && *partial.TransientRetries < 1 {
		*issues = append(*issues, ValidationIssue{Path: path + ".transientRetries", Message: "must be a positive integer"})
	}
	if partial.Scope != nil && *partial.Scope != ReviewerAutoMergeScopeLooperOnly {
		*issues = append(*issues, ValidationIssue{Path: path + ".scope", Message: fmt.Sprintf("must be %s", ReviewerAutoMergeScopeLooperOnly)})
	}
}

func validatePartialCoordinatorMarkReady(partial PartialCoordinatorMarkReadyConfig, path string, issues *[]ValidationIssue) {
	if partial.Scope != nil && *partial.Scope != CoordinatorMarkReadyScopeLooperOnly {
		*issues = append(*issues, ValidationIssue{Path: path + ".scope", Message: fmt.Sprintf("must be %s", CoordinatorMarkReadyScopeLooperOnly)})
	}
}

func validateIssueRoleTriggers(triggers IssueRoleTriggersConfig, path string, issues *[]ValidationIssue) {
	validateLabelTriggers(triggers.Labels, triggers.LabelMode, path, issues)
}

func validateReviewerRoleTriggers(triggers ReviewerRoleTriggersConfig, path string, issues *[]ValidationIssue) {
	validateLabelTriggers(triggers.Labels, triggers.LabelMode, path, issues)
}

func validateFixerRoleTriggers(triggers FixerRoleTriggersConfig, path string, issues *[]ValidationIssue) {
	validateLabelTriggers(triggers.Labels, triggers.LabelMode, path, issues)
	if !isValidFixerAuthorFilter(triggers.AuthorFilter) {
		*issues = append(*issues, ValidationIssue{Path: path + ".authorFilter", Message: fmt.Sprintf("must be one of: %s, %s", FixerAuthorFilterCurrentUser, FixerAuthorFilterAny)})
	}
}

func validateCoordinatorRoleConfig(config CoordinatorRoleConfig, path string, issues *[]ValidationIssue) {
	if strings.TrimSpace(config.PollInterval) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".pollInterval", Message: "must be a non-empty duration string"})
	} else if duration, err := time.ParseDuration(strings.TrimSpace(config.PollInterval)); err != nil {
		*issues = append(*issues, ValidationIssue{Path: path + ".pollInterval", Message: "must be a valid time.Duration string"})
	} else if duration <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".pollInterval", Message: "must be greater than 0"})
	}
	if config.Triage.MaxIssueAgeDays <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.maxIssueAgeDays", Message: "must be a positive integer"})
	}
	if config.Triage.MaxPerTick <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.maxPerTick", Message: "must be a positive integer"})
	}
	if !isNonEmptyTrimmed(config.Triage.TriagedLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.triagedLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if !isNonEmptyTrimmed(config.Triage.Disposition.OutOfScopeLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.disposition.outOfScopeLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if !isNonEmptyTrimmed(config.Triage.Disposition.UnclearLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".triage.disposition.unclearLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if config.Dispatch.Mode != "human-gated" && config.Dispatch.Mode != "autonomous" {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.mode", Message: "must be one of: human-gated, autonomous"})
	}
	validateStringList(config.Dispatch.HumanGate.SlashCommands, path+".dispatch.humanGate.slashCommands", issues)
	validateStringList(config.Dispatch.HumanGate.AllowedUsers, path+".dispatch.humanGate.allowedUsers", issues)
	if len(config.Dispatch.HumanGate.SlashCommands) == 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.humanGate.slashCommands", Message: "must contain at least one slash command"})
	}
	for _, command := range config.Dispatch.HumanGate.SlashCommands {
		if command != "/plan" && command != "/implement" {
			*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.humanGate.slashCommands", Message: fmt.Sprintf("contains unsupported slash command: %s", command)})
		}
	}
	if config.Dispatch.Autonomous.DelayMinutes <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.autonomous.delayMinutes", Message: "must be a positive integer"})
	}
	if !isNonEmptyTrimmed(config.Dispatch.Autonomous.HoldLabel) {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.autonomous.holdLabel", Message: "must be a non-empty string without leading or trailing whitespace"})
	}
	if config.Dispatch.AssignTo != strings.TrimSpace(config.Dispatch.AssignTo) {
		*issues = append(*issues, ValidationIssue{Path: path + ".dispatch.assignTo", Message: "must not contain leading or trailing whitespace"})
	}
	if config.Dependencies.Enabled {
		if config.Dependencies.APITimeoutSeconds <= 0 {
			*issues = append(*issues, ValidationIssue{Path: path + ".dependencies.apiTimeoutSeconds", Message: "must be a positive integer when dependencies are enabled"})
		}
		if config.Dependencies.APIRetryAttempts <= 0 {
			*issues = append(*issues, ValidationIssue{Path: path + ".dependencies.apiRetryAttempts", Message: "must be a positive integer when dependencies are enabled"})
		}
	}
	if config.MergeWatch.TransientRetries <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.transientRetries", Message: "must be a positive integer"})
	}
	if strings.TrimSpace(config.MergeWatch.MaxIndeterminateDuration) == "" {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.maxIndeterminateDuration", Message: "must be a non-empty duration string"})
	} else if duration, err := time.ParseDuration(strings.TrimSpace(config.MergeWatch.MaxIndeterminateDuration)); err != nil {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.maxIndeterminateDuration", Message: "must be a valid time.Duration string"})
	} else if duration <= 0 {
		*issues = append(*issues, ValidationIssue{Path: path + ".mergeWatch.maxIndeterminateDuration", Message: "must be greater than 0"})
	}
	if config.MarkReady.Scope != CoordinatorMarkReadyScopeLooperOnly {
		*issues = append(*issues, ValidationIssue{Path: path + ".markReady.scope", Message: fmt.Sprintf("must be %s", CoordinatorMarkReadyScopeLooperOnly)})
	}
	if config.PostMergeDigest != nil && config.PostMergeDigest.Enabled {
		if _, err := time.Parse("15:04", config.PostMergeDigest.Schedule); err != nil {
			*issues = append(*issues, ValidationIssue{Path: path + ".postMergeDigest.schedule", Message: "must be a valid time string (HH:MM)"})
		}
		if _, err := time.LoadLocation(config.PostMergeDigest.Timezone); err != nil {
			*issues = append(*issues, ValidationIssue{Path: path + ".postMergeDigest.timezone", Message: "must be a valid IANA timezone string"})
		}
		if config.PostMergeDigest.MaxItems < 1 || config.PostMergeDigest.MaxItems > 200 {
			*issues = append(*issues, ValidationIssue{Path: path + ".postMergeDigest.maxItems", Message: "must be an integer between 1 and 200"})
		}
	}
	validateDistinctLabels([]labelPathValue{
		{Path: path + ".triage.triagedLabel", Value: config.Triage.TriagedLabel},
		{Path: path + ".triage.disposition.outOfScopeLabel", Value: config.Triage.Disposition.OutOfScopeLabel},
		{Path: path + ".triage.disposition.unclearLabel", Value: config.Triage.Disposition.UnclearLabel},
		{Path: path + ".dispatch.autonomous.holdLabel", Value: config.Dispatch.Autonomous.HoldLabel},
	}, issues)
}

func isNonEmptyTrimmed(value string) bool {
	return strings.TrimSpace(value) != "" && value == strings.TrimSpace(value)
}

type labelPathValue struct {
	Path  string
	Value string
}

func validateDistinctLabels(labels []labelPathValue, issues *[]ValidationIssue) {
	seen := map[string]string{}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label.Value)
		if trimmed == "" {
			continue
		}
		if firstPath, ok := seen[trimmed]; ok {
			*issues = append(*issues, ValidationIssue{Path: label.Path, Message: fmt.Sprintf("duplicates %s", firstPath)})
			continue
		}
		seen[trimmed] = label.Path
	}
}

func validateStringList(values []string, path string, issues *[]ValidationIssue) {
	seen := map[string]struct{}{}
	for index, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s[%d]", path, index), Message: "must be a non-empty string"})
			continue
		}
		if value != trimmed {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s[%d]", path, index), Message: "must not contain leading or trailing whitespace"})
			continue
		}
		if _, ok := seen[value]; ok {
			*issues = append(*issues, ValidationIssue{Path: path, Message: fmt.Sprintf("contains duplicate value: %s", value)})
			continue
		}
		seen[value] = struct{}{}
	}
}

func validateLabelTriggers(labels []string, mode LabelMode, path string, issues *[]ValidationIssue) {
	if !isValidLabelMode(mode) {
		*issues = append(*issues, ValidationIssue{Path: path + ".labelMode", Message: fmt.Sprintf("must be one of: %s, %s", LabelModeAll, LabelModeAny)})
	}
	seen := map[string]struct{}{}
	for index, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s.labels[%d]", path, index), Message: "must be a non-empty string"})
			continue
		}
		if label != trimmed {
			*issues = append(*issues, ValidationIssue{Path: fmt.Sprintf("%s.labels[%d]", path, index), Message: "must not contain leading or trailing whitespace"})
			continue
		}
		if _, ok := seen[label]; ok {
			*issues = append(*issues, ValidationIssue{Path: path + ".labels", Message: fmt.Sprintf("contains duplicate label: %s", label)})
			continue
		}
		seen[label] = struct{}{}
	}
}

func isValidReviewerScope(scope ReviewerScope) bool {
	switch scope {
	case ReviewerScopeFullPR, ReviewerScopeChangedFiles, ReviewerScopeChangedRanges:
		return true
	default:
		return false
	}
}

func isValidReviewerThreadResolutionMode(mode ReviewerThreadResolutionMode) bool {
	switch mode {
	case ReviewerThreadResolutionModeReportOnly, ReviewerThreadResolutionModeCommentOnly, ReviewerThreadResolutionModeSuggestResolution, ReviewerThreadResolutionModeResolveObjective:
		return true
	default:
		return false
	}
}

func isValidConfiguredProjectID(projectID string) bool {
	return projectID != "" && projectID != "." && projectID != ".." && !containsProjectPathSeparator(projectID) && !isAbsoluteProjectPath(projectID)
}

func containsProjectPathSeparator(projectID string) bool {
	for _, char := range projectID {
		if char == '/' || char == '\\' {
			return true
		}
	}

	return false
}

func isAbsoluteProjectPath(projectID string) bool {
	if len(projectID) >= 1 && projectID[0] == '/' {
		return true
	}

	if len(projectID) >= 3 {
		drive := projectID[0]
		separator := projectID[2]
		if ((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) && projectID[1] == ':' && (separator == '/' || separator == '\\') {
			return true
		}
	}

	if len(projectID) >= 2 && projectID[0] == '\\' && projectID[1] == '\\' {
		return true
	}

	return false
}
