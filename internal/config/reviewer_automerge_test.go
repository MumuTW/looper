package config

import "testing"

func TestDefaultConfigReviewerAutoMergeDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	autoMerge := cfg.Roles.Reviewer.AutoMerge
	if autoMerge.Enabled {
		t.Fatal("DefaultConfig().Roles.Reviewer.AutoMerge.Enabled = true, want false")
	}
	if autoMerge.Strategy != ReviewerAutoMergeStrategySquash {
		t.Fatalf("DefaultConfig().Roles.Reviewer.AutoMerge.Strategy = %q, want %q", autoMerge.Strategy, ReviewerAutoMergeStrategySquash)
	}
	if !autoMerge.RequireBranchProtection {
		t.Fatal("DefaultConfig().Roles.Reviewer.AutoMerge.RequireBranchProtection = false, want true")
	}
	if autoMerge.TransientRetries != 3 {
		t.Fatalf("DefaultConfig().Roles.Reviewer.AutoMerge.TransientRetries = %d, want 3", autoMerge.TransientRetries)
	}
	if autoMerge.Scope != ReviewerAutoMergeScopeLooperOnly {
		t.Fatalf("DefaultConfig().Roles.Reviewer.AutoMerge.Scope = %q, want %q", autoMerge.Scope, ReviewerAutoMergeScopeLooperOnly)
	}
}

func TestProjectRoleConfigsMergesReviewerAutoMergeOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	cfg.Projects = []ProjectRefConfig{{
		ID:       "demo",
		Name:     "Demo",
		RepoPath: "/tmp/demo",
		Roles: &PartialRoleConfigs{Reviewer: &PartialReviewerRoleConfig{AutoMerge: &PartialReviewerAutoMergeConfig{
			Enabled:                 reviewerAutoMergeBoolPtr(true),
			Strategy:                reviewerAutoMergeStrategyPtr(ReviewerAutoMergeStrategyMerge),
			RequireBranchProtection: reviewerAutoMergeBoolPtr(false),
			TransientRetries:        reviewerAutoMergeIntPtr(5),
			Scope:                   reviewerAutoMergeScopePtr(ReviewerAutoMergeScopeLooperOnly),
		}}},
	}}

	roles := ProjectRoleConfigs(cfg, "demo")
	if !roles.Reviewer.AutoMerge.Enabled || roles.Reviewer.AutoMerge.Strategy != ReviewerAutoMergeStrategyMerge || roles.Reviewer.AutoMerge.RequireBranchProtection || roles.Reviewer.AutoMerge.TransientRetries != 5 || roles.Reviewer.AutoMerge.Scope != ReviewerAutoMergeScopeLooperOnly {
		t.Fatalf("ProjectRoleConfigs() reviewer auto-merge = %#v, want project override applied", roles.Reviewer.AutoMerge)
	}
}

func reviewerAutoMergeStrategyPtr(value ReviewerAutoMergeStrategy) *ReviewerAutoMergeStrategy {
	return &value
}
func reviewerAutoMergeScopePtr(value ReviewerAutoMergeScope) *ReviewerAutoMergeScope { return &value }
func reviewerAutoMergeBoolPtr(value bool) *bool                                      { return &value }
func reviewerAutoMergeIntPtr(value int) *int                                         { return &value }
