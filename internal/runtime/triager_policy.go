package runtime

import (
	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/triager"
	"github.com/MumuTW/looper/internal/triager/admission"
)

func triagerProjectPolicy(cfg config.Config, projectID string) triager.ProjectPolicy {
	role := config.ProjectRoleConfigs(cfg, projectID).Triager
	overrides := make(map[admission.AuthorTier]admission.Outcome, len(role.AuthorTiers))
	for tier, outcome := range role.AuthorTiers {
		overrides[admission.AuthorTier(tier)] = admission.Outcome(outcome)
	}
	return triager.ProjectPolicy{
		Admission: admission.Policy{
			Preset: admission.Preset(role.Preset), Classify: role.Classify, Overrides: overrides,
		},
		Legacy: triager.LegacyPolicy{
			AutoRouteConfidence:         role.Legacy.AutoRouteConfidence,
			MaxAutoRouteRisk:            triager.Risk(role.Legacy.MaxAutoRouteRisk),
			RequireInScope:              role.Legacy.RequireInScope,
			RequireNoMissingInformation: role.Legacy.RequireNoMissingInformation,
			RequirePlanner:              role.Legacy.RequirePlanner,
			RequireRationale:            role.Legacy.RequireRationale,
		},
	}
}
