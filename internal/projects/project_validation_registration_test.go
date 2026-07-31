package projects

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestDaemonOwnedProjectRegistrationRequiresValidationPolicy(t *testing.T) {
	t.Parallel()

	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "looper.sqlite"), storage.SQLiteCoordinatorOptions{})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}

	cfg, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	vendor := config.AgentVendorCodex
	cfg.Agent.Vendor = &vendor
	catalog := NewCatalog(cfg)
	service := &Service{
		Repos:           storage.NewRepositories(coordinator.DB()),
		ConfigSource:    catalog,
		PublishProjects: catalog.Publish,
	}

	_, err = service.AddProject(context.Background(), AddInput{ID: "missing", Name: "Missing", RepoPath: "/repos/missing", BaseBranch: "main"})
	if err == nil || !strings.Contains(err.Error(), "validation") {
		t.Fatalf("AddProject(missing policy) error = %v, want fail-closed validation error", err)
	}

	want := []string{"go test ./...", "go build ./..."}
	result, err := service.AddProject(context.Background(), AddInput{
		ID:         "looper",
		Name:       "Looper",
		RepoPath:   "/repos/looper",
		BaseBranch: "main",
		Validation: &config.ProjectValidationConfig{Commands: want},
	})
	if err != nil {
		t.Fatalf("AddProject(configured policy) error = %v", err)
	}
	materialized, found := catalog.View().Project(result.Project.ID)
	if !found || materialized.Project.Validation == nil || !reflect.DeepEqual(materialized.Project.Validation.Commands, want) {
		t.Fatalf("materialized project validation = %#v, want %#v", materialized.Project.Validation, want)
	}
}
