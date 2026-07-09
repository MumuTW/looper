package runtime

import (
	"testing"

	"github.com/nexu-io/looper/internal/coordinator/triage"
)

func TestDecideAutoIntakeRoute(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		decision       triage.Decision
		hasProductSpec bool
		want           intakeRoute
	}{
		{
			name:     "noop classification leaves the item untouched",
			decision: triage.Decision{NoOp: true},
			want:     intakeSkip,
		},
		{
			name:     "out of scope is marked and stopped",
			decision: triage.Decision{Disposition: triage.DispositionOutOfScope},
			want:     intakeMarkOutOfScope,
		},
		{
			name:     "unclear holds for a human",
			decision: triage.Decision{Disposition: triage.DispositionUnclear},
			want:     intakeHoldUnclear,
		},
		{
			name:           "feature without a product spec holds at node E",
			decision:       triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/feature", "dispatch/plan"}},
			hasProductSpec: false,
			want:           intakeHoldForProductSpec,
		},
		{
			name:           "feature with a product spec routes to the planner",
			decision:       triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/feature", "dispatch/plan"}},
			hasProductSpec: true,
			want:           intakeRouteToPlan,
		},
		{
			name:     "simple bug (dispatch/implement) goes straight to the worker",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/bug", "complexity/s", "dispatch/implement"}},
			want:     intakeRouteToImplement,
		},
		{
			name:     "complex bug (dispatch/plan) writes a tech spec first",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/bug", "complexity/l", "dispatch/plan"}},
			want:     intakeRouteToPlan,
		},
		{
			name:     "valid with no dispatch label falls back to spec-first",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"kind/docs"}},
			want:     intakeRouteToPlan,
		},
		{
			name:     "dispatch label matched case-insensitively",
			decision: triage.Decision{Disposition: triage.DispositionValid, ApplyLabels: []string{"Dispatch/Implement"}},
			want:     intakeRouteToImplement,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideAutoIntakeRoute(c.decision, c.hasProductSpec); got != c.want {
				t.Fatalf("decideAutoIntakeRoute() = %d, want %d", got, c.want)
			}
		})
	}
}

func TestLabelsContainFold(t *testing.T) {
	t.Parallel()

	labels := []string{"looper:auto", " Kind/Bug ", "dispatch/implement"}
	if !labelsContainFold(labels, "kind/bug") {
		t.Fatal("labelsContainFold should match case-insensitively and trim whitespace")
	}
	if !labelsContainFold(labels, "LOOPER:AUTO") {
		t.Fatal("labelsContainFold should match looper:auto")
	}
	if labelsContainFold(labels, "looper:plan") {
		t.Fatal("labelsContainFold should not match an absent label")
	}
}

func TestAutoIntakeEnabledGate(t *testing.T) {
	t.Setenv(autoIntakeEnvVar, "")
	if autoIntakeEnabled() {
		t.Fatal("auto-intake must be off when the env var is unset")
	}
	t.Setenv(autoIntakeEnvVar, "1")
	if !autoIntakeEnabled() {
		t.Fatal("auto-intake must be on when the env var is 1")
	}
}
