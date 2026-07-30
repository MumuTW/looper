package agent

import (
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// runtimeAdapter owns the CLI-shaped parts of an agent runtime. Process
// ownership, persistence, timeouts, checkpoints, and worktree authority remain
// in ConfiguredExecutor.
type runtimeAdapter struct {
	command                  string
	resolveStartArgs         func(ExecutorConfig, []string, string, string) []string
	resolveNativeResumeArgs  func(ExecutorConfig, []string, string, string, string) []string
	resolveInteractiveResume func(string, string) string
}

var runtimeAdapters = map[config.AgentVendor]runtimeAdapter{
	config.AgentVendorClaudeCode: {
		command: "claude",
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveClaudeArgs(cfg, args, prompt)
		},
		resolveNativeResumeArgs: resolveClaudeNativeResumeArgs,
		resolveInteractiveResume: func(command, sessionID string) string {
			return command + " --resume " + shellSingleQuote(sessionID)
		},
	},
	config.AgentVendorCodex: {
		command: "codex",
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveCodexArgs(cfg, args, prompt)
		},
		resolveNativeResumeArgs: resolveCodexNativeResumeArgs,
		resolveInteractiveResume: func(command, sessionID string) string {
			return command + " resume " + shellSingleQuote(sessionID)
		},
	},
	config.AgentVendorOpenCode: {
		command:                 "opencode",
		resolveStartArgs:        resolveOpenCodeArgs,
		resolveNativeResumeArgs: resolveOpenCodeNativeResumeArgs,
	},
	config.AgentVendorCursorCLI: {
		command: "agent",
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveCursorArgs(cfg, args, prompt)
		},
		resolveNativeResumeArgs: resolveCursorNativeResumeArgs,
	},
	config.AgentVendorGrokBuild: {
		command:          "grok",
		resolveStartArgs: resolveGrokArgs,
	},
	config.AgentVendorDevinExperimental: {
		command: "devin",
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveDevinArgs(cfg, args, prompt)
		},
	},
	config.AgentVendorHermes: {
		command: "hermes",
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveHermesArgs(cfg, args, prompt)
		},
	},
}

func runtimeAdapterFor(vendor config.AgentVendor) (runtimeAdapter, bool) {
	adapter, ok := runtimeAdapters[vendor]
	return adapter, ok
}

func resolveClaudeNativeResumeArgs(cfg ExecutorConfig, args []string, _ string, sessionID string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
	if !hasAnyFlag(resolved, []string{"--continue", "--resume"}) {
		resolved = append(resolved, "--resume", sessionID)
	}
	if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
		resolved = append(resolved, "--print", prompt)
	}
	if !hasAnyFlag(resolved, []string{"--dangerously-skip-permissions"}) {
		resolved = append(resolved, "--dangerously-skip-permissions")
	}
	return resolved
}

func resolveCodexNativeResumeArgs(cfg ExecutorConfig, args []string, _ string, sessionID string, prompt string) []string {
	resolved := removeFirstArg(args, "exec")
	resolved = removeFirstArg(resolved, "resume")
	withModel := prependModelFlag(append([]string{"exec"}, resolved...), cfg.Model, "--model", []string{"--model", "-m"})
	withModel = appendCodexSandboxDefaults(withModel)
	base := append(withModel, "resume")
	base = append(base, sessionID)
	if containsArg(withModel, "-") {
		return base
	}
	return append(base, prompt)
}

func resolveOpenCodeNativeResumeArgs(cfg ExecutorConfig, args []string, workingDirectory string, sessionID string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model", "-m"})
	if !containsArg(resolved, "run") {
		resolved = append([]string{"run"}, resolved...)
	}
	if strings.TrimSpace(workingDirectory) != "" && !hasAnyFlag(resolved, []string{"--dir"}) {
		resolved = appendDirFlag(resolved, workingDirectory)
	}
	if !hasAnyFlag(resolved, []string{"--session", "--continue"}) {
		resolved = append(resolved, "--session", sessionID)
	}
	if !hasAnyFlag(resolved, []string{"-p", "--prompt", "-f", "--file"}) {
		resolved = append(resolved, prompt)
	}
	return resolved
}

func resolveCursorNativeResumeArgs(cfg ExecutorConfig, args []string, _ string, sessionID string, prompt string) []string {
	resolved := prependModelFlag(args, cfg.Model, "--model", []string{"--model"})
	if !hasAnyFlag(resolved, []string{"--continue", "--resume"}) {
		resolved = append(resolved, "--resume", sessionID)
	}
	if !hasAnyFlag(resolved, []string{"-p", "--print"}) {
		resolved = append(resolved, "--print", prompt)
	}
	return resolved
}
