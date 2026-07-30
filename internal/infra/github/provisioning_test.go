package github

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/labels"
)

// labels.Standard drives label provisioning, and DefaultConfig decides
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
	for _, definition := range labels.Standard() {
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
				t.Errorf("%s defaults to %q, which labels.Standard does not provision", path, label)
			}
		}
	}
}

// Every provisioned label should carry deliberate metadata. A label falling
// through to the default arm reads as intentional in the provisioning output
// while actually meaning "nobody described this one".
func TestProvisionedLabelsHaveDeliberateMetadata(t *testing.T) {
	t.Parallel()

	for _, definition := range labels.Standard() {
		if definition.Color == "" || definition.Description == "" {
			t.Errorf("label %q has empty color or description", definition.Name)
		}
	}
}

// Provisioning has the same list-then-create race as on-demand label apply.
// A duplicate create is evidence that the desired label landed, so the
// provisioning result must report it as skipped rather than as a failed run.
func TestInitializeLabelsToleratesLabelCreatedAfterList(t *testing.T) {
	t.Parallel()

	runner := &fakeGHRunner{t: t}
	runner.respond = func(options shell.Options) (shell.Result, error) {
		args := strings.Join(options.Args, " ")
		switch {
		case strings.HasPrefix(args, "label list "):
			return shell.Result{Stdout: `[]`}, nil
		case strings.HasPrefix(args, "label create "):
			return shell.Result{Stderr: "HTTP 422: Label already exists"}, &shell.CommandExecutionError{Message: "Command exited with code 1: HTTP 422: Label already exists", Category: shell.FailureNonZeroExit}
		default:
			t.Fatalf("unexpected gh args: %q", args)
			return shell.Result{}, nil
		}
	}

	result, err := New(Options{GHPath: "gh", CWD: t.TempDir(), GHRun: runner.run}).InitializeLabels(context.Background(), InitializeLabelsInput{Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("InitializeLabels() error = %v, want duplicate creates tolerated", err)
	}
	if result.Summary.Created != 0 || result.Summary.Failed != 0 || result.Summary.Skipped != len(labels.Standard()) {
		t.Fatalf("InitializeLabels() summary = %#v, want all labels skipped after create races", result.Summary)
	}
}
