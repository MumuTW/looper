package config

import (
	"reflect"
	"testing"
)

func TestIsHotEditablePathUsesExplicitAllowlist(t *testing.T) {
	t.Parallel()

	allowed := []string{
		"agent.vendor",
		"agent.model",
		"agent.reasoningEffort",
		"agent.env",
		"agent.env.OPENAI_API_KEY",
		"agent.timeouts.workerMaxRuntimeSeconds",
		"agent.profiles.fast",
		"agent.profiles.fast.vendor",
		"agent.profiles.fast.model",
		"agent.profiles.fast.reasoningEffort",
		"agent.profiles.my-profile_1.vendor",
		"roles.planner.agent.profile",
		"roles.worker.agent.vendor",
		"roles.reviewer.agent.model",
		"roles.fixer.agent.reasoningEffort",
		"roles.fixer.agent.profile",
		"scheduler.maxConcurrentRuns",
		"scheduler.slowLaneWarnThresholdMs",
		"scheduler.agentBrownout.enabled",
		"scheduler.agentBrownout.windowSeconds",
		"scheduler.agentBrownout.minFailures",
		"scheduler.agentBrownout.failureRatio",
		"scheduler.agentBrownout.cooldownSeconds",
		"scheduler.agentBrownout.maxCooldownSeconds",
		"scheduler.agentBrownout.probeSuccesses",
		"scheduler.agentBrownout.notify",
		"daemon.worktreeCleanup.enabled",
		"daemon.worktreeCleanup.interval",
		"daemon.worktreeCleanup.dryRun",
		"daemon.worktreeCleanup.includeOrphans",
		"daemon.worktreeCleanup.maxPerTick",
		"daemon.worktreeCleanup.maxDiskSweepPerTick",
		"daemon.worktreeCleanup.retentionDays",
		"daemon.resourceGuard.enabled",
		"daemon.resourceGuard.minDiskFreePercent",
		"daemon.resourceGuard.minDiskFreeGb",
		"daemon.resourceGuard.maxLoadPerCpu",
		"notifications.inApp",
		"disclosure.channels.gitCommit",
		"defaults.allowAutoPush",
		"instructions.enabled",
		"roles.auditor.allowRevertProposals",
		"roles.reviewer.behavior.scope",
		"roles.reviewer.behavior.convergence.maxConsecutiveUnproductive",
		"roles.reviewer.behavior.convergence.maxFixerAttemptsPerItem",
		"roles.reviewer.behavior.convergence.maxTotalRounds",
		"roles.reviewer.behavior.convergence.severityFloor",
		"roles.coordinator.conflictPolicy.maxRepairs",
		"tools.looperPath",
		"tools.osascriptPath",
	}
	for _, path := range allowed {
		path := path
		t.Run("allowed_"+path, func(t *testing.T) {
			t.Parallel()
			if !IsHotEditablePath(path) {
				t.Fatalf("IsHotEditablePath(%q) = false, want true", path)
			}
		})
	}

	rejected := []string{
		"",
		" agent.model",
		"agent",
		"agent.params",
		"agent.params.temperature",
		"agent.profiles",
		"agent.profiles.fast.params",
		"agent.profiles.fast.unknown",
		"agent.profiles.bad.id.vendor",
		"agent.nativeResume.enabled",
		"agent.timeouts.workerSeconds",
		"scheduler",
		"scheduler.agentBrownout",
		"scheduler.pollIntervalSeconds",
		"scheduler.discoveryCacheTtlSeconds",
		"scheduler.retryMaxAttempts",
		"scheduler.retryBaseDelayMs",
		"tools",
		"tools.gitPath",
		"tools.ghPath",
		"server.port",
		"storage.dbPath",
		"providers",
		"projects",
		"logging.level",
		"daemon.mode",
		"daemon.worktreeCleanup",
		"daemon.resourceGuard",
		"daemon.logDir",
		"webhook.enabled",
		"network.enrolled",
		"hitl.github.mentionLogins",
		"notifications.webhook.verificationTokenEnv",
		"roles.gatekeeper.trust",
		"roles.coordinator.enabled",
		"roles.coordinator.dependencies.enabled",
		"roles.coordinator.agent",
		"roles.coordinator.agent.vendor",
		"roles.worker.agent",
		"roles.worker.agent.params",
		"instructions.maxBytes",
		"defaults.allowAutoMerge",
		"defaults.baseBranch",
		"roles.reviewer.behavior.loop.minPublishIntervalSeconds",
		"roles.reviewer.behavior.loop.quietPeriodSeconds",
		"roles.reviewer.behavior.retry.maxDelayMs",
		"roles.coordinator.mergeWatch.transientRetries",
		"roles..planner",
	}
	for _, path := range rejected {
		path := path
		t.Run("rejected_"+path, func(t *testing.T) {
			t.Parallel()
			if IsHotEditablePath(path) {
				t.Fatalf("IsHotEditablePath(%q) = true, want false", path)
			}
		})
	}
}

