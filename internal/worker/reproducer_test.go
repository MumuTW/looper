package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/loops/runpipe"
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
	if got := err.(*runpipe.LoopError).Kind; got != runpipe.FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, runpipe.FailureManualIntervention)
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

func TestWorkerReproductionAbsentBlocksPreExecutionAdoption(t *testing.T) {
	root, expected := writeWorkerReproductionFixtureWithIssue(t, 1, "acme/app")
	// First empty capture records absence.
	emptyRoot := t.TempDir()
	checkpoint := workerCheckpoint{Work: &workerInput{IssueNumber: 1, Repo: "acme/app"}}
	if err := captureWorkerReproduction(&checkpoint, emptyRoot); err != nil {
		t.Fatalf("empty capture error = %v", err)
	}
	if !checkpoint.ReproductionAbsent {
		t.Fatal("want ReproductionAbsent after empty capture")
	}
	// Pre-execution resume must not adopt agent-authored manifest.
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "before agent execution completed") {
		t.Fatalf("capture = %v, want pre-execution refusal", err)
	}
	// After completed execution, worker-authored adoption is allowed.
	checkpoint.Execution = &checkpointExecution{Status: "completed", ParseStatus: "parsed", CompletionPayload: workerReproductionCompletionJSON(t, expected)}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("post-execution capture error = %v", err)
	}
	if checkpoint.Work.Reproduction == nil || !checkpoint.Work.Reproduction.Equal(*expected) {
		t.Fatalf("captured = %#v, want %#v", checkpoint.Work.Reproduction, expected)
	}
	if !checkpoint.ReproductionAuthored {
		t.Fatal("ReproductionAuthored = false, want authored provenance after post-execution adoption")
	}
}

func workerReproductionCompletionPayload(t *testing.T, manifest *reproducer.Manifest) string {
	t.Helper()
	return "__LOOPER_RESULT__=" + workerReproductionCompletionJSON(t, manifest)
}

func workerReproductionCompletionJSON(t *testing.T, manifest *reproducer.Manifest) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"summary": "added reproduction", "reproduction": manifest})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func TestWorkerReproductionCaptureVerifiesOnFirstAdopt(t *testing.T) {
	root, expected := writeWorkerReproductionFixture(t)
	bad := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + strings.Repeat("0", 64) + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), bad, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint := workerCheckpoint{Work: &workerInput{IssueNumber: 1, Repo: "acme/app"}}
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil {
		t.Fatal("captureWorkerReproduction() error = nil, want hash verify failure")
	}
	if checkpoint.Work.Reproduction != nil {
		t.Fatalf("Reproduction = %#v, want nil after failed first capture", checkpoint.Work.Reproduction)
	}
	good := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + expected.TestSHA256 + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), good, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint = workerCheckpoint{Work: &workerInput{IssueNumber: 1, Repo: "acme/app"}}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v", err)
	}
	if checkpoint.Work.Reproduction == nil || !checkpoint.Work.Reproduction.Equal(*expected) {
		t.Fatalf("captured = %#v, want %#v", checkpoint.Work.Reproduction, expected)
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

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeCreate); err != nil {
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

	err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeCreate)
	if err == nil || !strings.Contains(err.Error(), "passed immediately") {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want immediate-pass rejection", err)
	}
	if got := err.(*runpipe.LoopError).Kind; got != runpipe.FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, runpipe.FailureManualIntervention)
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

	err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeCreate)
	if err == nil || !strings.Contains(err.Error(), "ordinary failing test result") {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want operational-failure rejection", err)
	}
	if got := err.(*runpipe.LoopError).Kind; got != runpipe.FailureRetryableTransient {
		t.Fatalf("failure kind = %q, want %q", got, runpipe.FailureRetryableTransient)
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

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeCreate); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() replay error = %v", err)
	}
	if called {
		t.Fatal("ensureWorkerReproductionBaseline() re-ran a persisted baseline")
	}
}

