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
	commands := make([]string, 0, len(cfg.Defaults.ValidationCommands))
	for _, command := range cfg.Defaults.ValidationCommands {
		trimmed := strings.TrimSpace(command)
		if trimmed == "" {
			continue
		}
		commands = append(commands, trimmed)
	}
	if len(commands) == 0 {
		return nil
	}
	return commands
}