func TestRestartRequiredChangesReturnsConcreteSortedPaths(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	newConfig := CloneConfig(oldConfig)
	newConfig.Server.Port++
	newConfig.Scheduler.PollIntervalSeconds++
	newConfig.Scheduler.DiscoveryCacheTTLSeconds++
	newConfig.Scheduler.RetryMaxAttempts++
	newConfig.Scheduler.RetryBaseDelayMS++
	newConfig.Scheduler.MaxConcurrentRuns++
	newConfig.Agent.Model = stringPtr("hot-model")
	newConfig.Agent.Params["temperature"] = 0.5
	newConfig.Tools.GitPath = stringPtr("/restart/git")
	newConfig.Tools.LooperPath = stringPtr("/hot/looper")
	newConfig.Logging.Level = LogLevelDebug
	newConfig.Daemon.Environment["NEW_VAR"] = "value"
	newConfig.Roles.Planner.Triggers.Labels = []string{"hot-label"}
	newConfig.Defaults.BaseBranch = "develop"
	newConfig.Roles.Coordinator.Enabled = !newConfig.Roles.Coordinator.Enabled
	newConfig.Providers = append(newConfig.Providers, ProviderConfig{ID: "ghes", Kind: ProviderKindGitHub, BaseURL: "https://ghe.example.test"})
	newConfig.Projects = append(newConfig.Projects, ProjectRefConfig{ID: "import"})

	want := []string{
		"agent.params.temperature",
		"daemon.environment.NEW_VAR",
		"defaults.baseBranch",
		"logging.level",
		"projects",
		"providers",
		"roles.coordinator.enabled",
		"scheduler.discoveryCacheTtlSeconds",
		"scheduler.pollIntervalSeconds",
		"scheduler.retryBaseDelayMs",
		"scheduler.retryMaxAttempts",
		"server.port",
		"tools.gitPath",
	}
	if got := RestartRequiredChanges(oldConfig, newConfig); !reflect.DeepEqual(got, want) {
		t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
	}
}

func TestRestartRequiredChangesIgnoresHotOnlyCandidate(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	newConfig := CloneConfig(oldConfig)
	newConfig.Agent.Env["TOKEN"] = "new"
	newConfig.Notifications.InApp = !newConfig.Notifications.InApp
	newConfig.Defaults.AllowAutoPush = !newConfig.Defaults.AllowAutoPush
	newConfig.Disclosure.Enabled = !newConfig.Disclosure.Enabled
	newConfig.Tools.OsascriptPath = stringPtr("/hot/osascript")
	newConfig.Agent.Timeouts.WorkerMaxRuntimeSeconds++
	newConfig.Agent.Timeouts.WorkerSeconds = newConfig.Agent.Timeouts.WorkerMaxRuntimeSeconds
	newConfig.Defaults.AllowAutoApprove = true
	newConfig.Roles.Reviewer.Behavior.ReviewEvents.Clean = ReviewerReviewEventApprove
	newConfig.Defaults.FixAllPullRequests = true
	newConfig.Roles.Fixer.Triggers.AuthorFilter = FixerAuthorFilterAny

	if got := RestartRequiredChanges(oldConfig, newConfig); len(got) != 0 {
		t.Fatalf("RestartRequiredChanges() = %#v, want no restart-bound changes", got)
	}
}

