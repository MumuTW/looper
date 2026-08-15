package labels

import "testing"

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
	for _, prefix := range []string{"looper", "team looper:", "team/*:", ":", "looper:team:", "looper:team:", "Looper:Team:"} {
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

// A custom namespace nested under the default "looper:" prefix breaks
// isolation: IsOwned is a prefix check, so "looper:team:" labels also satisfy
// the default instance's IsOwned. ValidatePrefix must reject these so two
// namespaced instances on the same repository stay isolated.
func TestNamespaceRejectsNestedLooperPrefix(t *testing.T) {
	for _, prefix := range []string{"looper:team:", "looper:team:", "Looper:Team:", "looper:a:"} {
		if err := ValidatePrefix(prefix); err == nil {
			t.Fatalf("ValidatePrefix(%q) succeeded; want rejection of namespace nested under looper:", prefix)
		}
	}
	// The default prefix itself is valid.
	if err := ValidatePrefix(Prefix); err != nil {
		t.Fatalf("ValidatePrefix(%q) error = %v; want nil (default prefix is valid)", Prefix, err)
	}
	// NewNamespace falls back to default for a nested prefix.
	if got := NewNamespace("looper:team:"); got.Prefix != Prefix {
		t.Fatalf("NewNamespace(looper:team:) = %q, want fallback to %q", got.Prefix, Prefix)
	}
	// A distinct prefix is still accepted.
	if err := ValidatePrefix("team.looper:"); err != nil {
		t.Fatalf("ValidatePrefix(team.looper:) error = %v; want nil", err)
	}
}
