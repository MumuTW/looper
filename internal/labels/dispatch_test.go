package labels

import "testing"

func TestDispatchLabelsAreExactLooperNamespaceLabels(t *testing.T) {
	t.Parallel()

	for _, label := range DispatchLabels() {
		if !IsLooperOwned(label) {
			t.Fatalf("DispatchLabels() returned foreign label %q", label)
		}
		if !IsDispatch(label) {
			t.Fatalf("IsDispatch(%q) = false, want true", label)
		}
	}

	for _, label := range []string{"dispatch/plan", "dispatch/implement", "looper:dispatch:other"} {
		if IsDispatch(label) {
			t.Fatalf("IsDispatch(%q) = true, want false", label)
		}
	}
}

func TestStandardIncludesDispatchLabels(t *testing.T) {
	t.Parallel()

	defined := make(map[string]bool)
	for _, definition := range Standard() {
		defined[definition.Name] = true
	}
	for _, label := range DispatchLabels() {
		if !defined[label] {
			t.Fatalf("Standard() is missing %q", label)
		}
	}
}