func TestRestartRequiredChangesGuardsVendorSpecificCompanions(t *testing.T) {
	t.Parallel()

	base, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	oldVendor := AgentVendorCodex
	base.Agent.Vendor = &oldVendor

	t.Run("first vendor can activate a preconfigured profile", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Vendor = nil
		old.Agent.Model = stringPtr("gpt-5")
		old.Agent.Params["command"] = "/opt/codex-wrapper"
		candidate := CloneConfig(old)
		vendor := AgentVendorCodex
		candidate.Agent.Vendor = &vendor
		if got := RestartRequiredChanges(old, candidate); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges() = %#v, want no blockers", got)
		}
	})

	t.Run("vendor alone is hot when model and params are implicit", func(t *testing.T) {
		candidate := CloneConfig(base)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		if got := RestartRequiredChanges(base, candidate); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges() = %#v, want no blockers", got)
		}
	})

	t.Run("unchanged explicit model blocks vendor-only edit", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Model = stringPtr("gpt-5")
		candidate := CloneConfig(old)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		if got, want := RestartRequiredChanges(old, candidate), []string{"agent.model"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
		}
	})

	t.Run("paired model edit makes the vendor choice explicit", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Model = stringPtr("gpt-5")
		candidate := CloneConfig(old)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		candidate.Agent.Model = stringPtr("claude-sonnet")
		if got := RestartRequiredChanges(old, candidate); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges() = %#v, want no blockers", got)
		}
	})

	t.Run("configured command or args requires restart", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Params["command"] = "/opt/codex-wrapper"
		candidate := CloneConfig(old)
		vendor := AgentVendorClaudeCode
		candidate.Agent.Vendor = &vendor
		if got, want := RestartRequiredChanges(old, candidate), []string{"agent.params"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
		}
	})

	t.Run("vendor cannot be cleared to launder a retained profile", func(t *testing.T) {
		old := CloneConfig(base)
		old.Agent.Model = stringPtr("gpt-5")
		old.Agent.Params["command"] = "/opt/codex-wrapper"
		disabled := CloneConfig(old)
		disabled.Agent.Vendor = nil
		if got, want := RestartRequiredChanges(old, disabled), []string{"agent.model", "agent.params"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("RestartRequiredChanges(codex -> nil) = %#v, want %#v", got, want)
		}

		claude := CloneConfig(disabled)
		vendor := AgentVendorClaudeCode
		claude.Agent.Vendor = &vendor
		if got := RestartRequiredChanges(disabled, claude); len(got) != 0 {
			t.Fatalf("RestartRequiredChanges(nil -> claude) = %#v, want prepared-profile activation allowed", got)
		}
	})
}

func TestRestartRequiredChangesRoleAgentModelIsHot(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := AgentVendorCodex
	oldConfig.Agent.Vendor = &vendor
	oldConfig.Agent.Model = stringPtr("gpt-5")
	oldConfig.Agent.Params = map[string]any{}

	newConfig := CloneConfig(oldConfig)
	newConfig.Roles.Reviewer.Agent = &RoleAgentConfig{Model: stringPtr("o3")}

	if got := RestartRequiredChanges(oldConfig, newConfig); len(got) != 0 {
		t.Fatalf("RestartRequiredChanges() = %#v, want no restart-bound changes for role model-only edit", got)
	}
}

func TestRestartRequiredChangesReasoningEffortIsHotAtEveryScope(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	low := ReasoningEffortLow
	oldConfig.Agent.ReasoningEffort = &low
	oldConfig.Agent.Profiles = map[string]AgentBindingConfig{
		"fast": {ReasoningEffort: &low},
	}
	oldConfig.Roles.Worker.Agent = &RoleAgentConfig{ReasoningEffort: &low}

	newConfig := CloneConfig(oldConfig)
	high := ReasoningEffortHigh
	newConfig.Agent.ReasoningEffort = &high
	profile := newConfig.Agent.Profiles["fast"]
	profile.ReasoningEffort = &high
	newConfig.Agent.Profiles["fast"] = profile
	newConfig.Roles.Worker.Agent.ReasoningEffort = &high

	if got := RestartRequiredChanges(oldConfig, newConfig); len(got) != 0 {
		t.Fatalf("RestartRequiredChanges() = %#v, want no restart-bound reasoning effort changes", got)
	}
}

func TestRestartRequiredChangesResolvedVendorWithParamsRequiresRestart(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	globalVendor := AgentVendorCodex
	oldConfig.Agent.Vendor = &globalVendor
	oldConfig.Agent.Model = stringPtr("gpt-5")
	oldConfig.Agent.Params = map[string]any{"command": "/opt/codex-wrapper"}
	oldConfig.Agent.Profiles = map[string]AgentBindingConfig{
		"strong": {Vendor: agentVendorPtr(AgentVendorClaudeCode), Model: stringPtr("claude-sonnet")},
	}

	newConfig := CloneConfig(oldConfig)
	newConfig.Roles.Reviewer.Agent = &RoleAgentConfig{Profile: stringPtr("strong")}

	if got, want := RestartRequiredChanges(oldConfig, newConfig), []string{"agent.params"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
	}
}

