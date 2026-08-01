package labels

import "testing"

func TestNamespaceDerivesOwnedLabelsAndReadsLegacyDispatch(t *testing.T) {
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

func TestNamespaceRejectsUnsafePrefixes(t *testing.T) {
	for _, prefix := range []string{"looper", "team looper:", "team/*:", ":"} {
		if err := ValidatePrefix(prefix); err == nil {
			t.Fatalf("ValidatePrefix(%q) succeeded", prefix)
		}
	}
	if got := NewNamespace("bad prefix:"); got.Prefix != Prefix {
		t.Fatalf("invalid namespace fallback = %q, want %q", got.Prefix, Prefix)
	}
}
