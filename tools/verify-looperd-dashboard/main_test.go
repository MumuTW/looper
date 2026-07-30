package main

import (
	"strings"
	"testing"
)

func TestProductionScriptSourceRejectsFallbackAndMissingBundle(t *testing.T) {
	t.Parallel()

	if _, err := productionScriptSource([]byte(`<p>Production dashboard assets are not embedded in this binary.</p>`)); err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("fallback error = %v, want explicit rejection", err)
	}
	if _, err := productionScriptSource([]byte(`<!doctype html><div id="root"></div>`)); err == nil || !strings.Contains(err.Error(), "no production JavaScript") {
		t.Fatalf("missing-bundle error = %v, want explicit rejection", err)
	}
	got, err := productionScriptSource([]byte(`<!doctype html><script type="module" src="/dashboard/assets/index-abc12345.js"></script>`))
	if err != nil || got != "/dashboard/assets/index-abc12345.js" {
		t.Fatalf("productionScriptSource() = (%q, %v)", got, err)
	}
}

func TestIsolatedEnvironmentDropsAmbientLooperConfiguration(t *testing.T) {
	t.Setenv("LOOPER_PORT", "1")
	t.Setenv("LOOPER_REQUIRE_TRUSTED_SRT", "1")
	t.Setenv("UNRELATED_VALUE", "kept")

	environment := isolatedEnvironment("/tmp/state", "/tmp/state/config.toml")
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if got := values["LOOPER_HOME"]; got != "/tmp/state" {
		t.Fatalf("LOOPER_HOME = %q", got)
	}
	if got := values["LOOPER_CONFIG"]; got != "/tmp/state/config.toml" {
		t.Fatalf("LOOPER_CONFIG = %q", got)
	}
	if _, ok := values["LOOPER_PORT"]; ok {
		t.Fatal("ambient LOOPER_PORT survived isolation")
	}
	if _, ok := values["LOOPER_REQUIRE_TRUSTED_SRT"]; ok {
		t.Fatal("ambient LOOPER_REQUIRE_TRUSTED_SRT survived isolation")
	}
	if got := values["UNRELATED_VALUE"]; got != "kept" {
		t.Fatalf("UNRELATED_VALUE = %q", got)
	}
}

func TestRequireOKEnvelope(t *testing.T) {
	t.Parallel()
	if err := requireOKEnvelope("status", []byte(`{"ok":true,"data":{}}`)); err != nil {
		t.Fatalf("requireOKEnvelope(valid) error = %v", err)
	}
	if err := requireOKEnvelope("status", []byte(`{"ok":false}`)); err == nil || !strings.Contains(err.Error(), "not an ok envelope") {
		t.Fatalf("requireOKEnvelope(false) error = %v", err)
	}
	if err := requireOKEnvelope("status", []byte(`not-json`)); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("requireOKEnvelope(invalid) error = %v", err)
	}
}
