package automerge

import (
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/labels"
)

type RefusalReason string

const (
	RefusalReasonDisabled           RefusalReason = "disabled"
	RefusalReasonScope              RefusalReason = "scope"
	RefusalReasonNoBranchProtection RefusalReason = "no-branch-protection"
	RefusalReasonStrategyDisallowed RefusalReason = "strategy-disallowed"
	RefusalReasonAutoMergeDisabled  RefusalReason = "auto-merge-disabled"
)

type PRSnapshot struct {
	Labels              []string
	HasTrackedIssueLink bool
}

type BranchProtectionSnapshot struct {
	Exists            bool
	HasRequiredChecks bool
}

type RepoSettingsSnapshot struct {
	AllowSquashMerge bool
	AllowMergeCommit bool
	AllowRebaseMerge bool
	AllowAutoMerge   bool
}

type AutoMergeDecision struct {
	Strategy config.ReviewerAutoMergeStrategy
	Reason   RefusalReason
}

func OptInWithStrategy(strategy config.ReviewerAutoMergeStrategy) AutoMergeDecision {
	return AutoMergeDecision{Strategy: strategy}
}

func RefuseWithReason(reason RefusalReason) AutoMergeDecision {
	return AutoMergeDecision{Reason: reason}
}

func Decide(pr PRSnapshot, autoMergeConfig config.ReviewerAutoMergeConfig, protection BranchProtectionSnapshot, settings RepoSettingsSnapshot) AutoMergeDecision {
	return DecideForNamespace(pr, autoMergeConfig, protection, settings, labels.DefaultNamespace())
}

// DecideForNamespace applies the auto-merge policy against the project's label
// namespace. The namespace is an authority boundary: a PR carrying another
// Looper instance's labels must not opt this instance into auto-merge.
func DecideForNamespace(pr PRSnapshot, autoMergeConfig config.ReviewerAutoMergeConfig, protection BranchProtectionSnapshot, settings RepoSettingsSnapshot, namespace labels.Namespace) AutoMergeDecision {
	if !autoMergeConfig.Enabled {
		return RefuseWithReason(RefusalReasonDisabled)
	}
	if !namespace.AnyOwned(pr.Labels) || !pr.HasTrackedIssueLink {
		return RefuseWithReason(RefusalReasonScope)
	}
	if autoMergeConfig.RequireBranchProtection && (!protection.Exists || !protection.HasRequiredChecks) {
		return RefuseWithReason(RefusalReasonNoBranchProtection)
	}
	if !StrategyAllowed(autoMergeConfig.Strategy, settings) {
		return RefuseWithReason(RefusalReasonStrategyDisallowed)
	}
	if !settings.AllowAutoMerge {
		return RefuseWithReason(RefusalReasonAutoMergeDisabled)
	}
	return OptInWithStrategy(autoMergeConfig.Strategy)
}

func StrategyAllowed(strategy config.ReviewerAutoMergeStrategy, settings RepoSettingsSnapshot) bool {
	switch strategy {
	case config.ReviewerAutoMergeStrategySquash:
		return settings.AllowSquashMerge
	case config.ReviewerAutoMergeStrategyMerge:
		return settings.AllowMergeCommit
	case config.ReviewerAutoMergeStrategyRebase:
		return settings.AllowRebaseMerge
	default:
		return false
	}
}