func TestRestartRequiredChangesResolvedVendorSameModelBlocks(t *testing.T) {
	t.Parallel()

	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	globalVendor := AgentVendorCodex
	oldConfig.Agent.Vendor = &globalVendor
	oldConfig.Agent.Model = stringPtr("shared-model")
	oldConfig.Agent.Params = map[string]any{}
	roleVendor := AgentVendorClaudeCode
	oldConfig.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &roleVendor, Model: stringPtr("shared-model")}

	newConfig := CloneConfig(oldConfig)
	newVendor := AgentVendorCodex
	newConfig.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &newVendor, Model: stringPtr("shared-model")}

	if got, want := RestartRequiredChanges(oldConfig, newConfig), []string{"roles.worker.agent.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RestartRequiredChanges() = %#v, want %#v", got, want)
	}
}

func TestRestartRequiredChangesBlocksVendorResetModelLaundering(t *testing.T) {
	t.Parallel()

	// Inline role binding: unset vendor while retaining model must not be hot.
	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	oldConfig.Agent.Params = map[string]any{}
	roleVendor := AgentVendorCodex
	oldConfig.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &roleVendor, Model: stringPtr("gpt-5")}

	unsetVendor := CloneConfig(oldConfig)
	unsetVendor.Roles.Worker.Agent = &RoleAgentConfig{Model: stringPtr("gpt-5")}
	if got, want := RestartRequiredChanges(oldConfig, unsetVendor), []string{"roles.worker.agent.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unset role vendor retaining model = %#v, want %#v", got, want)
	}

	// Single-step vendor switch retaining model is also blocked.
	switched := CloneConfig(oldConfig)
	newVendor := AgentVendorClaudeCode
	switched.Roles.Worker.Agent = &RoleAgentConfig{Vendor: &newVendor, Model: stringPtr("gpt-5")}
	if got, want := RestartRequiredChanges(oldConfig, switched), []string{"roles.worker.agent.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switch role vendor retaining model = %#v, want %#v", got, want)
	}

	// Profile-based binding: same laundering via profile vendor unset.
	profileOld, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	profileOld.Agent.Params = map[string]any{}
	profileOld.Agent.Profiles = map[string]AgentBindingConfig{
		"worker": {Vendor: &roleVendor, Model: stringPtr("gpt-5")},
	}
	profileOld.Roles.Worker.Agent = &RoleAgentConfig{Profile: stringPtr("worker")}

	profileUnset := CloneConfig(profileOld)
	profileUnset.Agent.Profiles = map[string]AgentBindingConfig{
		"worker": {Model: stringPtr("gpt-5")},
	}
	if got, want := RestartRequiredChanges(profileOld, profileUnset), []string{"agent.profiles.worker.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unset profile vendor retaining model = %#v, want %#v", got, want)
	}
}

