package config

import "testing"

func TestNormalizeProjectPersonalProject(t *testing.T) {
	enabled := true
	cfg, err := Normalize(t.TempDir(), PartialConfig{Projects: &[]PartialProjectRefConfig{{ID: "personal", Name: "Personal", RepoPath: "/repo", PersonalProject: &enabled}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if !cfg.Projects[0].PersonalProject || !IsPersonalProject(cfg, "personal") {
		t.Fatalf("personal project = %#v, want enabled", cfg.Projects[0])
	}
	if IsPersonalProject(cfg, "missing") {
		t.Fatal("unknown project unexpectedly treated as personal")
	}
}

func TestProjectPersonalProjectDefaultsOff(t *testing.T) {
	cfg, err := Normalize(t.TempDir(), PartialConfig{Projects: &[]PartialProjectRefConfig{{ID: "shared", Name: "Shared", RepoPath: "/repo"}}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if cfg.Projects[0].PersonalProject || IsPersonalProject(cfg, "shared") {
		t.Fatal("personal project defaulted on")
	}
}
