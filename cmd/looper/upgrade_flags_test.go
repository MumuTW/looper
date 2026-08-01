package main

import "testing"

func TestSplitGlobalFlagsPreservesUpgradeOwnedFlags(t *testing.T) {
	t.Parallel()
	parsed, err := splitGlobalFlags([]string{
		"upgrade", "preflight",
		"--target-looper", "/tmp/looper",
		"--target-looperd", "/tmp/looperd",
		"--json",
	})
	if err != nil {
		t.Fatalf("splitGlobalFlags() error = %v", err)
	}
	if parsed.Verb != "upgrade" {
		t.Fatalf("Verb = %q", parsed.Verb)
	}
	want := []string{"preflight", "--target-looper", "/tmp/looper", "--target-looperd", "/tmp/looperd", "--json"}
	if len(parsed.Operands) != len(want) {
		t.Fatalf("Operands = %#v, want %#v", parsed.Operands, want)
	}
	for i := range want {
		if parsed.Operands[i] != want[i] {
			t.Fatalf("Operands = %#v, want %#v", parsed.Operands, want)
		}
	}
	// drain flags too
	parsed, err = splitGlobalFlags([]string{"upgrade", "drain", "--deadline", "30s"})
	if err != nil {
		t.Fatalf("drain flags error = %v", err)
	}
	if got := parsed.Operands; len(got) != 3 || got[0] != "drain" || got[1] != "--deadline" || got[2] != "30s" {
		t.Fatalf("drain Operands = %#v", got)
	}
	// stage / activate / restore flags
	for _, args := range [][]string{
		{"upgrade", "stage-release", "--release-root", "/r", "--target-looper", "/l", "--target-looperd", "/d"},
		{"upgrade", "activate-release", "--release-root", "/r", "--release", "id"},
		{"upgrade", "verify-start", "--release-root", "/r", "--release", "id", "--bundle", "/b"},
		{"upgrade", "verify", "--bundle", "/b"},
		{"upgrade", "restore", "--bundle", "/b", "--confirm"},
	} {
		parsed, err = splitGlobalFlags(args)
		if err != nil {
			t.Fatalf("splitGlobalFlags(%v) error = %v", args, err)
		}
		if parsed.Verb != "upgrade" {
			t.Fatalf("Verb = %q for %v", parsed.Verb, args)
		}
		if len(parsed.Operands) != len(args)-1 {
			t.Fatalf("Operands for %v = %#v", args, parsed.Operands)
		}
	}
}
