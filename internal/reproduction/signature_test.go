package reproduction

import (
	"context"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/validationcmd"
)

// failWith builds a runner that exits non-zero with the given output, so each
// case below differs only in what the command printed.
func failWith(output string) func(context.Context, validationcmd.Options) (shell.Result, error) {
	return func(context.Context, validationcmd.Options) (shell.Result, error) {
		result := shell.Result{Stdout: output, ExitCode: 1}
		return result, &shell.CommandExecutionError{Message: "exit 1", Category: shell.FailureNonZeroExit, Result: result}
	}
}

// The signature is what separates "the command failed" from "this bug failed".
// Each case here exits non-zero, which under a bare exit-code check would have
// been accepted as reproduction authority.
func TestProveFailingAcceptsOnlyTheClaimedFailure(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		output string
		want   Reason
	}{
		"a syntax error before any test ran": {
			output: "pkg/bug_test.go:4:1: syntax error: unexpected token",
			want:   ReasonUnexpectedFailure,
		},
		"a setup step that failed": {
			output: "could not connect to the test database; aborting",
			want:   ReasonUnexpectedFailure,
		},
		"an unrelated test that was already failing": {
			output: "FAIL: TestSomethingElse\nbug still present",
			want:   ReasonUnexpectedFailure,
		},
		"the right test failing for a different reason": {
			output: "FAIL: TestBugRepro\nconnection refused",
			want:   ReasonUnexpectedFailure,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFile(t, root, "pkg/bug_test.go", "func TestBugRepro(t *testing.T) {}")
			proof, err := ProveFailing(context.Background(), Input{
				WorktreePath: root, Record: recordFor(t, root), Run: failWith(testCase.output),
			})
			if err != nil {
				t.Fatalf("ProveFailing() error = %v", err)
			}
			if proof.Passed || proof.Reason != testCase.want {
				t.Fatalf("ProveFailing() = %#v, want %s", proof, testCase.want)
			}
		})
	}
}

// The check that keeps the signature from being a free assertion: the test it
// names must exist in a declared reproduction file. Without this an agent could
// quote whatever the command happened to print and call it a reproduction, which
// would make the signature ceremony rather than evidence.
func TestProveFailingRejectsASignatureNoDeclaredFileDefines(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// The declared file exists and is hashed, but does not define the test the
	// signature claims — the agent quoted a pre-existing failure elsewhere.
	writeFile(t, root, "pkg/bug_test.go", "// nothing here yet")
	record := recordFor(t, root)

	proof, err := ProveFailing(context.Background(), Input{
		WorktreePath: root, Record: record, Run: failWith("FAIL: TestBugRepro\nbug still present"),
	})
	if err != nil {
		t.Fatalf("ProveFailing() error = %v", err)
	}
	if proof.Passed || proof.Reason != ReasonUnexpectedFailure {
		t.Fatalf("ProveFailing() = %#v, want the undeclared signature rejected", proof)
	}
	if !strings.Contains(proof.Summary, "no declared reproduction file contains") {
		t.Fatalf("summary = %q, want it to name the missing declaration", proof.Summary)
	}
}

// Whitespace and case differ between a runner's own output and what a human or
// agent quotes from it. Matching on those would reject real reproductions.
func TestProveFailingMatchesAcrossFormattingDifferences(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "func TestBugRepro(t *testing.T) {}")
	proof, err := ProveFailing(context.Background(), Input{
		WorktreePath: root, Record: recordFor(t, root),
		Run: failWith("    --- FAIL: TestBugRepro (0.01s)\n        bug_test.go:9: Bug   Still\n            Present\n"),
	})
	if err != nil {
		t.Fatalf("ProveFailing() error = %v", err)
	}
	if !proof.Passed {
		t.Fatalf("ProveFailing() = %#v, want the reformatted failure accepted", proof)
	}
}

// The signature is committed and pushed with the pull request, so it is a narrow
// validated field rather than a diagnostics dump: an oversized or multi-line
// value is how "a short quote" becomes "the whole log", and the log is what the
// sandbox could have read secrets into.
func TestExpectedFailureRejectsUnboundedOrMultilineValues(t *testing.T) {
	t.Parallel()
	cases := map[string]ExpectedFailure{
		"no test":            {Message: "bug still present"},
		"no message":         {Test: "TestBugRepro"},
		"multiline test":     {Test: "TestBugRepro\nand more", Message: "bug still present"},
		"multiline message":  {Test: "TestBugRepro", Message: "line one\nline two"},
		"oversized message":  {Test: "TestBugRepro", Message: strings.Repeat("x", maxSignatureFieldBytes+1)},
		"control characters": {Test: "TestBugRepro", Message: "bug\x00present"},
	}
	for name, signature := range cases {
		if err := signature.Validate(); err == nil {
			t.Fatalf("ExpectedFailure.Validate(%s) error = nil, want rejection", name)
		}
	}
	valid := ExpectedFailure{Test: " TestBugRepro ", Message: " bug still present "}
	if err := valid.Validate(); err != nil {
		t.Fatalf("ExpectedFailure.Validate() error = %v, want a padded but valid signature accepted", err)
	}
	if normalized := valid.Normalize(); normalized.Test != "TestBugRepro" || normalized.Message != "bug still present" {
		t.Fatalf("Normalize() = %#v, want both halves trimmed", normalized)
	}
}
