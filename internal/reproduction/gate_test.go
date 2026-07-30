package reproduction

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/validationcmd"
)

func writeFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func recordFor(t *testing.T, root string) Record {
	t.Helper()
	hashes, err := HashFiles(root, []string{"pkg/bug_test.go"})
	if err != nil {
		t.Fatalf("HashFiles() error = %v", err)
	}
	return Record{Repo: "acme/looper", IssueNumber: 7, Command: "run-reproduction", Files: hashes}
}

func passing(context.Context, validationcmd.Options) (shell.Result, error) {
	return shell.Result{Stdout: "ok"}, nil
}

func failing(context.Context, validationcmd.Options) (shell.Result, error) {
	result := shell.Result{Stdout: "FAIL: bug still present", ExitCode: 1}
	return result, &shell.CommandExecutionError{Message: "exit 1", Category: shell.FailureNonZeroExit, Result: result}
}

func TestVerifyPassesWhenReproductionIsIntactAndGreen(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "original reproduction")
	result, err := Verify(context.Background(), Input{WorktreePath: root, Record: recordFor(t, root), Run: passing})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !result.Passed {
		t.Fatalf("Verify() = %#v, want pass", result)
	}
}

func TestVerifyFailsWhenReproductionStillFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "original reproduction")
	result, err := Verify(context.Background(), Input{WorktreePath: root, Record: recordFor(t, root), Run: failing})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.Passed || result.Reason != ReasonCommandFailed {
		t.Fatalf("Verify() = %#v, want %s", result, ReasonCommandFailed)
	}
}

// The tamper cases are the reason the Role exists: the suite would still be
// green after weakening the test, so the gate must see it from the hash rather
// than from the suite.
func TestVerifyDetectsModifiedReproductionWithoutRunningTheCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "original reproduction")
	record := recordFor(t, root)
	writeFile(t, root, "pkg/bug_test.go", "t.Skip(\"flaky\")")

	ran := false
	result, err := Verify(context.Background(), Input{WorktreePath: root, Record: record, Run: func(ctx context.Context, options validationcmd.Options) (shell.Result, error) {
		ran = true
		return passing(ctx, options)
	}})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.Passed || result.Reason != ReasonTestModified {
		t.Fatalf("Verify() = %#v, want %s", result, ReasonTestModified)
	}
	if ran {
		t.Fatalf("Verify() ran the reproduction command despite a tampered file")
	}
	if !strings.Contains(result.Summary, "pkg/bug_test.go") || !strings.Contains(result.Summary, "modified after the reproduction commit") {
		t.Fatalf("Verify() summary = %q, want a non-generic tamper message naming the file", result.Summary)
	}
}

func TestVerifyDetectsDeletedReproduction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "original reproduction")
	record := recordFor(t, root)
	if err := os.Remove(filepath.Join(root, "pkg", "bug_test.go")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	result, err := Verify(context.Background(), Input{WorktreePath: root, Record: record, Run: passing})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if result.Passed || result.Reason != ReasonTestMissing {
		t.Fatalf("Verify() = %#v, want %s", result, ReasonTestMissing)
	}
	if !strings.Contains(result.Summary, "was deleted after the reproduction commit") {
		t.Fatalf("Verify() summary = %q, want a distinct deletion message", result.Summary)
	}
}

func TestProveFailingAcceptsOnlyACommandThatFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/bug_test.go", "candidate reproduction")
	record := recordFor(t, root)

	proof, err := ProveFailing(context.Background(), Input{WorktreePath: root, Record: record, Run: failing})
	if err != nil {
		t.Fatalf("ProveFailing() error = %v", err)
	}
	if !proof.Passed {
		t.Fatalf("ProveFailing() = %#v, want the observed failure accepted", proof)
	}

	rejected, err := ProveFailing(context.Background(), Input{WorktreePath: root, Record: record, Run: passing})
	if err != nil {
		t.Fatalf("ProveFailing() error = %v", err)
	}
	if rejected.Passed || rejected.Reason != ReasonCommandPassedOnBase {
		t.Fatalf("ProveFailing() = %#v, want %s", rejected, ReasonCommandPassedOnBase)
	}
}

func TestFailureSummaryNamesTheReproductionRatherThanValidation(t *testing.T) {
	t.Parallel()
	summary := FailureSummary("acme/looper", 7, Result{Reason: ReasonTestModified, Summary: "detail"})
	if !strings.Contains(summary, "Reproduction gate for acme/looper#7") || !strings.Contains(summary, string(ReasonTestModified)) {
		t.Fatalf("FailureSummary() = %q, want a reproduction-specific failure reason", summary)
	}
}
