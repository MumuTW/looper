package api

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/MumuTW/looper/internal/config"
	pkgapi "github.com/MumuTW/looper/pkg/api"
)

func (h *Handler) handleConfigRoute(w http.ResponseWriter, r *http.Request, requestID string) {
	switch r.Method {
	case http.MethodGet:
		h.writeSuccess(w, requestID, h.buildConfigResponse())
	case http.MethodPatch:
		patch, err := decodeConfigPatchRequest(w, r)
		if err != nil {
			h.writeError(w, requestID, configRequestAPIError(err))
			return
		}
		if h.context.PatchConfig == nil {
			h.writeError(w, requestID, configRequestAPIError(ConfigRequestError{
				Kind:    ConfigRequestErrorKindUnsupported,
				Message: "Dynamic configuration updates are unavailable",
				Issues: []ConfigPatchIssue{{
					Code:    "config_patch_unsupported",
					Message: "This daemon does not support field-level configuration updates",
				}},
			}))
			return
		}
		if err := h.context.PatchConfig(r.Context(), patch); err != nil {
			h.writeError(w, requestID, configRequestAPIError(err))
			return
		}

		// A mutation establishes a new snapshot boundary. Refresh once after the
		// callback so the PATCH response projects the configuration just applied.
		if h.context.ConfigSnapshot != nil {
			cfg, metadata := h.context.ConfigSnapshot()
			h.context.Config = cfg
			h.context.ConfigMetadata = func() ConfigMetadata { return metadata }
		} else if runtimeConfig, ok := any(h.context.Runtime).(interface{ Config() config.Config }); ok {
			h.context.Config = runtimeConfig.Config()
		}
		h.writeSuccess(w, requestID, h.buildConfigResponse())
	default:
		h.writeError(w, requestID, apiError{
			code:    pkgapi.ErrorCodeMethodNotAllowed,
			status:  http.StatusMethodNotAllowed,
			message: fmt.Sprintf("Unsupported method for %s", apiBasePath+"/config"),
		})
	}
}

type configResponse struct {
	Server        configServerResponse      `json:"server"`
	Storage       config.StorageConfig      `json:"storage"`
	Scheduler     config.SchedulerConfig    `json:"scheduler"`
	Webhook       config.WebhookConfig      `json:"webhook"`
	Agent         configAgentResponse       `json:"agent"`
	Logging       config.LoggingConfig      `json:"logging"`
	Notifications config.NotificationConfig `json:"notifications"`
	Disclosure    config.DisclosureConfig   `json:"disclosure"`
	Tools         config.ToolPathsConfig    `json:"tools"`
	Daemon        configDaemonResponse      `json:"daemon"`
	Package       configPackageResponse     `json:"package"`
	Defaults      config.DefaultsConfig     `json:"defaults"`
	Instructions  config.InstructionsConfig `json:"instructions"`
	HITL          config.HITLConfig         `json:"hitl"`
	Roles         configRolesResponse       `json:"roles"`
	Providers     []config.ProviderConfig   `json:"providers"`
	Projects      []config.ProjectRefConfig `json:"projects"`
	Metadata      ConfigMetadata            `json:"metadata"`
}

// redactProjectSecrets copies projects with their deploy credentials removed.
//
// projects[].roles.deployer.environment holds the values a deploy authenticates
// with — the same class of secret as daemon.environment, which this response
// already withholds. The copy is deep through Roles because the slice copy shares
// that pointer with the live configuration.
func redactProjectSecrets(projects []config.ProjectRefConfig) []config.ProjectRefConfig {
	redacted := append([]config.ProjectRefConfig{}, projects...)
	for i := range redacted {
		roles := redacted[i].Roles
		if roles == nil || roles.Deployer == nil || roles.Deployer.Environment == nil {
			continue
		}
		deployer := *roles.Deployer
		deployer.Environment = nil
		clonedRoles := *roles
		clonedRoles.Deployer = &deployer
		redacted[i].Roles = &clonedRoles
	}
	return redacted
}

type configRolesResponse struct {
	Coding      map[string]config.CodingRoleConfig `json:"coding"`
	Planner     config.PlannerRoleConfig           `json:"planner"`
	Reviewer    config.ReviewerRoleConfig          `json:"reviewer"`
	Fixer       config.FixerRoleConfig             `json:"fixer"`
	Worker      config.WorkerRoleConfig            `json:"worker"`
	Coordinator config.CoordinatorRoleConfig       `json:"coordinator"`
}

type configServerResponse struct {
	Host     string          `json:"host"`
	Port     int             `json:"port"`
	BaseURL  *string         `json:"baseUrl,omitempty"`
	AuthMode config.AuthMode `json:"authMode"`
}

type configAgentResponse struct {
	Vendor          *config.AgentVendor                  `json:"vendor,omitempty"`
	Model           *string                              `json:"model,omitempty"`
	ReasoningEffort *config.ReasoningEffort              `json:"reasoningEffort,omitempty"`
	Profiles        map[string]config.AgentBindingConfig `json:"profiles,omitempty"`
	Params          map[string]any                       `json:"params"`
	Env             map[string]string                    `json:"env"`
	EnvKeys         []string                             `json:"envKeys"`
	Timeouts        config.AgentTimeoutConfig            `json:"timeouts"`
	NativeResume    config.AgentNativeResumeConfig       `json:"nativeResume"`
}

