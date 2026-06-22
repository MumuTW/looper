package e2e

import (
	"strings"
	"testing"
)

func TestGitHubSandboxRepoEnvCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "preferred", env: map[string]string{envSandboxRepo: "acme/looper"}, want: "acme/looper"},
		{name: "legacy", env: map[string]string{envSandboxRepoLegacy: "legacy/looper"}, want: "legacy/looper"},
		{name: "same value", env: map[string]string{envSandboxRepo: "acme/looper", envSandboxRepoLegacy: "acme/looper"}, want: "acme/looper"},
		{name: "trims whitespace", env: map[string]string{envSandboxRepo: " acme/looper ", envSandboxRepoLegacy: "\tacme/looper\n"}, want: "acme/looper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGitHubSandboxRepoEnv(t, func(key string) string { return tc.env[key] })
			if got != tc.want {
				t.Fatalf("repo = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGitHubSandboxRepoEnvConflictFailsFast(t *testing.T) {
	env := map[string]string{envSandboxRepo: "acme/looper", envSandboxRepoLegacy: "other/looper"}
	_, err := parseGitHubSandboxRepoEnv(func(key string) string { return env[key] })
	if err == nil || !strings.Contains(err.Error(), envSandboxRepo) || !strings.Contains(err.Error(), envSandboxRepoLegacy) {
		t.Fatalf("err = %v, want conflict naming both env vars", err)
	}
}