// TestWorkerReproductionBaselineDiscardsRejectedObservationOnReuse covers the
// P1 review item: a rejected observation (immediate pass or operational
// failure) that was persisted must not be silently accepted on the next retry.
// It is discarded and re-derived instead of gating Worker execution.
func TestWorkerReproductionBaselineDiscardsRejectedObservationOnReuse(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	// A previously persisted immediate-pass diagnostic, as the prior round would
	// have stored before returning the manual-intervention error.
	checkpoint := workerCheckpoint{
		Work:                 &workerInput{Reproduction: manifest},
		ReproductionBaseline: &ValidationResult{Passed: true, Summary: "Validation passed"},
	}
	called := false
	runner := New(Options{ValidationRunner: func(_ context.Context, _ ValidationInput) (ValidationResult, error) {
		called = true
		return ValidationResult{Passed: false, Summary: "Validation failed: " + manifest.TestCommand, FailureCategory: validation.FailureNonZeroExit}, nil
	}})

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeCreate); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want re-derived red evidence", err)
	}
	if !called {
		t.Fatal("ensureWorkerReproductionBaseline() reused a rejected passing observation instead of re-deriving")
	}
	if checkpoint.ReproductionBaseline == nil || checkpoint.ReproductionBaseline.Passed || checkpoint.ReproductionBaseline.FailureCategory != validation.FailureNonZeroExit {
		t.Fatalf("ReproductionBaseline = %#v, want re-derived ordinary failing result", checkpoint.ReproductionBaseline)
	}
}

// TestWorkerReproductionBaselineReuseOnlyFailsClosed covers the P1 review item:
// after Worker execution (validate/open-pr), a missing baseline must fail
// closed rather than manufacture red evidence from a possibly mutated worktree.
func TestWorkerReproductionBaselineReuseOnlyFailsClosed(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{Reproduction: manifest}}
	called := false
	runner := New(Options{ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		called = true
		return ValidationResult{Passed: false, FailureCategory: validation.FailureNonZeroExit}, nil
	}})

	err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeReuseOnly)
	if err == nil || !strings.Contains(err.Error(), "absent after Worker execution") {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want fail-closed after execution", err)
	}
	if got := err.(*runpipe.LoopError).Kind; got != runpipe.FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, runpipe.FailureManualIntervention)
	}
	if called {
		t.Fatal("ensureWorkerReproductionBaseline() ran the reproduction after execution instead of failing closed")
	}
	if checkpoint.ReproductionBaseline != nil {
		t.Fatalf("ReproductionBaseline = %#v, want nil (no manufactured evidence)", checkpoint.ReproductionBaseline)
	}
}

func TestWorkerReproductionBaselineReuseOnlyAllowsAuthoredManifest(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:                 &workerInput{Reproduction: manifest},
		ReproductionAuthored: true,
	}
	called := false
	runner := New(Options{ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		called = true
		return ValidationResult{Passed: false, FailureCategory: validation.FailureNonZeroExit}, nil
	}})

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeReuseOnly); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want authored contract to skip impossible baseline", err)
	}
	if called {
		t.Fatal("ensureWorkerReproductionBaseline() manufactured red evidence after authored execution")
	}
}

// TestWorkerReproductionBaselineReuseOnlyReusesAcceptableEvidence ensures a
// reuse-only scope still trusts an acceptable red baseline captured before
// execution, so the post-execution gate does not block valid work.
func TestWorkerReproductionBaselineReuseOnlyReusesAcceptableEvidence(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:                 &workerInput{Reproduction: manifest},
		ReproductionBaseline: &ValidationResult{Passed: false, FailureCategory: validation.FailureNonZeroExit, HeadSHA: "abc123"},
	}
	called := false
	runner := New(Options{ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
		called = true
		return ValidationResult{Passed: true}, nil
	}})

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "", "", reproductionBaselineScopeReuseOnly); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() reuse error = %v", err)
	}
	if called {
		t.Fatal("ensureWorkerReproductionBaseline() re-ran an acceptable baseline in reuse-only scope")
	}
}

