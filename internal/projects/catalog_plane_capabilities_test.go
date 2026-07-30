package projects

import (
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func TestMaterializeCatalogValidatesPlaneRoleCapabilities(t *testing.T) {
	t.Parallel()

	metadata := `{"provider":"plane-main","repo":"acme/code","source":"api"}`
	record := storage.ProjectRecord{ID: "plane-api", MetadataJSON: &metadata}
	tests := []struct {
		name   string
		mutate func(*config.Config)
		path   string
	}{
		{
			name: "coordinator",
			mutate: func(cfg *config.Config) {
				cfg.Roles.Coordinator.Enabled = true
			},
			path: "roles.coordinator.enabled",
		},
		{
			name: "summary comment publishing",
			mutate: func(cfg *config.Config) {
				cfg.Roles.Reviewer.Behavior.PublishMode = config.ReviewerPublishModeSummaryComment
			},
			path: "roles.reviewer.behavior.publishMode",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			global, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatalf("DefaultConfig() error = %v", err)
			}
			global.Providers = []config.ProviderConfig{{ID: "plane-main", Kind: config.ProviderKindPlane}}
			tt.mutate(&global)

			_, err = MaterializeCatalog(global, []storage.ProjectRecord{record})
			if err == nil || !strings.Contains(err.Error(), `projects["plane-api"].`+tt.path) {
				t.Fatalf("MaterializeCatalog() error = %v, want Plane capability error at %s", err, tt.path)
			}
		})
	}
}
