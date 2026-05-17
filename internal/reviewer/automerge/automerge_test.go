package automerge

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	baseConfig := config.ReviewerAutoMergeConfig{
		Enabled:                 true,
		Strategy:                config.ReviewerAutoMergeStrategySquash,
		RequireBranchProtection: true,
		TransientRetries:        3,
		Scope:                   config.ReviewerAutoMergeScopeLooperOnly,
	}
	basePR := PRSnapshot{Labels: []string{"looper:worker-ready"}, HasTrackedIssueLink: true}
	baseProtection := BranchProtectionSnapshot{Exists: true, HasRequiredChecks: true}
	baseSettings := RepoSettingsSnapshot{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: true}

	tests := []struct {
		name         string
		pr           PRSnapshot
		cfg          config.ReviewerAutoMergeConfig
		protection   BranchProtectionSnapshot
		settings     RepoSettingsSnapshot
		wantDecision AutoMergeDecision
	}{
		{name: "full pass", pr: basePR, cfg: baseConfig, protection: baseProtection, settings: baseSettings, wantDecision: OptInWithStrategy(config.ReviewerAutoMergeStrategySquash)},
		{name: "mixed case looper label", pr: PRSnapshot{Labels: []string{"Looper:worker-ready"}, HasTrackedIssueLink: true}, cfg: baseConfig, protection: baseProtection, settings: baseSettings, wantDecision: OptInWithStrategy(config.ReviewerAutoMergeStrategySquash)},
		{name: "label only", pr: PRSnapshot{Labels: []string{"looper:worker-ready"}}, cfg: baseConfig, protection: baseProtection, settings: baseSettings, wantDecision: RefuseWithReason(RefusalReasonScope)},
		{name: "tracked issue only", pr: PRSnapshot{HasTrackedIssueLink: true}, cfg: baseConfig, protection: baseProtection, settings: baseSettings, wantDecision: RefuseWithReason(RefusalReasonScope)},
		{name: "no branch protection", pr: basePR, cfg: baseConfig, protection: BranchProtectionSnapshot{}, settings: baseSettings, wantDecision: RefuseWithReason(RefusalReasonNoBranchProtection)},
		{name: "strategy disallowed", pr: basePR, cfg: config.ReviewerAutoMergeConfig{Enabled: true, Strategy: config.ReviewerAutoMergeStrategyRebase, RequireBranchProtection: true, TransientRetries: 3, Scope: config.ReviewerAutoMergeScopeLooperOnly}, protection: baseProtection, settings: RepoSettingsSnapshot{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: false, AllowAutoMerge: true}, wantDecision: RefuseWithReason(RefusalReasonStrategyDisallowed)},
		{name: "auto merge disabled", pr: basePR, cfg: baseConfig, protection: baseProtection, settings: RepoSettingsSnapshot{AllowSquashMerge: true, AllowMergeCommit: true, AllowRebaseMerge: true, AllowAutoMerge: false}, wantDecision: RefuseWithReason(RefusalReasonAutoMergeDisabled)},
		{name: "feature disabled", pr: basePR, cfg: config.ReviewerAutoMergeConfig{Enabled: false, Strategy: config.ReviewerAutoMergeStrategySquash, RequireBranchProtection: true, TransientRetries: 3, Scope: config.ReviewerAutoMergeScopeLooperOnly}, protection: baseProtection, settings: baseSettings, wantDecision: RefuseWithReason(RefusalReasonDisabled)},
		{name: "branch protection waived", pr: basePR, cfg: config.ReviewerAutoMergeConfig{Enabled: true, Strategy: config.ReviewerAutoMergeStrategySquash, RequireBranchProtection: false, TransientRetries: 3, Scope: config.ReviewerAutoMergeScopeLooperOnly}, protection: BranchProtectionSnapshot{}, settings: baseSettings, wantDecision: OptInWithStrategy(config.ReviewerAutoMergeStrategySquash)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tt.pr, tt.cfg, tt.protection, tt.settings)
			if got != tt.wantDecision {
				t.Fatalf("Decide() = %#v, want %#v", got, tt.wantDecision)
			}
		})
	}
}
