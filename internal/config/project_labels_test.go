package config

import (
	"strings"
	"testing"
)

func TestProjectLabelNamespaceNormalizesConfiguredPrefix(t *testing.T) {
	prefix := "Team.Looper:"
	projects := []PartialProjectRefConfig{{ID: "demo", LabelNamespace: &prefix}}
	cfg, err := Normalize(t.TempDir(), PartialConfig{Projects: &projects})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if got, want := ProjectLabelNamespace(&cfg, "demo").Prefix, "team.looper:"; got != want {
		t.Fatalf("ProjectLabelNamespace() = %q, want %q", got, want)
	}
	if got := ProjectLabelNamespace(&cfg, "unknown").Prefix; got != "looper:" {
		t.Fatalf("unknown project namespace = %q, want default looper:", got)
	}
}

func TestProjectLabelNamespaceReadsPersistedProjectMetadata(t *testing.T) {
	metadata := `{"labelNamespace":"api.looper:"}`
	if got, want := ProjectLabelNamespaceForMetadata(nil, "api-project", &metadata).Prefix, "api.looper:"; got != want {
		t.Fatalf("metadata namespace = %q, want %q", got, want)
	}
}

func TestValidateRejectsUnsafeProjectLabelNamespace(t *testing.T) {
	prefix := "team looper:"
	projects := []PartialProjectRefConfig{{ID: "demo", LabelNamespace: &prefix}}
	cfg, err := Normalize(t.TempDir(), PartialConfig{Projects: &projects})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	err = ValidateWithOptions(cfg, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "labelNamespace") {
		t.Fatalf("ValidateWithOptions() error = %v, want labelNamespace validation", err)
	}
}