func TestRestartRequiredChangesGuardsGlobalVendorWhenRolesOverride(t *testing.T) {
	t.Parallel()

	// Every coding role resolves vendor from a profile/inline binding, so the
	// role-resolved loop sees no vendor change. Global agent.vendor still owns
	// coordinator triage and agent.params and must be guarded independently.
	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	globalVendor := AgentVendorCodex
	roleVendor := AgentVendorClaudeCode
	oldConfig.Agent.Vendor = &globalVendor
	oldConfig.Agent.Model = stringPtr("gpt-5")
	oldConfig.Agent.Params = map[string]any{"command": "/opt/codex-wrapper"}
	oldConfig.Agent.Profiles = map[string]AgentBindingConfig{
		"coding": {Vendor: &roleVendor, Model: stringPtr("claude-sonnet")},
	}
	oldConfig.Roles.Planner.Agent = &RoleAgentConfig{Profile: stringPtr("coding")}
	oldConfig.Roles.Worker.Agent = &RoleAgentConfig{Profile: stringPtr("coding")}
	oldConfig.Roles.Reviewer.Agent = &RoleAgentConfig{Profile: stringPtr("coding")}
	oldConfig.Roles.Fixer.Agent = &RoleAgentConfig{Vendor: &roleVendor, Model: stringPtr("claude-sonnet")}

	newConfig := CloneConfig(oldConfig)
	switched := AgentVendorClaudeCode
	newConfig.Agent.Vendor = &switched

	if got, want := RestartRequiredChanges(oldConfig, newConfig), []string{"agent.model", "agent.params"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("global vendor switch with role overrides = %#v, want %#v", got, want)
	}

	// Paired global model clear makes the vendor switch explicit for model, but
	// retained params still require restart.
	clearedModel := CloneConfig(newConfig)
	clearedModel.Agent.Model = stringPtr("")
	if got, want := RestartRequiredChanges(oldConfig, clearedModel), []string{"agent.params"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("global vendor switch with model cleared = %#v, want %#v", got, want)
	}

	// Clean global switch (no params, paired model change) remains hot.
	baseClean := CloneConfig(oldConfig)
	baseClean.Agent.Params = map[string]any{}
	clean := CloneConfig(baseClean)
	clean.Agent.Vendor = &switched
	clean.Agent.Model = stringPtr("claude-sonnet")
	if got := RestartRequiredChanges(baseClean, clean); len(got) != 0 {
		t.Fatalf("clean global vendor+model switch = %#v, want no blockers", got)
	}
}

func TestRestartRequiredChangesReportsCanonicalRoleModelPath(t *testing.T) {
	t.Parallel()
	oldConfig, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	oldConfig.Agent.Params = map[string]any{}
	roleVendor := AgentVendorCodex
	oldConfig.Roles.Coding = CodingRolesFromLegacy(oldConfig.Roles)
	worker := oldConfig.Roles.Coding[CodingRoleWorker]
	worker.Agent = &RoleAgentConfig{Vendor: &roleVendor, Model: stringPtr("gpt-5")}
	oldConfig.Roles.Coding[CodingRoleWorker] = worker

	switched := CloneConfig(oldConfig)
	newVendor := AgentVendorClaudeCode
	worker = switched.Roles.Coding[CodingRoleWorker]
	worker.Agent = &RoleAgentConfig{Vendor: &newVendor, Model: stringPtr("gpt-5")}
	switched.Roles.Coding[CodingRoleWorker] = worker

	if got, want := RestartRequiredChanges(oldConfig, switched), []string{"roles.coding.worker", "roles.coding.worker.agent.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical role vendor switch = %#v, want %#v", got, want)
	}
}

func TestRestartRequiredChangesPreservesNormalizedLegacyRoleModelPath(t *testing.T) {
	t.Parallel()

	oldConfig, err := Normalize(t.TempDir(), mustDecodeTOML(t, `
[roles.worker.agent]
vendor = "codex"
model = "gpt-5"
`))
	if err != nil {
		t.Fatalf("Normalize(old) error = %v", err)
	}
	newConfig := CloneConfig(oldConfig)
	newVendor := AgentVendorClaudeCode
	newConfig.Roles.Worker.Agent.Vendor = &newVendor
	newConfig.Roles.Coding = CodingRolesFromLegacy(newConfig.Roles)

	if got, want := RestartRequiredChanges(oldConfig, newConfig), []string{"roles.worker.agent.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized legacy role vendor switch = %#v, want %#v", got, want)
	}
}

func TestRestartRequiredChangesPreservesCanonicalOwnerForEqualRoleModels(t *testing.T) {
	t.Parallel()

	oldConfig, err := Normalize(t.TempDir(), mustDecodeTOML(t, `
[roles.worker.agent]
vendor = "codex"
model = "gpt-5"

[roles.coding.worker.agent]
vendor = "codex"
model = "gpt-5"
`))
	if err != nil {
		t.Fatalf("Normalize(old) error = %v", err)
	}
	switched := CloneConfig(oldConfig)
	newVendor := AgentVendorClaudeCode
	worker := switched.Roles.Coding[CodingRoleWorker]
	worker.Agent.Vendor = &newVendor
	switched.Roles.Coding[CodingRoleWorker] = worker

	if got, want := RestartRequiredChanges(oldConfig, switched), []string{"roles.coding.worker", "roles.coding.worker.agent.model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("equal dual-authored model switch = %#v, want %#v", got, want)
	}
}

func agentVendorPtr(v AgentVendor) *AgentVendor {
	return &v
}
