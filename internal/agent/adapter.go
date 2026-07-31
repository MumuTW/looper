package agent

import (
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/validationcmd"
)

// runtimeAdapter owns the CLI-shaped parts of an agent runtime. Process
// ownership, persistence, timeouts, checkpoints, and worktree authority remain
// in ConfiguredExecutor.
type runtimeAdapter struct {
	contract                 RuntimeContract
	resolveStartArgs         func(ExecutorConfig, []string, string, string) []string
	resolveNativeResumeArgs  func(ExecutorConfig, []string, string, string, string) []string
	resolveInteractiveResume func(string, string) string
	// enforceToolNetworkDenied rewrites spawn args for a validation-gated run so
	// the agent's tool subprocesses cannot reach the network while the parent
	// agent keeps the connection it needs for model transport. A nil hook means
	// the vendor cannot express that capability, and Start refuses the run
	// rather than executing it unrestricted.
	enforceToolNetworkDenied func(args []string, prompt string, sandbox *validationcmd.Sandbox) ([]string, error)
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
		enforceToolNetworkDenied: func(args []string, prompt string, sandbox *validationcmd.Sandbox) ([]string, error) {
			return enforceCodexToolNetworkDenied(args, prompt, sandbox), nil
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
		enforceToolNetworkDenied: enforceDevinToolNetworkDenied,
	},
}

func runtimeAdapterFor(vendor config.AgentVendor) (runtimeAdapter, bool) {
	adapter, ok := runtimeAdapters[vendor]
	return adapter, ok
}

// VendorSupportsToolNetworkDenial reports whether a vendor's adapter can run a
// validation-gated execution with its tool subprocesses cut off from the
// network. Unknown vendors report false so the gate fails closed.
func VendorSupportsToolNetworkDenial(vendor config.AgentVendor) bool {
	adapter, ok := runtimeAdapterFor(vendor)
	return ok && adapter.enforceToolNetworkDenied != nil
}

// ToolNetworkDenialVendors returns the vendors whose adapters implement
// tool-network denial, sorted for stable messages. This adapter table is the
// source of truth; config.ToolNetworkDenialVendors mirrors it for validation
// and a drift test keeps the two identical.
func ToolNetworkDenialVendors() []config.AgentVendor {
	vendors := make([]config.AgentVendor, 0, len(runtimeAdapters))
	for vendor, adapter := range runtimeAdapters {
		if adapter.enforceToolNetworkDenied != nil {
			vendors = append(vendors, vendor)
		}
	}
	sort.Slice(vendors, func(i, j int) bool { return vendors[i] < vendors[j] })
	return vendors
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