// TestWorkerReproductionBaselineRederivesOnHeadDrift covers the P2 review item:
// a persisted baseline bound to a different checkout head is discarded and
// re-derived before execution so a drifted worktree cannot ride on stale red
// evidence.
func TestWorkerReproductionBaselineRederivesOnHeadDrift(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:                 &workerInput{Reproduction: manifest},
		Worktree:             &checkpointWorktree{Path: root, HeadSHA: "abc123", BaseBranch: "main"},
		ReproductionBaseline: &ValidationResult{Passed: false, FailureCategory: validation.FailureNonZeroExit, HeadSHA: "abc123"},
	}
	called := false
	runner := New(Options{
		Git: &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "drifted-head"}},
		ValidationRunner: func(_ context.Context, _ ValidationInput) (ValidationResult, error) {
			called = true
			return ValidationResult{Passed: false, Summary: "Validation failed: " + manifest.TestCommand, FailureCategory: validation.FailureNonZeroExit, HeadSHA: "drifted-head"}, nil
		},
	})

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "repo", "root", reproductionBaselineScopeCreate); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want re-derived red evidence", err)
	}
	if !called {
		t.Fatal("ensureWorkerReproductionBaseline() reused a baseline whose recorded head drifted")
	}
	if checkpoint.ReproductionBaseline == nil || checkpoint.ReproductionBaseline.HeadSHA != "drifted-head" {
		t.Fatalf("ReproductionBaseline = %#v, want re-derived evidence bound to the current head", checkpoint.ReproductionBaseline)
	}
}

// TestWorkerReproductionBaselineReusesWhenHeadMatches ensures a pre-execution
// baseline is reused when the checkout head still matches the recorded head.
func TestWorkerReproductionBaselineReusesWhenHeadMatches(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:                 &workerInput{Reproduction: manifest},
		Worktree:             &checkpointWorktree{Path: root, HeadSHA: "abc123", BaseBranch: "main"},
		ReproductionBaseline: &ValidationResult{Passed: false, FailureCategory: validation.FailureNonZeroExit, HeadSHA: "abc123"},
	}
	called := false
	runner := New(Options{
		Git: &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "abc123"}},
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			called = true
			return ValidationResult{Passed: true}, nil
		},
	})

	if err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "repo", "root", reproductionBaselineScopeCreate); err != nil {
		t.Fatalf("ensureWorkerReproductionBaseline() reuse error = %v", err)
	}
	if called {
		t.Fatal("ensureWorkerReproductionBaseline() re-ran a baseline whose head still matched")
	}
}

// TestWorkerReproductionBaselineRejectsDirtyWorktree covers the P2 review item:
// a reproduction command that modifies the worktree is rejected so the Worker
// does not start on side effects that reconcileWorkerGitState could commit.
func TestWorkerReproductionBaselineRejectsDirtyWorktree(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{Reproduction: manifest}, Worktree: &checkpointWorktree{Path: root, HeadSHA: "abc123", BaseBranch: "main"}}
	runner := New(Options{
		Git: &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "abc123", HasUncommittedChanges: true}},
		ValidationRunner: func(context.Context, ValidationInput) (ValidationResult, error) {
			return ValidationResult{Passed: false, Summary: "Validation failed: " + manifest.TestCommand, FailureCategory: validation.FailureNonZeroExit, HeadSHA: "abc123"}, nil
		},
	})

	err := runner.ensureWorkerReproductionBaseline(context.Background(), &checkpoint, root, "repo", "root", reproductionBaselineScopeCreate)
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("ensureWorkerReproductionBaseline() error = %v, want dirty-worktree rejection", err)
	}
	if got := err.(*runpipe.LoopError).Kind; got != runpipe.FailureManualIntervention {
		t.Fatalf("failure kind = %q, want %q", got, runpipe.FailureManualIntervention)
	}
	if checkpoint.ReproductionBaseline != nil {
		t.Fatalf("ReproductionBaseline = %#v, want nil (no evidence accepted on dirty worktree)", checkpoint.ReproductionBaseline)
	}
}

