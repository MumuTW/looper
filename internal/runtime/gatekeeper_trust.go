package runtime

import (
	"strings"

	"github.com/MumuTW/looper/internal/config"
)

// gatekeeperTrustForProject resolves a project's merge-authority level.
//
// Like the deployer lane, it avoids config.ProjectRoleConfigs: that clones the
// whole coding-role registry to apply overrides, and this runs per pull request
// per tick to read one enum.
//
// Project IDs are matched by exact equality, the same semantics as
// runtimeProjectBinding and config.ProjectRoleConfigs: a looser case-insensitive
// or trimmed comparison can stop on the wrong project when two configured IDs
// differ only by case or surrounding whitespace, applying the wrong override.
func gatekeeperTrustForProject(cfg config.Config, projectID string) config.GatekeeperTrustLevel {
	trust := cfg.Roles.Gatekeeper.Trust
	for _, project := range cfg.Projects {
		if project.ID != projectID {
			continue
		}
		if project.Roles != nil && project.Roles.Gatekeeper != nil && project.Roles.Gatekeeper.Trust != nil {
			trust = *project.Roles.Gatekeeper.Trust
		}
		break
	}
	if strings.TrimSpace(string(trust)) == "" {
		return config.GatekeeperTrustObserve
	}
	return config.GatekeeperTrustLevel(strings.ToLower(strings.TrimSpace(string(trust))))
}

func gatekeeperRequiredReviewChangedLinesForProject(cfg config.Config, projectID string) int {
	threshold := cfg.Roles.Gatekeeper.RequiredReviewChangedLines
	for _, project := range cfg.Projects {
		if !strings.EqualFold(strings.TrimSpace(project.ID), strings.TrimSpace(projectID)) {
			continue
		}
		if project.Roles != nil && project.Roles.Gatekeeper != nil && project.Roles.Gatekeeper.RequiredReviewChangedLines != nil {
			threshold = *project.Roles.Gatekeeper.RequiredReviewChangedLines
		}
		break
	}
	return threshold
}
