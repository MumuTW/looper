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

// gatekeeperDiffBudgetForProject resolves only the small project override used
// by the Gatekeeper lane, avoiding a full ProjectRoleConfigs registry clone on
// every pull request.
//
// Project IDs are matched by exact equality, matching runtimeProjectBinding and
// config.ProjectRoleConfigs: a case-insensitive or trimmed comparison can select
// the wrong project's budget when two configured IDs differ only by case or
// surrounding whitespace, letting Gatekeeper apply limits that belong to another
// project.
func gatekeeperDiffBudgetForProject(cfg config.Config, projectID string) config.GatekeeperDiffBudget {
	var budget config.GatekeeperDiffBudget
	if cfg.Roles.Gatekeeper.DiffBudget != nil {
		budget = *cfg.Roles.Gatekeeper.DiffBudget
	}
	for _, project := range cfg.Projects {
		if project.ID != projectID {
			continue
		}
		if project.Roles != nil && project.Roles.Gatekeeper != nil && project.Roles.Gatekeeper.DiffBudget != nil {
			if project.Roles.Gatekeeper.DiffBudget.MaxChangedFiles != nil {
				budget.MaxChangedFiles = *project.Roles.Gatekeeper.DiffBudget.MaxChangedFiles
			}
			if project.Roles.Gatekeeper.DiffBudget.MaxDeletions != nil {
				budget.MaxDeletions = *project.Roles.Gatekeeper.DiffBudget.MaxDeletions
			}
		}
		break
	}
	return budget
}
