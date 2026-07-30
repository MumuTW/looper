package config

import "strings"

// ResolveValidationCommands returns the mechanical validation gate the worker
// and fixer run before opening a PR or pushing. The result is a detached copy
// with each command trimmed and blank entries dropped, so callers can hold it
// for the lifetime of a runner without aliasing config state. Validated config
// rejects blanks; dropping them here deliberately protects Config values built
// directly in code without weakening the validation policy.
//
// An empty result means no gate is configured and the validate step passes
// unconditionally.
func ResolveValidationCommands(cfg Config) []string {
	return normalizeValidationCommands(cfg.Defaults.ValidationCommands)
}

// ResolveProjectValidationCommands returns the operator-authored mechanical
// gate for one project. A project policy wins over the deprecated global
// fallback, including an explicit opt-out.
func ResolveProjectValidationCommands(cfg Config, projectID string) []string {
	project := findConfiguredProject(cfg.Projects, projectID)
	if project != nil && project.Validation != nil {
		if project.Validation.OptOut {
			return nil
		}
		return normalizeValidationCommands(project.Validation.Commands)
	}
	return ResolveValidationCommands(cfg)
}

// ResolveProjectValidationCommandsByID snapshots every registered project's
// effective commands. Explicit opt-outs remain present empty entries so runtime
// consumers do not confuse them with an absent policy.
func ResolveProjectValidationCommandsByID(cfg Config) map[string][]string {
	resolved := make(map[string][]string, len(cfg.Projects))
	for _, project := range cfg.Projects {
		resolved[project.ID] = ResolveProjectValidationCommands(cfg, project.ID)
	}
	return resolved
}

func ProjectValidationOptedOut(cfg Config, projectID string) bool {
	project := findConfiguredProject(cfg.Projects, projectID)
	return project != nil && project.Validation != nil && project.Validation.OptOut
}

func ProjectValidationUsesLegacyDefaults(cfg Config, projectID string) bool {
	project := findConfiguredProject(cfg.Projects, projectID)
	return project != nil && project.Validation == nil && len(ResolveValidationCommands(cfg)) > 0
}

// HasEffectiveValidationCommands reports whether daemon startup needs the
// trusted validation sandbox for at least one configured project. The global
// list still applies before a project catalog has been imported.
func HasEffectiveValidationCommands(cfg Config) bool {
	if len(cfg.Projects) == 0 {
		return len(ResolveValidationCommands(cfg)) > 0
	}
	for _, project := range cfg.Projects {
		if len(ResolveProjectValidationCommands(cfg, project.ID)) > 0 {
			return true
		}
	}
	return false
}

func normalizeValidationCommands(source []string) []string {
	commands := make([]string, 0, len(source))
	for _, command := range source {
		trimmed := strings.TrimSpace(command)
		if trimmed != "" {
			commands = append(commands, trimmed)
		}
	}
	if len(commands) == 0 {
		return nil
	}
	return commands
}
