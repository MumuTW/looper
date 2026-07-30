package agent

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

func TestRuntimeAdapterRegistryCoversEveryConfigurableVendor(t *testing.T) {
	t.Parallel()

	for _, vendor := range config.ConfigurableAgentVendors() {
		adapter, ok := runtimeAdapterFor(vendor)
		if !ok {
			t.Errorf("vendor %q has no runtime adapter", vendor)
			continue
		}
		if adapter.command == "" {
			t.Errorf("vendor %q adapter has no command", vendor)
		}
		if adapter.resolveStartArgs == nil {
			t.Errorf("vendor %q adapter has no fresh-run argument builder", vendor)
		}
	}
}
