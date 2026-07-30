package config

import "testing"

// Enabling the Reproducer changes what "done" means for every bug in a project,
// so absent configuration must leave it off — matching hitl.enabled.
func TestReproducerDefaultsToDisabled(t *testing.T) {
	t.Parallel()
	config, err := Normalize("", PartialConfig{})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if config.Reproducer.Enabled {
		t.Fatal("Reproducer.Enabled = true by default, want opt-in")
	}
}

func TestReproducerEnabledIsAuthorable(t *testing.T) {
	t.Parallel()
	enabled := true
	config, err := Normalize("", PartialConfig{Reproducer: &PartialReproducerConfig{Enabled: &enabled}})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if !config.Reproducer.Enabled {
		t.Fatal("Reproducer.Enabled = false after an explicit opt-in")
	}

	// A later partial that says nothing about the Role must not clear it.
	config, err = Normalize("", PartialConfig{Reproducer: &PartialReproducerConfig{Enabled: &enabled}}, PartialConfig{})
	if err != nil {
		t.Fatalf("second Normalize() error = %v", err)
	}
	if !config.Reproducer.Enabled {
		t.Fatal("Reproducer.Enabled was cleared by an unrelated partial")
	}
}
