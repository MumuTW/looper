package projects

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer/automerge"
)

type GetRepositorySettingsFunc func(context.Context, githubinfra.RepositorySettingsInput) (githubinfra.RepositorySettings, error)
type GetBranchProtectionFunc func(context.Context, githubinfra.BranchProtectionInput) (githubinfra.BranchProtection, error)

func (s *Service) validateReviewerAutoMergeForProject(ctx context.Context, projectID string, repo *string, baseBranch string, cfg config.Config) error {
	roles := config.ProjectRoleConfigs(cfg, projectID)
	autoMergeCfg := roles.Reviewer.AutoMerge
	if !autoMergeCfg.Enabled {
		return nil
	}
	if autoMergeCfg.Scope != config.ReviewerAutoMergeScopeLooperOnly {
		return reviewerAutoMergeValidationError(projectRepoLabel(repo, projectID), fmt.Sprintf("scope %q is unsupported in v1", autoMergeCfg.Scope))
	}
	if s.GetRepositorySettings == nil || (autoMergeCfg.RequireBranchProtection && s.GetBranchProtection == nil) {
		return reviewerAutoMergeValidationError(projectRepoLabel(repo, projectID), "GitHub auto-merge validation is not configured")
	}
	repoName := strings.TrimSpace(stringValue(repo))
	if repoName == "" {
		return reviewerAutoMergeValidationError(projectID, "GitHub repo is unknown")
	}
	settings, err := s.GetRepositorySettings(ctx, githubinfra.RepositorySettingsInput{Repo: repoName})
	if err != nil {
		return fmt.Errorf("read repo settings for %s: %w", repoName, err)
	}
	if !automerge.StrategyAllowed(autoMergeCfg.Strategy, automerge.RepoSettingsSnapshot{
		AllowSquashMerge: settings.AllowSquashMerge,
		AllowMergeCommit: settings.AllowMergeCommit,
		AllowRebaseMerge: settings.AllowRebaseMerge,
		AllowAutoMerge:   settings.AllowAutoMerge,
	}) {
		return reviewerAutoMergeValidationError(repoName, fmt.Sprintf("configured strategy %q is not allowed by repo settings", autoMergeCfg.Strategy))
	}
	if !settings.AllowAutoMerge {
		return reviewerAutoMergeValidationError(repoName, "GitHub repo auto-merge is disabled")
	}
	if autoMergeCfg.RequireBranchProtection {
		branch := strings.TrimSpace(baseBranch)
		if branch == "" {
			branch = strings.TrimSpace(cfg.Defaults.BaseBranch)
		}
		if branch == "" {
			return reviewerAutoMergeValidationError(repoName, "default branch is unknown")
		}
		protection, err := s.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: repoName, Branch: branch})
		if err != nil {
			return fmt.Errorf("read branch protection for %s@%s: %w", repoName, branch, err)
		}
		if !protection.Enabled || !protection.HasRequiredChecks {
			return reviewerAutoMergeValidationError(repoName, "default branch protection is missing or has no required checks")
		}
	}
	return nil
}

func reviewerAutoMergeValidationError(repo, failure string) error {
	return ProjectValidationError{Message: fmt.Sprintf("reviewer auto-merge enabled on %s but %s; disable roles.reviewer.autoMerge.enabled or fix repo settings", repo, failure)}
}

func projectRepoLabel(repo *string, projectID string) string {
	if trimmed := strings.TrimSpace(stringValue(repo)); trimmed != "" {
		return trimmed
	}
	return projectID
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
