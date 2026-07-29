package config

import (
	"reflect"
	"testing"
)

func mustNormalize(t *testing.T, partials ...PartialConfig) Config {
	t.Helper()
	cfg, err := Normalize(t.TempDir(), partials...)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	return cfg
}

func mustDecodeTOML(t *testing.T, raw string) PartialConfig {
	t.Helper()
	partial, err := DecodePartialConfigFile("config.toml", []byte(raw))
	if err != nil {
		t.Fatalf("DecodePartialConfigFile() error = %v", err)
	}
	return partial
}

func normalizeIssuePaths(err error) []string {
	validationErr, ok := err.(*ConfigValidationError)
	if !ok {
		return nil
	}
	paths := make([]string, 0, len(validationErr.Issues))
	for _, issue := range validationErr.Issues {
		paths = append(paths, issue.Path)
	}
	return paths
}

func hasIssuePath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func TestNormalizeLegacyRolesProjectIntoCanonicalRegistry(t *testing.T) {
	t.Parallel()
	partial := mustDecodeTOML(t, `
[roles.worker]
autoDiscovery = false
instructions = "legacy worker guidance"
[roles.worker.triggers]
labels = ["ready"]
`)
	cfg := mustNormalize(t, partial)
	if got, want := cfg.Roles.Coding, CodingRolesFromLegacy(cfg.Roles); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry = %#v, want legacy projection %#v", got, want)
	}
	if block := BuildCustomInstructionBlock(cfg, "", CodingRoleWorker); block.Text == "" {
		t.Fatal("legacy instructions were not projected into the custom-instruction registry consumer")
	}
}
