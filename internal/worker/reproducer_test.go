package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/reproducer"
	"github.com/MumuTW/looper/internal/validation"
)

func writeWorkerReproductionFixture(t *testing.T) (string, *reproducer.Manifest) {
	t.Helper()
	root := t.TempDir()
	testPath := filepath.Join(root, "internal", "bug_test.go")
	if err := os.MkdirAll(filepath.Dir(testPath), 0o755); err != nil {
		t.Fatal(err)
	}
	testContent := []byte("func TestBug(t *testing.T) {}\n")
	if err := os.WriteFile(testPath, testContent, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(testContent)
	manifest := &reproducer.Manifest{Version: 1, TestPath: "internal/bug_test.go", TestName: "TestBug", TestCommand: "go test ./internal -run '^TestBug$'", TestSHA256: hex.EncodeToString(hash[:])}
	if err := os.Mkdir(filepath.Join(root, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + manifest.TestSHA256 + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, manifest
}

func TestWorkerReproductionCapturePersistsAndDetectsTampering(t *testing.T) {
	root, expected := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{}}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v", err)
	}
	if checkpoint.Work.Reproduction == nil || !checkpoint.Work.Reproduction.Equal(*expected) {
		t.Fatalf("captured reproduction = %#v, want %#v", checkpoint.Work.Reproduction, expected)
	}
	if err := verifyWorkerReproduction(checkpoint, root); err != nil {
		t.Fatalf("verifyWorkerReproduction() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, expected.TestPath), []byte("func TestBug(t *testing.T) { t.Skip() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyWorkerReproduction(checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("verifyWorkerReproduction() after tamper = %v, want integrity failure", err)
	}
	if got := err.(*loopError).kind; got != FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, FailureManualIntervention)
	}
}

func TestWorkerValidationCommandsIncludeReproductionCommand(t *testing.T) {
	_, manifest := writeWorkerReproductionFixture(t)
	commands := workerValidationCommands([]string{"go test ./..."}, workerInput{Reproduction: manifest})
	if len(commands) != 2 || commands[1] != manifest.TestCommand {
		t.Fatalf("workerValidationCommands() = %#v, want configured plus reproduction command", commands)
	}
	commands = workerValidationCommands(commands, workerInput{Reproduction: manifest})
	if len(commands) != 2 {
		t.Fatalf("workerValidationCommands() duplicated command: %#v", commands)
	}
}

func TestWorkerReproductionBaselineRequiresOrdinaryTestFailure(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{Reproduction: manifest}}
	var got ValidationInput
	runner := New(Options{ValidationRunner: func(_ context.Context, input ValidationInput) (ValidationResult, error) {
		got = input
		return ValidationResult{Passed: false, Summary: "Validation failed: " + manifest.TestCommand, FailureCategory: validation.FailureNonZeroExit}, nil
	}})

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want red evidence", err)
	}
	if checkpoint.ReproductionBaseline == nil || checkpoint.ReproductionBaseline.Passed {
		t.Fatalf("ReproductionBaseline = %#v, want persisted failing result", checkpoint.ReproductionBaseline)
	}
	if len(got.Commands) != 1 || got.Commands[0] != manifest.TestCommand || got.CWD != root {
		t.Fatalf("baseline input = %#v, want exactly the manifest command in the worktree", got)
	}
}

func TestWorkerReproductionBaselineRejectsPassingTest(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{Reproduction: manifest}}
	runner := New(Options{ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: true, Summary: "Validation passed"}, nil
	}})

	err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "passed immediately") {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want immediate-pass rejection", err)
	}
	if got := err.(*loopError).kind; got != FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, FailureManualIntervention)
	}
	if checkpoint.ReproductionBaseline == nil || !checkpoint.ReproductionBaseline.Passed {
		t.Fatalf("ReproductionBaseline = %#v, want persisted passing diagnostic", checkpoint.ReproductionBaseline)
	}
}

func TestWorkerReproductionBaselineRejectsOperationalFailure(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{Reproduction: manifest}}
	runner := New(Options{ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		return ValidationResult{Passed: false, Summary: "Validation timed out: " + manifest.TestCommand, FailureCategory: validation.FailureSupervisorTimeout}, nil
	}})

	err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "ordinary failing test result") {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want operational-failure rejection", err)
	}
	if got := err.(*loopError).kind; got != FailureRetryableTransient {
		t.Fatalf("failure kind = %q, want %q", got, FailureRetryableTransient)
	}
}

func TestWorkerReproductionBaselineIsReplaySafe(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:                 &workerInput{Reproduction: manifest},
		ReproductionBaseline: &ValidationResult{Passed: false, FailureCategory: validation.FailureNonZeroExit},
	}
	called := false
	runner := New(Options{ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		called = true
		return ValidationResult{Passed: true}, nil
	}})

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() replay error = %v", err)
	}
	if called {
		t.Fatal("ensureWorkerReproductionBaseline() re-ran a persisted baseline")
	}
}
