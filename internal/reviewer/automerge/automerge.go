package automerge

import (
	"strings"

	"github.com/nexu-io/looper/internal/config"
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
	if !autoMergeConfig.Enabled {
		return RefuseWithReason(RefusalReasonDisabled)
	}
	if !hasLooperLabel(pr.Labels) || !pr.HasTrackedIssueLink {
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

func hasLooperLabel(labels []string) bool {
	for _, label := range labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(label)), "looper:") {
			return true
		}
	}
	return false
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
