package labels

import (
	"strings"
	"testing"
)

func TestNamespaceDerivesOwnedLabelsAndRejectsForeignDispatch(t *testing.T) {
	ns := NewNamespace("Team.Looper:")
	if ns.Prefix != "team.looper:" {
		t.Fatalf("Prefix = %q, want team.looper:", ns.Prefix)
	}
	if ns.DispatchPlan() != "team.looper:dispatch:plan" || ns.HoldGlobal() != "team.looper:hold" {
		t.Fatalf("derived labels = %q, %q", ns.DispatchPlan(), ns.HoldGlobal())
	}
	if !ns.IsOwned(" TEAM.LOOPER:worker-ready ") || ns.IsOwned("looper:worker-ready") {
		t.Fatal("namespace ownership did not isolate the configured prefix")
	}
	if ns.IsDispatch("dispatch/plan") || !ns.IsDispatch("team.looper:dispatch:plan") || ns.IsDispatch("looper:dispatch:plan") {
		t.Fatal("dispatch compatibility did not isolate the configured namespace")
	}
}

func TestNamespaceRemapsBuiltInsButLeavesForeignLabelsUntouched(t *testing.T) {
	namespace := NewNamespace("team.looper:")
	if got, want := namespace.Remap(DefaultWorkerReadyTrigger), "team.looper:worker-ready"; got != want {
		t.Fatalf("Remap(default) = %q, want %q", got, want)
	}
	if got, want := namespace.Remap("bug"), "bug"; got != want {
		t.Fatalf("Remap(foreign) = %q, want %q", got, want)
	}
	if definition, ok := DefinitionFor("team.looper:hold:worker"); !ok || definition.Color != "b60205" {
		t.Fatalf("DefinitionFor(custom hold) = %#v, %t, want standard hold presentation", definition, ok)
	}
}

func TestDefinitionForPrefersLongestStandardSuffix(t *testing.T) {
	definition, ok := DefinitionFor("team.looper:dispatch:plan")
	if !ok || definition.Name != "team.looper:dispatch:plan" || definition.Description != "Coordinator dispatches this issue to the planner" {
		t.Fatalf("DefinitionFor(dispatch:plan) = %#v, %t, want the dispatch presentation", definition, ok)
	}
}

func TestNamespaceRejectsUnsafePrefixes(t *testing.T) {
	for _, prefix := range []string{"looper", "team looper:", "team/*:", ":"} {
		if err := ValidatePrefix(prefix); err == nil {
			t.Fatalf("ValidatePrefix(%q) succeeded", prefix)
		}
	}
	if got := NewNamespace("bad prefix:"); got.Prefix != Prefix {
		t.Fatalf("invalid namespace fallback = %q, want %q", got.Prefix, Prefix)
	}
	if err := ValidatePrefix("abcdefghijklmnopqrstuvwxyz123456:"); err == nil {
		t.Fatal("ValidatePrefix() accepted a prefix that makes standard labels exceed GitHub's 50-character limit")
	}
}

// A custom namespace nested under the reserved default would make every label
// it emit satisfy the default instance's IsOwned prefix check, so the default
// instance could adopt the custom instance's PRs for auto-merge and
// merge-watch. Only the default itself may use the looper: stem.
func TestNamespaceRejectsPrefixesNestedUnderDefault(t *testing.T) {
	for _, prefix := range []string{"looper:team:", "looper:t:", "LOOPER:Team:", "looper:" + strings.Repeat("a", 20) + ":"} {
		if err := ValidatePrefix(prefix); err == nil {
			t.Fatalf("ValidatePrefix(%q) succeeded, want nested-namespace rejection", prefix)
		}
	}
	for _, prefix := range []string{"looper:", "LOOPER:", "team.looper:", "team:", "looperteam:"} {
		if err := ValidatePrefix(prefix); err != nil {
			t.Fatalf("ValidatePrefix(%q) error = %v, want accepted", prefix, err)
		}
	}
	// The nested fallback keeps legacy callers deterministic: an invalid nested
	// prefix never becomes a second namespace under the default.
	if got := NewNamespace("looper:team:"); got.Prefix != Prefix {
		t.Fatalf("nested namespace fallback = %q, want default %q", got.Prefix, Prefix)
	}
	// Discovery must not map a nested-spelled label back to any namespace.
	if ns, ok := NamespaceForLabel("looper:team:auto-merge"); ok {
		t.Fatalf("NamespaceForLabel(looper:team:auto-merge) = %q, want unrecognized", ns.Prefix)
	}
}
