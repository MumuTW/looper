package labels

import "testing"

// Forge labels are case-insensitive and reach Looper with incidental
// whitespace from CLI output and config files, so every comparison has to
// survive both.
func TestNormalizeAndHas(t *testing.T) {
	t.Parallel()

	if got := Normalize("  LOOPER:Plan\n"); got != DefaultPlanTrigger {
		t.Fatalf("Normalize() = %q, want %q", got, DefaultPlanTrigger)
	}
	if !Has([]string{"other", " LOOPER:HOLD "}, HoldGlobal) {
		t.Fatal("Has() = false for a differently-cased, padded hold label, want true")
	}
	if Has([]string{"looper:hold:worker"}, HoldGlobal) {
		t.Fatal("Has() = true for looper:hold:worker against looper:hold, want false")
	}
	if Has(nil, HoldGlobal) {
		t.Fatal("Has(nil) = true, want false")
	}
}

func TestDoNotMergeMatchesSeparatorVariants(t *testing.T) {
	t.Parallel()
	for _, candidate := range []string{"do-not-merge", "do not merge", "DO NOT MERGE", "do_not_merge", "  Do   Not   Merge  "} {
		if !Has([]string{candidate}, DoNotMerge) {
			t.Fatalf("Has(%q, DoNotMerge) = false, want true", candidate)
		}
	}
}

// A per-role hold must not satisfy the global hold and vice versa: they are
// different vetoes, and conflating them would either over- or under-block a
// Role. The shared prefix makes that an easy mistake, so pin it.
func TestHoldLabelsAreDistinct(t *testing.T) {
	t.Parallel()

	all := []string{HoldGlobal, HoldWorker, HoldFixer, HoldReviewer}
	for i, a := range all {
		for j, b := range all {
			if i != j && a == b {
				t.Fatalf("hold labels %d and %d are both %q", i, j, a)
			}
		}
	}
	if Has([]string{HoldGlobal}, HoldWorker) {
		t.Fatal("global hold satisfied the worker hold, want distinct")
	}
}

func TestLooperOwnership(t *testing.T) {
	t.Parallel()

	for _, label := range []string{DefaultPlanTrigger, DefaultWorkerReadyTrigger, SpecReviewing, SpecReady, NeedsHuman, AwaitingHuman, HoldGlobal, HoldWorker, HoldFixer, HoldReviewer} {
		if !IsLooperOwned(label) {
			t.Fatalf("IsLooperOwned(%q) = false, want true", label)
		}
	}
	if IsLooperOwned("bug") || IsLooperOwned("") {
		t.Fatal("IsLooperOwned() = true for a non-Looper label, want false")
	}
	if !AnyLooperOwned([]string{"bug", "  Looper:Plan  "}) {
		t.Fatal("AnyLooperOwned() = false, want true")
	}
	if AnyLooperOwned([]string{"bug", "enhancement"}) {
		t.Fatal("AnyLooperOwned() = true for non-Looper labels, want false")
	}
}

// These strings are Looper's protocol with the forge: an existing repository
// already carries them, so changing one silently strands in-flight work. Pin
// the wire values so a rename has to be a deliberate, visible edit.
func TestLabelWireValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ got, want string }{
		{DefaultPlanTrigger, "looper:plan"},
		{DefaultWorkerReadyTrigger, "looper:worker-ready"},
		{SpecReviewing, "looper:spec-reviewing"},
		{SpecReady, "looper:spec-ready"},
		{NeedsHuman, "looper:needs-human"},
		{AwaitingHuman, "looper:awaiting-human"},
		{HoldGlobal, "looper:hold"},
		{HoldWorker, "looper:hold:worker"},
		{HoldFixer, "looper:hold:fixer"},
		{HoldReviewer, "looper:hold:reviewer"},
	} {
		if tc.got != tc.want {
			t.Errorf("label = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestMergeQueueRoutingLabelWireValues(t *testing.T) {
	t.Parallel()

	if AutoMerge != "auto-merge" || NeedsHumanReview != "needs-human-review" || DoNotMerge != "do-not-merge" {
		t.Fatalf("routing labels = %q/%q/%q, want the Mergify wire contract", AutoMerge, NeedsHumanReview, DoNotMerge)
	}
	if IsLooperOwned(AutoMerge) || IsLooperOwned(NeedsHumanReview) || IsLooperOwned(DoNotMerge) {
		t.Fatal("Mergify routing labels unexpectedly entered the Looper-owned namespace")
	}
}