type configDaemonResponse struct {
	Mode                   config.DaemonMode            `json:"mode"`
	RestartPolicy          config.DaemonRestartPolicy   `json:"restartPolicy"`
	RestartThrottleSeconds int                          `json:"restartThrottleSeconds"`
	PlistPath              *string                      `json:"plistPath,omitempty"`
	LogDir                 string                       `json:"logDir"`
	WorkingDirectory       string                       `json:"workingDirectory"`
	Environment            map[string]string            `json:"environment"`
	WorktreeCleanup        config.WorktreeCleanupConfig `json:"worktreeCleanup"`
}

type configPackageResponse struct {
	Distribution string `json:"distribution"`
	// AutoUpgradeEnabled preserves the frozen response shape; runtime no longer
	// reads or acts on this value.
	AutoUpgradeEnabled         bool `json:"autoUpgradeEnabled"`
	AutoMigrateOnStartup       bool `json:"autoMigrateOnStartup"`
	RequireBackupBeforeMigrate bool `json:"requireBackupBeforeMigrate"`
}

func (h *Handler) buildConfigResponse() configResponse {
	cfg := h.context.Config

	return configResponse{
		Server: configServerResponse{
			Host:     cfg.Server.Host,
			Port:     cfg.Server.Port,
			BaseURL:  cfg.Server.BaseURL,
			AuthMode: cfg.Server.AuthMode,
		},
		Storage:   cfg.Storage,
		Scheduler: cfg.Scheduler,
		Webhook:   cfg.Webhook,
		Agent: configAgentResponse{
			Vendor:          cfg.Agent.Vendor,
			Model:           cfg.Agent.Model,
			ReasoningEffort: cfg.Agent.ReasoningEffort,
			Profiles:        cloneAgentProfiles(cfg.Agent.Profiles),
			Params:          map[string]any{},
			Env:             map[string]string{},
			EnvKeys:         sortedMapKeys(cfg.Agent.Env),
			Timeouts:        cfg.Agent.Timeouts,
			NativeResume:    cfg.Agent.NativeResume,
		},
		Logging:       cfg.Logging,
		Notifications: cfg.Notifications,
		Disclosure:    cfg.Disclosure,
		Tools:         cfg.Tools,
		Daemon: configDaemonResponse{
			Mode:                   cfg.Daemon.Mode,
			RestartPolicy:          cfg.Daemon.RestartPolicy,
			RestartThrottleSeconds: cfg.Daemon.RestartThrottleSeconds,
			PlistPath:              cfg.Daemon.PlistPath,
			LogDir:                 cfg.Daemon.LogDir,
			WorkingDirectory:       cfg.Daemon.WorkingDirectory,
			Environment:            map[string]string{},
			WorktreeCleanup:        cfg.Daemon.WorktreeCleanup,
		},
		Package: configPackageResponse{
			Distribution:               cfg.Package.Distribution,
			AutoUpgradeEnabled:         true,
			AutoMigrateOnStartup:       cfg.Package.AutoMigrateOnStartup,
			RequireBackupBeforeMigrate: cfg.Package.RequireBackupBeforeMigrate,
		},
		Defaults:     cfg.Defaults,
		Instructions: cfg.Instructions,
		HITL:         cfg.HITL,
		Roles: configRolesResponse{
			Coding:      config.EffectiveCodingRoles(cfg.Roles),
			Planner:     cfg.Roles.Planner,
			Reviewer:    cfg.Roles.Reviewer,
			Fixer:       cfg.Roles.Fixer,
			Worker:      cfg.Roles.Worker,
			Coordinator: cfg.Roles.Coordinator,
		},
		Providers: append([]config.ProviderConfig{}, cfg.Providers...),
		Projects:  redactProjectSecrets(cfg.Projects),
		Metadata:  h.buildConfigMetadata(),
	}
}

func (h *Handler) buildConfigMetadata() ConfigMetadata {
	metadata := ConfigMetadata{}
	if h.context.ConfigMetadata != nil {
		metadata = h.context.ConfigMetadata()
	}
	metadata.RejectedPaths = append([]string{}, metadata.RejectedPaths...)
	fields := make(map[string]ConfigFieldMetadata, len(metadata.Fields))
	for path, field := range metadata.Fields {
		fields[path] = field
	}
	metadata.Fields = fields
	return metadata
}

func sortedMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// cloneAgentProfiles copies profile bindings for the secret-safe config projection.
// Empty maps become nil so json omitempty matches zero-diff style.
func cloneAgentProfiles(profiles map[string]config.AgentBindingConfig) map[string]config.AgentBindingConfig {
	if len(profiles) == 0 {
		return nil
	}
	cloned := make(map[string]config.AgentBindingConfig, len(profiles))
	for id, binding := range profiles {
		entry := config.AgentBindingConfig{}
		if binding.Vendor != nil {
			vendor := *binding.Vendor
			entry.Vendor = &vendor
		}
		if binding.Model != nil {
			model := *binding.Model
			entry.Model = &model
		}
		if binding.ReasoningEffort != nil {
			effort := *binding.ReasoningEffort
			entry.ReasoningEffort = &effort
		}
		cloned[id] = entry
	}
	return cloned
}
