package projects

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestServiceUpdateProjectPromotesGatekeeperTrustInCatalogMetadata(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	nowISO := currentISO(func() time.Time { return now })
	metadata := `{"repo":"acme/looper","source":"api"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: stringPointer("main"),
		MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	catalog := NewCatalog(cfg)
	service := &Service{
		DB: coordinator.DB(), Repos: repos, Config: cfg, ConfigSource: catalog,
		PublishProjects: func(projects []config.ProjectRefConfig) { catalog.Publish(projects) },
		Now:             func() time.Time { return now }, ScheduleDiscovery: func(func()) {},
	}

	updated, err := service.UpdateProject(context.Background(), "looper", UpdateInput{
		GatekeeperTrust: UpdateStringField{Set: true, Value: stringPointer(string(config.GatekeeperTrustAdvise))},
	})
	if err != nil {
		t.Fatalf("UpdateProject() error = %v", err)
	}
	if updated.ID != "looper" {
		t.Fatalf("updated project = %#v, want looper", updated)
	}
	var storedMetadata map[string]any
	if updated.MetadataJSON == nil || json.Unmarshal([]byte(*updated.MetadataJSON), &storedMetadata) != nil {
		t.Fatalf("updated metadata = %v, want valid JSON", updated.MetadataJSON)
	}
	roles, ok := storedMetadata["roles"].(map[string]any)
	if !ok {
		t.Fatalf("roles metadata = %#v, want object", storedMetadata["roles"])
	}
	gatekeeper, ok := roles["gatekeeper"].(map[string]any)
	if !ok || gatekeeper["trust"] != string(config.GatekeeperTrustAdvise) {
		t.Fatalf("gatekeeper metadata = %#v, want advise", roles["gatekeeper"])
	}

	_, err = service.UpdateProject(context.Background(), "looper", UpdateInput{
		GatekeeperTrust: UpdateStringField{Set: true, Value: stringPointer(string(config.GatekeeperTrustAdvise))},
	})
	if err == nil || !strings.Contains(err.Error(), "must advance") {
		t.Fatalf("repeat promotion error = %v, want monotonic promotion rejection", err)
	}
}

func TestServiceUpdateProjectRejectsGatekeeperPromotionForConfigManagedProject(t *testing.T) {
	coordinator := openCoordinator(t)
	repos := storage.NewRepositories(coordinator.DB())
	metadata := `{"repo":"acme/looper","source":"config"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "looper", Name: "Looper", RepoPath: "/tmp/looper", BaseBranch: stringPointer("main"), MetadataJSON: &metadata,
		CreatedAt: "2026-07-31T12:00:00.000Z", UpdatedAt: "2026-07-31T12:00:00.000Z",
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	service := &Service{DB: coordinator.DB(), Repos: repos, Config: config.Config{}}
	_, err := service.UpdateProject(context.Background(), "looper", UpdateInput{
		GatekeeperTrust: UpdateStringField{Set: true, Value: stringPointer(string(config.GatekeeperTrustAdvise))},
	})
	if err == nil || !strings.Contains(err.Error(), "managed by config") {
		t.Fatalf("config-managed promotion error = %v, want explicit config authority message", err)
	}
}
