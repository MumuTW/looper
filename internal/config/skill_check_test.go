package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillCheckRejectsAmbiguousDefaultConfigs(t *testing.T) {
	home := t.TempDir()
	looperHome := filepath.Join(home, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.toml", "config.yaml"} {
		if err := os.WriteFile(filepath.Join(looperHome, name), []byte("test\n"), 0o600); err != nil {
			t.Fatal(err)
		}

	}
	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "skills", "looper", "scripts", "check.sh"))
	cmd.Env = append(os.Environ(), "HOME="+home, "LOOPER_CONFIG=")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check.sh succeeded with ambiguous defaults:\n%s", output)
	}
	text := string(output)
	if !strings.Contains(text, "multiple default config files found") || strings.Contains(text, "ok: config exists") {
		t.Fatalf("check.sh output = %q, want ambiguity without usable-config claim", text)
	}
}

func TestSkillCheckPrefersCanonicalTOMLAlongsideLegacyJSON(t *testing.T) {
	home := t.TempDir()
	looperHome := filepath.Join(home, ".looper")
	if err := os.MkdirAll(looperHome, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.toml", "config.json"} {
		if err := os.WriteFile(filepath.Join(looperHome, name), []byte("test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "skills", "looper", "scripts", "check.sh"))
	cmd.Env = append(os.Environ(), "HOME="+home, "LOOPER_CONFIG=")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check.sh error = %v:\n%s", err, output)
	}
	want := "ok: config exists at " + filepath.Join(looperHome, "config.toml")
	if !strings.Contains(string(output), want) {
		t.Fatalf("check.sh output = %q, missing %q", output, want)
	}
}
