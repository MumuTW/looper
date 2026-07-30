package labelaudit

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/labels"
	"github.com/MumuTW/looper/internal/network/protocol"
)

const repoRoot = "../.."

// The rule this package exists for, applied to this repository.
func TestRepositoryWritesNoLabelOutsideItsOwner(t *testing.T) {
	t.Parallel()

	violations, err := Scan(repoRoot)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	for _, violation := range violations {
		t.Errorf("%s", violation)
	}
}

func scanFixture(t *testing.T, dir string) []Violation {
	t.Helper()
	violations, err := ScanWith(Config{
		Root:   repoRoot,
		Roots:  []string{filepath.Join("internal", "labelaudit", "testdata", dir)},
		Owners: DefaultOwners(),
	})
	if err != nil {
		t.Fatalf("ScanWith(%s) error = %v", dir, err)
	}
	return violations
}

// Each of these bypassed an earlier version of the check. They are kept as
// fixtures rather than as assertions against the repository so that the
// analysis is covered even when no production code happens to contain them.
func TestScanCatchesEveryKnownBypass(t *testing.T) {
	t.Parallel()

	violations := scanFixture(t, "violations")
	found := map[string]Violation{}
	for _, violation := range violations {
		found[violation.Value] = violation
	}

	for _, want := range []struct {
		value  string
		use    string
		reason string
	}{
		{value: labels.DefaultPlanTrigger, use: "labels.DefaultPlanTrigger", reason: "a bare literal"},
		{value: fmt.Sprintf("%s%s", strings.ToUpper(labels.Prefix), strings.Replace(strings.TrimPrefix(labels.DefaultWorkerReadyTrigger, labels.Prefix), "worker-ready", "Worker-Ready", 1)), use: "labels.DefaultWorkerReadyTrigger", reason: "a case variant of the same label"},
		{value: labels.HoldGlobal, use: "labels.HoldGlobal", reason: "assembled through an aliased import"},
		{value: protocol.TargetLabelForNode("blue"), reason: "assembled through a dot-imported owner constant"},
		{value: protocol.TargetLabelForNode("red"), reason: "a concrete member of the target family"},
		{value: labels.SpecReady, use: "labels.SpecReady", reason: "assembled through a function-local constant"},
	} {
		violation, ok := found[want.value]
		if !ok {
			t.Errorf("%s (%q) was not reported", want.reason, want.value)
			continue
		}
		if want.use != "" && violation.Use != want.use {
			t.Errorf("%q reported Use = %q, want %q", want.value, violation.Use, want.use)
		}
		if want.use == "" && !strings.Contains(violation.Message, "TargetLabelForNode") {
			t.Errorf("%q should be told to construct the label, got %q", want.value, violation.Message)
		}
	}
}

func TestScanIncludesTestFiles(t *testing.T) {
	t.Parallel()

	for _, violation := range scanFixture(t, "testonly") {
		if violation.Value == labels.DefaultPlanTrigger && violation.Use == "labels.DefaultPlanTrigger" {
			return
		}
	}
	t.Fatalf("a protected label literal in a test file was not reported")
}

// False positives cost more than the rule is worth: a check that fires on
// correct code gets suppressed rather than obeyed.
func TestScanLeavesCorrectUsageAlone(t *testing.T) {
	t.Parallel()

	for _, violation := range scanFixture(t, "clean") {
		t.Errorf("reported correct usage: %s", violation)
	}
}

// Ownership must be unambiguous. If the two owners ever declared the same
// value, silently letting the second win would make the reported "use this
// instead" wrong.
func TestScanRejectsAmbiguousOwnership(t *testing.T) {
	t.Parallel()

	duplicated := append(DefaultOwners(), Owner{
		Qualifier:  "impostor",
		ImportPath: "github.com/MumuTW/looper/internal/labelaudit/testdata/impostor",
		Dir:        filepath.Join("internal", "labelaudit", "testdata", "impostor"),
	})

	_, err := ScanWith(Config{Root: repoRoot, Roots: []string{"cmd"}, Owners: duplicated})
	if err == nil {
		t.Fatal("ScanWith() error = nil, want ambiguous ownership to be rejected")
	}
	if !strings.Contains(err.Error(), "unambiguous") {
		t.Fatalf("ScanWith() error = %v, want it to name the ambiguity", err)
	}
}

// tools holds production Go that ships in CI; a label hardcoded there is as
// much a second definition as one in internal.
func TestDefaultRootsIncludeTools(t *testing.T) {
	t.Parallel()

	for _, root := range DefaultRoots() {
		if root == "tools" {
			return
		}
	}
	t.Fatalf("DefaultRoots() = %v, want tools included", DefaultRoots())
}

// An owner may define a label as an expression — const NewTrigger = Prefix +
// "new". Deriving the protected set from literals alone would leave that value
// unowned and therefore free to be restated anywhere.
func TestScanProtectsOwnerConstantsDefinedByExpression(t *testing.T) {
	t.Parallel()

	owner := Owner{
		Qualifier:  "impostor",
		ImportPath: "github.com/MumuTW/looper/internal/labelaudit/testdata/impostor",
		Dir:        filepath.Join("internal", "labelaudit", "testdata", "impostor"),
	}
	violations, err := ScanWith(Config{
		Root:   repoRoot,
		Roots:  []string{filepath.Join("internal", "labelaudit", "testdata", "assembled")},
		Owners: []Owner{owner},
	})
	if err != nil {
		t.Fatalf("ScanWith() error = %v", err)
	}
	for _, violation := range violations {
		if violation.Value == "looper:assembled" && violation.Use == "impostor.Assembled" {
			return
		}
	}
	t.Fatalf("an owner constant defined by expression was not protected; got %v", violations)
}
