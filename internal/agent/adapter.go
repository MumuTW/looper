package agent

import (
	"strings"

	"github.com/MumuTW/looper/internal/config"
)

// runtimeAdapter owns the CLI-shaped parts of an agent runtime. Process
// ownership, persistence, timeouts, checkpoints, and worktree authority remain
// in ConfiguredExecutor.
type runtimeAdapter struct {
	contract                 RuntimeContract
	resolveStartArgs         func(ExecutorConfig, []string, string, string) []string
	resolveNativeResumeArgs  func(ExecutorConfig, []string, string, string, string) []string
	resolveInteractiveResume func(string, string) string
}

var runtimeAdapters = map[config.AgentVendor]runtimeAdapter{
	config.AgentVendorClaudeCode: {
		contract: characterizedContract(config.AgentVendorClaudeCode, "claude", map[RuntimeCapability]CapabilityEvidence{
			CapabilitySessionIdentity:     translatedEvidence("executor/session-id-extraction"),
			CapabilityHeadlessResume:      nativeEvidence("claude/--resume-print"),
			CapabilityInteractiveTakeover: nativeEvidence("claude/--resume-verified-2026-07"),
		}),
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveClaudeArgs(cfg, args, prompt)
		},
		resolveNativeResumeArgs: resolveClaudeNativeResumeArgs,
		resolveInteractiveResume: func(command, sessionID string) string {
			return command + " --resume " + shellSingleQuote(sessionID)
		},
	},
	config.AgentVendorCodex: {
		contract: characterizedContract(config.AgentVendorCodex, "codex", map[RuntimeCapability]CapabilityEvidence{
			CapabilityStructuredLiveEvents:   nativeEvidence("codex/exec-jsonl"),
			CapabilitySessionIdentity:        translatedEvidence("codex/thread-id-jsonl"),
			CapabilityHeadlessResume:         nativeEvidence("codex/exec-resume"),
			CapabilityInteractiveTakeover:    nativeEvidence("codex/resume-verified-2026-07"),
			CapabilityToolNetworkRestriction: enforcedEvidence("codex/validation-sandbox"),
		}),
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveCodexArgs(cfg, args, prompt)
		},
		resolveNativeResumeArgs: resolveCodexNativeResumeArgs,
		resolveInteractiveResume: func(command, sessionID string) string {
			return command + " resume " + shellSingleQuote(sessionID)
		},
	},
	config.AgentVendorOpenCode: {
		contract: characterizedContract(config.AgentVendorOpenCode, "opencode", map[RuntimeCapability]CapabilityEvidence{
			CapabilitySessionIdentity: translatedEvidence("executor/session-id-extraction"),
			CapabilityHeadlessResume:  nativeEvidence("opencode/run-session"),
		}),
		resolveStartArgs:        resolveOpenCodeArgs,
		resolveNativeResumeArgs: resolveOpenCodeNativeResumeArgs,
	},
	config.AgentVendorCursorCLI: {
		contract: characterizedContract(config.AgentVendorCursorCLI, "agent", map[RuntimeCapability]CapabilityEvidence{
			CapabilitySessionIdentity: translatedEvidence("cursor/chat-id-extraction"),
			CapabilityHeadlessResume:  nativeEvidence("cursor/--resume-print"),
		}),
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveCursorArgs(cfg, args, prompt)
		},
		resolveNativeResumeArgs: resolveCursorNativeResumeArgs,
	},
	config.AgentVendorGrokBuild: {
		contract:         characterizedContract(config.AgentVendorGrokBuild, "grok", nil),
		resolveStartArgs: resolveGrokArgs,
	},
	config.AgentVendorDevinExperimental: {
		contract: characterizedContract(config.AgentVendorDevinExperimental, "devin", map[RuntimeCapability]CapabilityEvidence{
			CapabilityExecutableDiscovery:  experimentalEvidence("devin-3000.3.22", "devin/print-fixture"),
			CapabilityNonInteractivePrompt: experimentalEvidence("devin-3000.3.22", "devin/--print"),
			CapabilityModelSelection:       experimentalEvidence("devin-3000.3.22", "devin/--model"),
		}),
		resolveStartArgs: func(cfg ExecutorConfig, args []string, _ string, prompt string) []string {
			return resolveDevinArgs(cfg, args, prompt)
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
