package projects

import (
	"reflect"
	"testing"

	"github.com/MumuTW/looper/internal/config"
	"github.com/MumuTW/looper/internal/storage"
)

func TestProjectValidationPolicySurvivesCatalogPersistence(t *testing.T) {
	t.Parallel()

	want := &config.ProjectValidationConfig{Commands: []string{"scripts/verify.sh", "go test ./..."}}
	metadata, err := buildProjectMetadataJSON(nil, config.ProjectRefConfig{
		ID:         "looper",
		Validation: want,
	}, nil)
	if err != nil {
		t.Fatalf("buildProjectMetadataJSON() error = %v", err)
	}

	record := storage.ProjectRecord{
		ID:           "looper",
		Name:         "Looper",
		RepoPath:     "/repos/looper",
		MetadataJSON: &metadata,
	}
	projects, err := MaterializeCatalog(config.Config{}, []storage.ProjectRecord{record})
	if err != nil {
		t.Fatalf("MaterializeCatalog() error = %v", err)
	}
	if len(projects) != 1 || !reflect.DeepEqual(projects[0].Validation, want) {
		t.Fatalf("materialized validation = %#v, want %#v", projects, want)
	}

	projects[0].Validation.Commands[0] = "mutated"
	if want.Commands[0] != "scripts/verify.sh" {
		t.Fatal("materialized validation aliases the input policy")
	}
}
