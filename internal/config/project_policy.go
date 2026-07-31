package config

// IsPersonalProject is the explicit project-level authority for self-authored
// issue admission. Repository ownership, issue author names, and agent output
// are not enough to infer this policy safely.
func IsPersonalProject(cfg Config, projectID string) bool {
	for _, project := range cfg.Projects {
		if project.ID == projectID {
			return project.PersonalProject
		}
	}
	return false
}
