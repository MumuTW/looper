package config

// RedactProjectSecrets copies projects with their deploy credentials and
// parse-only compatibility fields removed, leaving its input untouched.
//
// roles.deployer.environment holds the values a deploy authenticates with — the
// same class of secret as daemon.environment. Every payload that leaves the
// daemon carrying projects must pass through here.
//
// The copy has to reach through Roles. A slice copy shares its backing array, so
// clearing Environment through the copy erases the operator's real credentials
// rather than withholding them from one payload, and every deploy afterwards
// fails for want of values that are still in the config file.
//
// A project with nothing to redact is passed through untouched rather than
// copied into a subtly different shape.
func RedactProjectSecrets(projects []ProjectRefConfig) []ProjectRefConfig {
	redacted := append([]ProjectRefConfig{}, projects...)
	for i := range redacted {
		roles := redacted[i].Roles
		if roles == nil {
			continue
		}
		clonedRoles := *roles
		changed := false
		if roles.Deployer != nil && roles.Deployer.Environment != nil {
			deployer := *roles.Deployer
			deployer.Environment = nil
			clonedRoles.Deployer = &deployer
			changed = true
		}
		if roles.Reviewer != nil && roles.Reviewer.AutoMerge != nil {
			reviewer := *roles.Reviewer
			reviewer.AutoMerge = nil
			clonedRoles.Reviewer = &reviewer
			changed = true
		}
		if !changed {
			continue
		}
		redacted[i].Roles = &clonedRoles
	}
	return redacted
}
