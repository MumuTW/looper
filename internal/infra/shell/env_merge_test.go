package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Options.Env used to replace the child environment outright, so injecting a
// single token would strip PATH and HOME from the child. Assert it layers.
func TestRunEnvOverridesLayerOnInheritedEnvironment(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "print-env.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("LOOPER_INHERITED_MARKER", "inherited-value")
	t.Setenv("LOOPER_OVERRIDDEN_MARKER", "stale-value")

	result, err := Run(context.Background(), Options{
		Command: script,
		Env:     map[string]string{"LOOPER_OVERRIDDEN_MARKER": "fresh-value"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v (stderr=%s)", err, result.Stderr)
	}

	lines := map[string]string{}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if name, value, found := strings.Cut(strings.TrimRight(line, "\r"), "="); found {
			lines[name] = value
		}
	}

	if lines["LOOPER_INHERITED_MARKER"] != "inherited-value" {
		t.Errorf("inherited variable = %q, want it preserved", lines["LOOPER_INHERITED_MARKER"])
	}
	if lines["LOOPER_OVERRIDDEN_MARKER"] != "fresh-value" {
		t.Errorf("overridden variable = %q, want fresh-value", lines["LOOPER_OVERRIDDEN_MARKER"])
	}
	if strings.TrimSpace(lines["PATH"]) == "" {
		t.Error("PATH missing from the child environment; an override must not replace it")
	}
}

func TestMergedEnvSliceReplacesOnlyByName(t *testing.T) {
	merged := mergedEnvSlice([]string{"KEEP=1", "REPLACE=old", "MALFORMED"}, map[string]string{"REPLACE": "new", "ADD": "2"})

	got := strings.Join(merged, " ")
	for _, want := range []string{"KEEP=1", "REPLACE=new", "ADD=2", "MALFORMED"} {
		if !strings.Contains(got, want) {
			t.Errorf("mergedEnvSlice() = %v, missing %s", merged, want)
		}
	}
	if strings.Contains(got, "REPLACE=old") {
		t.Errorf("mergedEnvSlice() = %v, still carries the replaced value", merged)
	}
}
