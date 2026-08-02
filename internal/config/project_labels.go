package config

import (
	"encoding/json"
	"strings"

	"github.com/MumuTW/looper/internal/labels"
)

// ProjectLabelNamespace resolves the forge-label authority for a project.
// Configuration owns the project selection; labels owns validation and the
// default. Keeping the lookup here prevents each Role from inventing a
// slightly different fallback.
func ProjectLabelNamespace(cfg *Config, projectID string) labels.Namespace {
	namespace, configured := configuredProjectLabelNamespace(cfg, projectID)
	if configured {
		return namespace
	}
	return labels.DefaultNamespace()
}

// ProjectLabelNamespaceForMetadata resolves API-managed projects as well as
// config-managed projects. The catalog persists project metadata so a runtime
// project that is not present in the current config still retains its label
// authority.
func ProjectLabelNamespaceForMetadata(cfg *Config, projectID string, metadataJSON *string) labels.Namespace {
	namespace, configured := configuredProjectLabelNamespace(cfg, projectID)
	if configured {
		return namespace
	}
	if metadataJSON != nil {
		var metadata map[string]any
		if json.Unmarshal([]byte(*metadataJSON), &metadata) == nil {
			if raw, ok := metadata["labelNamespace"].(string); ok {
				return labels.NewNamespace(raw)
			}
		}
	}
	return labels.DefaultNamespace()
}

// ProjectClassificationLabels reports the explicit project opt-in for
// projecting kind/area/complexity labels. The default namespace keeps its
// historical projection through triage's default policy; custom namespaces
// must opt in here so isolation does not silently change classification scope.
func ProjectClassificationLabels(cfg *Config, projectID string) bool {
	if cfg == nil {
		return false
	}
	for _, project := range cfg.Projects {
		if strings.TrimSpace(project.ID) == strings.TrimSpace(projectID) {
			return project.ClassificationLabels
		}
	}
	return false
}

func configuredProjectLabelNamespace(cfg *Config, projectID string) (labels.Namespace, bool) {
	if cfg != nil {
		for _, project := range cfg.Projects {
			if strings.TrimSpace(project.ID) == strings.TrimSpace(projectID) {
				return labels.NewNamespace(project.LabelNamespace), true
			}
		}
	}
	return labels.DefaultNamespace(), false
}
