package github

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

// StandardLooperLabels drives label provisioning, and DefaultConfig decides
// which labels a default installation then waits for. Nothing tied the two
// together, so looper:worker-ready was configured as the Worker trigger and
// never provisioned: a freshly initialized repository could not have the label
// applied, and default Worker discovery never fired.
//
// The failure is silent in both directions — provisioning reports success for
// the labels it does know, and the Worker just sees no work — so pin the
// relationship rather than relying on the two lists being edited together.
func TestDefaultTriggerLabelsAreProvisioned(t *testing.T) {
	t.Parallel()

	defaults, err := config.DefaultConfig(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultConfig() error = %v", err)
	}

	provisioned := map[string]struct{}{}
	for _, definition := range StandardLooperLabels() {
		provisioned[definition.Name] = struct{}{}
	}

	required := map[string][]string{
		"roles.planner.triggers.labels":                   defaults.Roles.Planner.Triggers.Labels,
		"roles.worker.triggers.labels":                    defaults.Roles.Worker.Triggers.Labels,
		"roles.coordinator.dispatch.autonomous.holdLabel": {defaults.Roles.Coordinator.Dispatch.Autonomous.HoldLabel},
	}

	for path, wanted := range required {
		for _, label := range wanted {
			if label == "" {
				continue
			}
			if _, ok := provisioned[label]; !ok {
				t.Errorf("%s defaults to %q, which StandardLooperLabels does not provision", path, label)
			}
		}
	}
}

// Every provisioned label should carry deliberate metadata. A label falling
// through to the default arm reads as intentional in the provisioning output
// while actually meaning "nobody described this one".
func TestProvisionedLabelsHaveDeliberateMetadata(t *testing.T) {
	t.Parallel()

	for _, definition := range StandardLooperLabels() {
		if definition.Color == "" || definition.Description == "" {
			t.Errorf("label %q has empty color or description", definition.Name)
		}
		if definition.Description == "Managed by looper" {
			t.Errorf("label %q falls through to the default description; give it one in resolveLabelDescription", definition.Name)
		}
	}
}
