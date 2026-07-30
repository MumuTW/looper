package config

import (
	"strings"
	"testing"
)

func TestDevinIsAValidAgentVendor(t *testing.T) {
	t.Parallel()

	if !isValidAgentVendor(AgentVendorDevinExperimental) {
		t.Fatal("devin-experimental should be a valid configurable agent vendor")
	}

	invalid, err := DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}
	invalid.Daemon.LogDir = t.TempDir()
	invalid.Daemon.WorkingDirectory = t.TempDir()
	unknown := AgentVendor("unknown")
	invalid.Agent.Vendor = &unknown
	err = ValidateWithOptions(invalid, ValidateOptions{DefaultWorktreeRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), string(AgentVendorDevinExperimental)) {
		t.Fatalf("invalid vendor error = %v, want configurable-vendor copy to include devin-experimental", err)
	}
}
