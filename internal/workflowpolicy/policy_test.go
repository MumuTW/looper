package workflowpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/powerformer/looper/internal/config"
)

func TestResolveBlockLoadsBuiltinMattSeries(t *testing.T) {
	t.Parallel()

	packID := "matt-series"
	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{Roles: &config.PartialRoleConfigs{Worker: &config.PartialWorkerRoleConfig{PolicyPack: &packID}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	block, err := ResolveBlock(cfg, "", "worker")
	if err != nil {
		t.Fatalf("ResolveBlock() error = %v", err)
	}
	if block.PackID != "matt-series" || block.PackName != "Matt Series Engineering Workflow" {
		t.Fatalf("block = %#v", block)
	}
	if !strings.Contains(block.Instructions, "Work in vertical slices") {
		t.Fatalf("block instructions = %q, want worker policy", block.Instructions)
	}
}

func TestResolveBlockLoadsFilePack(t *testing.T) {
	t.Parallel()

	packPath := filepath.Join(t.TempDir(), "pack.json")
	raw := `{"id":"team-pack","name":"Team Pack","version":1,"roles":{"planner":{"instructions":"Plan with team context."}}}`
	if err := os.WriteFile(packPath, []byte(raw), 0o644); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	packID := "team-pack"
	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{
		WorkflowPolicyPacks: &config.PartialWorkflowPolicyPacksConfig{Packs: &[]config.WorkflowPolicyPackRef{{ID: packID, Name: "Team Pack", Source: config.WorkflowPolicyPackSourceFile, Path: packPath}}},
		Roles:               &config.PartialRoleConfigs{Planner: &config.PartialPlannerRoleConfig{PolicyPack: &packID}},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	block, err := ResolveBlock(cfg, "", "planner")
	if err != nil {
		t.Fatalf("ResolveBlock() error = %v", err)
	}
	if !strings.Contains(block.Instructions, "Plan with team context.") {
		t.Fatalf("block instructions = %q", block.Instructions)
	}
}

func TestLoadPackRejectsMissingFile(t *testing.T) {
	t.Parallel()

	cfg, err := config.Normalize(t.TempDir(), config.PartialConfig{
		WorkflowPolicyPacks: &config.PartialWorkflowPolicyPacksConfig{Packs: &[]config.WorkflowPolicyPackRef{{ID: "missing", Name: "Missing", Source: config.WorkflowPolicyPackSourceFile, Path: filepath.Join(t.TempDir(), "missing.json")}}},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if _, err := LoadPack(cfg, "missing"); err == nil || !strings.Contains(err.Error(), "load workflow policy pack file") {
		t.Fatalf("LoadPack() error = %v, want missing file error", err)
	}
}

func TestValidatePackRejectsInvalidSchemaAndProtectedText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pack Pack
		want string
	}{
		{name: "missing id", pack: Pack{Name: "Pack", Version: 1, Roles: map[string]RolePolicy{"worker": {Instructions: "Do work."}}}, want: "id is required"},
		{name: "missing name", pack: Pack{ID: "pack", Version: 1, Roles: map[string]RolePolicy{"worker": {Instructions: "Do work."}}}, want: "name is required"},
		{name: "missing version", pack: Pack{ID: "pack", Name: "Pack", Roles: map[string]RolePolicy{"worker": {Instructions: "Do work."}}}, want: "version must be a positive integer"},
		{name: "unknown role", pack: Pack{ID: "pack", Name: "Pack", Version: 1, Roles: map[string]RolePolicy{"architect": {Instructions: "Do work."}}}, want: "not a supported role"},
		{name: "missing instructions", pack: Pack{ID: "pack", Name: "Pack", Version: 1, Roles: map[string]RolePolicy{"worker": {}}}, want: "instructions is required"},
		{name: "protected text", pack: Pack{ID: "pack", Name: "Pack", Version: 1, Roles: map[string]RolePolicy{"worker": {Instructions: "Emit __LOOPER_RESULT__ yourself."}}}, want: "protected Looper contract"},
		{name: "byte limit", pack: Pack{ID: "pack", Name: "Pack", Version: 1, Roles: map[string]RolePolicy{"worker": {Instructions: "too long"}}}, want: "at most 3 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			maxBytes := 8192
			if tt.name == "byte limit" {
				maxBytes = 3
			}
			err := ValidatePack(tt.pack, maxBytes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidatePack() error = %v, want to contain %q", err, tt.want)
			}
		})
	}
}