// writeWorkerReproductionFixtureWithIssue writes a reproduction fixture whose
// manifest declares an originating issue scope.
func writeWorkerReproductionFixtureWithIssue(t *testing.T, issueNumber int64, issueRepo string) (string, *reproducer.Manifest) {
	t.Helper()
	root, manifest := writeWorkerReproductionFixture(t)
	manifest.IssueNumber = issueNumber
	manifest.IssueRepo = issueRepo
	data := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + manifest.TestSHA256 + `","issueNumber":` + strconv.FormatInt(issueNumber, 10) + `,"issueRepo":"` + issueRepo + `"}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root, manifest
}

// TestWorkerCaptureReproductionSkipsManifestScopedToOtherIssue covers the P1
// review item: a manifest left in the base branch after a previous fix merged
// is already-fixed history for a different issue and must not be adopted as the
// current task's reproduction.
func TestWorkerCaptureReproductionSkipsManifestScopedToOtherIssue(t *testing.T) {
	root, _ := writeWorkerReproductionFixtureWithIssue(t, 999, "MumuTW/looper")
	checkpoint := workerCheckpoint{Work: &workerInput{Repo: "MumuTW/looper", IssueNumber: 113}}

	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v", err)
	}
	if checkpoint.Work.Reproduction != nil {
		t.Fatalf("Reproduction = %#v, want nil for a manifest scoped to a different issue", checkpoint.Work.Reproduction)
	}
}

func TestWorkerCaptureReproductionPreservesHistoricalManifestAcrossCompletion(t *testing.T) {
	root, historical := writeWorkerReproductionFixtureWithIssue(t, 999, "MumuTW/looper")
	checkpoint := workerCheckpoint{Work: &workerInput{Repo: "MumuTW/looper", IssueNumber: 113}}

	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("initial capture = %v, want historical manifest ignored", err)
	}
	if !checkpoint.ReproductionAbsent || checkpoint.ReproductionHistorical == nil || !checkpoint.ReproductionHistorical.Equal(*historical) {
		t.Fatalf("checkpoint after initial capture = %#v, want historical identity and absence", checkpoint)
	}

	checkpoint.Execution = &checkpointExecution{Status: "completed", ParseStatus: "parsed"}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("post-completion capture = %v, want unchanged historical manifest preserved", err)
	}
	if checkpoint.Work.Reproduction != nil || !checkpoint.ReproductionAbsent {
		t.Fatalf("checkpoint after completion = %#v, want historical manifest still ignored", checkpoint)
	}
}

// TestWorkerCaptureReproductionAdoptsManifestScopedToSameIssue ensures a
// manifest declaring the current task's issue is still adopted.
func TestWorkerCaptureReproductionAdoptsManifestScopedToSameIssue(t *testing.T) {
	root, manifest := writeWorkerReproductionFixtureWithIssue(t, 113, "MumuTW/looper")
	checkpoint := workerCheckpoint{Work: &workerInput{Repo: "MumuTW/looper", IssueNumber: 113}}

	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v", err)
	}
	if checkpoint.Work.Reproduction == nil || !checkpoint.Work.Reproduction.Equal(*manifest) {
		t.Fatalf("Reproduction = %#v, want adopted manifest for the current issue", checkpoint.Work.Reproduction)
	}
}

// TestWorkerCaptureReproductionAdoptsUnscopedManifest ensures a legacy manifest
// with no declared issue remains backward-compatible and is adopted.
func TestWorkerCaptureReproductionAdoptsUnscopedManifest(t *testing.T) {
	root, manifest := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{Work: &workerInput{Repo: "MumuTW/looper", IssueNumber: 113}}

	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v", err)
	}
	if checkpoint.Work.Reproduction == nil || !checkpoint.Work.Reproduction.Equal(*manifest) {
		t.Fatalf("Reproduction = %#v, want adopted unscoped manifest", checkpoint.Work.Reproduction)
	}
}

func TestWorkerCaptureReproductionRejectsNewUnscopedManifestForIssue(t *testing.T) {
	root, _ := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "completed", ParseStatus: "parsed"},
	}
	if err := captureWorkerReproduction(&checkpoint, root); err == nil || !strings.Contains(err.Error(), "does not match the current issue scope") {
		t.Fatalf("captureWorkerReproduction() = %v, want issue-scoped worker-authored rejection", err)
	}
	if checkpoint.Work.Reproduction != nil {
		t.Fatalf("Reproduction = %#v, want no authority adopted", checkpoint.Work.Reproduction)
	}
}

func TestWorkerCaptureReproductionIgnoresUntrustedManifestDuringRetry(t *testing.T) {
	root, _ := writeWorkerReproductionFixtureWithIssue(t, 113, "MumuTW/looper")
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "timeout"},
	}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v, want retry to ignore untrusted file", err)
	}
	if checkpoint.Work.Reproduction != nil || !checkpoint.ReproductionAbsent {
		t.Fatalf("checkpoint = %#v, want untrusted manifest ignored while absence remains", checkpoint)
	}
}

func TestWorkerCaptureReproductionDefersMalformedManifestDuringRetry(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".looper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), []byte(`{"version":`), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "timeout"},
	}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() = %v, want malformed retry file deferred", err)
	}
	if !checkpoint.ReproductionAbsent || checkpoint.Work.Reproduction != nil {
		t.Fatalf("checkpoint = %#v, want absence retained and no adopted reproduction", checkpoint)
	}
}

func TestWorkerCaptureReproductionIgnoresInvalidCompletedExecutionDuringRetry(t *testing.T) {
	root, _ := writeWorkerReproductionFixtureWithIssue(t, 113, "MumuTW/looper")
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "completed", ParseStatus: "missing"},
	}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() = %v, want invalid completed retry evidence deferred", err)
	}
	if !checkpoint.ReproductionAbsent || checkpoint.Work.Reproduction != nil {
		t.Fatalf("checkpoint = %#v, want absence retained and no adopted reproduction", checkpoint)
	}
}

func TestWorkerCaptureReproductionRequiresRetryAttestationForInheritedManifest(t *testing.T) {
	root, _ := writeWorkerReproductionFixtureWithIssue(t, 113, "MumuTW/looper")
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "completed", ParseStatus: "parsed", CompletionPayload: `{"summary":"finished retry","reproduction":null}`},
	}
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "missing reproduction") {
		t.Fatalf("captureWorkerReproduction() = %v, want inherited manifest attestation refusal", err)
	}
	if checkpoint.Work.Reproduction != nil || !checkpoint.ReproductionAbsent {
		t.Fatalf("checkpoint = %#v, want inherited file unadopted and absence retained", checkpoint)
	}
}

func TestWorkerInvalidCompletedExecutionReplaysExecute(t *testing.T) {
	checkpoint := workerCheckpoint{Execution: &checkpointExecution{Status: "completed", ParseStatus: "missing"}}
	if !shouldReplayExecuteOnResume("failed", stepExecute, checkpoint) {
		t.Fatal("shouldReplayExecuteOnResume() = false, want replay for invalid completed execution")
	}
	rewound := rewindCheckpointForExecuteRetry(checkpoint)
	if rewound.Execution == nil || executeStepAlreadyCompleted(rewound) {
		t.Fatalf("rewound checkpoint = %#v, want invalid execution retained but not treated as completed", rewound)
	}
}

func TestWorkerCaptureReproductionRejectsMismatchedAuthoredIssueScope(t *testing.T) {
	root, manifest := writeWorkerReproductionFixtureWithIssue(t, 999, "MumuTW/looper")
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "completed", CompletionPayload: workerReproductionCompletionJSON(t, manifest)},
	}
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "does not match the current issue scope") {
		t.Fatalf("captureWorkerReproduction() = %v, want wrong issue scope refusal", err)
	}
}

func TestWorkerCaptureReproductionRejectsAuthoredIssueScopeWithoutRepository(t *testing.T) {
	root, manifest := writeWorkerReproductionFixtureWithIssue(t, 113, "MumuTW/looper")
	manifest.IssueRepo = ""
	data := []byte(`{"version":1,"testPath":"internal/bug_test.go","testName":"TestBug","testCommand":"go test ./internal -run '^TestBug$'","testSha256":"` + manifest.TestSHA256 + `","issueNumber":113}`)
	if err := os.WriteFile(filepath.Join(root, reproducer.ManifestPath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "completed", CompletionPayload: workerReproductionCompletionJSON(t, manifest)},
	}
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "does not match the current issue scope") {
		t.Fatalf("captureWorkerReproduction() = %v, want missing repository scope refusal", err)
	}
}

func TestWorkerCaptureReproductionRejectsAuthoredManifestWithoutStructuredContract(t *testing.T) {
	root, _ := writeWorkerReproductionFixtureWithIssue(t, 113, "MumuTW/looper")
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "completed", ParseStatus: "parsed"},
	}
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "missing a structured completion contract") {
		t.Fatalf("captureWorkerReproduction() = %v, want missing structured contract refusal", err)
	}
	if checkpoint.Work.Reproduction != nil {
		t.Fatalf("Reproduction = %#v, want no authority adopted", checkpoint.Work.Reproduction)
	}
}

func TestWorkerCaptureReproductionRejectsMismatchedStructuredContract(t *testing.T) {
	root, manifest := writeWorkerReproductionFixtureWithIssue(t, 113, "MumuTW/looper")
	declared := *manifest
	declared.TestCommand = "go test ./... -run '^TestOther$'"
	checkpoint := workerCheckpoint{
		Work:               &workerInput{Repo: "MumuTW/looper", IssueNumber: 113},
		ReproductionAbsent: true,
		Execution:          &checkpointExecution{Status: "completed", ParseStatus: "parsed", Stdout: workerReproductionCompletionPayload(t, &declared)},
	}
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "does not match the structured completion contract") {
		t.Fatalf("captureWorkerReproduction() = %v, want contract mismatch refusal", err)
	}
	if checkpoint.Work.Reproduction != nil {
		t.Fatalf("Reproduction = %#v, want no authority adopted", checkpoint.Work.Reproduction)
	}
}

func TestWorkerReproductionFirstCaptureWithPreparedWorktreeAdoptsManifest(t *testing.T) {
	// Prepare-worktree always sets Worktree before the first capture call.
	// That must not trip the legacy past-initial fail-closed path.
	root, expected := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:     &workerInput{IssueNumber: 1, Repo: "acme/app"},
		Worktree: &checkpointWorktree{Path: root, Branch: "worker/issue-1"},
	}
	if err := captureWorkerReproduction(&checkpoint, root); err != nil {
		t.Fatalf("captureWorkerReproduction() error = %v", err)
	}
	if checkpoint.Work.Reproduction == nil || !checkpoint.Work.Reproduction.Equal(*expected) {
		t.Fatalf("captured = %#v, want %#v", checkpoint.Work.Reproduction, expected)
	}
	if checkpoint.ReproductionAbsent {
		t.Fatal("ReproductionAbsent = true, want false after successful first capture")
	}
}

func TestWorkerReproductionFailsClosedForLegacyUnknownAbsence(t *testing.T) {
	root, _ := writeWorkerReproductionFixture(t)
	checkpoint := workerCheckpoint{
		Work:      &workerInput{IssueNumber: 1, Repo: "acme/app"},
		Worktree:  &checkpointWorktree{Path: root, Branch: "worker/issue-1"},
		Execution: &checkpointExecution{Status: "timeout"},
	}
	err := captureWorkerReproduction(&checkpoint, root)
	if err == nil || !strings.Contains(err.Error(), "legacy checkpoint") {
		t.Fatalf("captureWorkerReproduction() = %v, want legacy unknown absence refusal", err)
	}
	if checkpoint.Work.Reproduction != nil {
		t.Fatalf("Reproduction = %#v, want nil", checkpoint.Work.Reproduction)
	}
}
