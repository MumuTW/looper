package hitl

import "testing"

func TestFingerprintContentStable(t *testing.T) {
	a := FingerprintContent("hello", "world")
	b := FingerprintContent("hello", "world")
	if a == "" || a != b {
		t.Fatalf("FingerprintContent unstable: %q vs %q", a, b)
	}
	if FingerprintContent("hello", "world") == FingerprintContent("hello", "other") {
		t.Fatal("different inputs produced same fingerprint")
	}
}

func TestMaterialFingerprintsMatch(t *testing.T) {
	if !MaterialFingerprintsMatch("", "live", "", "r", "", "i") {
		t.Fatal("empty stored fingerprints should match (not checked)")
	}
	if MaterialFingerprintsMatch("abc", "def", "", "", "", "") {
		t.Fatal("mismatched head should not match")
	}
	if !MaterialFingerprintsMatch("abc", "abc", "r1", "r1", "i1", "i1") {
		t.Fatal("matching fingerprints should match")
	}
	if MaterialFingerprintsMatch("abc", "abc", "r1", "r2", "", "") {
		t.Fatal("mismatched review fingerprint should not match")
	}
}
