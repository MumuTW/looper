package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
)

type reproductionGateFixture struct {
	repos    *storage.Repositories
	worktree string
	runner   *Runner
	input    stepInput
	work     workerInput
}

func newReproductionGateFixture(t *testing.T, record reproduction.Record) *reproductionGateFixture {
	t.Helper()
	base := newRunnerFixture(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "bug_test.go"), []byte("original reproduction"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	hashes, err := reproduction.HashFiles(worktree, []string{"bug_test.go"})
	if err != nil {
		t.Fatalf("HashFiles() error = %v", err)
	}
	record.ProjectID = "project_1"
	record.Repo = "acme/looper"
	record.IssueNumber = 41
	record.Files = hashes
	record.RecordedAt = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if err := reproduction.AppendRecord(context.Background(), base.repos, record); err != nil {
		t.Fatalf("AppendRecord() error = %v", err)
	}
	if err := reproduction.WriteManifest(worktree, reproduction.Manifest{
		Version: reproduction.ManifestVersion, Repo: record.Repo, IssueNumber: record.IssueNumber,
		Command: record.Command, Files: hashes, ExpectedFailure: record.ExpectedFailure,
		IdempotencyKey: record.IdempotencyKey,
	}); err != nil {
		t.Fatalf("WriteManifest() error = %v", err)
	}
	work := workerInput{Repo: "acme/looper", IssueNumber: 41}
	return &reproductionGateFixture{
		repos:    base.repos,
		worktree: worktree,
		runner:   New(Options{Repos: base.repos}),
		input:    stepInput{Project: storage.ProjectRecord{ID: "project_1"}},
		work:     work,
	}
}

func TestWorkerReproductionGateIsInertWithoutARecord(t *testing.T) {
	t.Parallel()
	base := newRunnerFixture(t)
	runner := New(Options{Repos: base.repos})
	err := runner.enforceReproductionGate(context.Background(),
		stepInput{Project: storage.ProjectRecord{ID: "project_1"}},
		workerInput{Repo: "acme/looper", IssueNumber: 41}, t.TempDir())
	if err != nil {
		t.Fatalf("enforceReproductionGate() error = %v, want inert without a Reproduction Record", err)
	}
}

// The completion gate is additional to the full suite, not a replacement: this
// case has a green suite by construction and still fails because the recorded
// reproduction was weakened.
func TestWorkerReproductionGateFailsOnATamperedReproduction(t *testing.T) {
	t.Parallel()
	fixture := newReproductionGateFixture(t, reproduction.Record{
		Command: "run-reproduction", CommitSHA: "abc",
		ExpectedFailure: reproduction.ExpectedFailure{Test: "original reproduction", Message: "want 1 got 0"},
	})
	if err := os.WriteFile(filepath.Join(fixture.worktree, "bug_test.go"), []byte("t.Skip()"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := fixture.runner.enforceReproductionGate(context.Background(), fixture.input, fixture.work, fixture.worktree)
	if err == nil {
		t.Fatalf("enforceReproductionGate() error = nil, want a tamper failure")
	}
	loopErr, ok := err.(*loopError)
	if !ok {
		t.Fatalf("enforceReproductionGate() error = %T, want a loopError", err)
	}
	if loopErr.kind != FailureManualIntervention {
		t.Fatalf("failure kind = %q, want manual intervention", loopErr.kind)
	}
	for _, want := range []string{string(reproduction.ReasonTestModified), "bug_test.go", "acme/looper#41"} {
		if !strings.Contains(loopErr.message, want) {
			t.Fatalf("message = %q, want it to contain %q", loopErr.message, want)
		}
	}
}

// Timeouts, cancellations, and containment failures all arrive as the same
// command-execution error. Reading them as "the bug still fails" converts a
// transient condition into a demand for a human, unlike the repository's own
// validation policy, which retries those categories.
func TestWorkerGateRetriesExecutionErrorsAndEscalatesVerdicts(t *testing.T) {
	t.Parallel()
	cases := map[reproduction.Reason]QueueFailureKind{
		reproduction.ReasonCommandError:      FailureRetryableTransient,
		reproduction.ReasonCommandFailed:     FailureManualIntervention,
		reproduction.ReasonTestModified:      FailureManualIntervention,
		reproduction.ReasonTestMissing:       FailureManualIntervention,
		reproduction.ReasonUnexpectedFailure: FailureManualIntervention,
	}
	for reason, want := range cases {
		if got := reproductionGateFailureKind(reproduction.Result{Reason: reason}); got != want {
			t.Fatalf("reproductionGateFailureKind(%s) = %q, want %q", reason, got, want)
		}
	}
}
