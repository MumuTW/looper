package fixer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/reproduction"
	"github.com/nexu-io/looper/internal/storage"
)

func seedFixerReproduction(t *testing.T, repos *storage.Repositories, worktree, command string) reproduction.Record {
	t.Helper()
	if err := os.WriteFile(filepath.Join(worktree, "bug_test.go"), []byte("original reproduction"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	hashes, err := reproduction.HashFiles(worktree, []string{"bug_test.go"})
	if err != nil {
		t.Fatalf("HashFiles() error = %v", err)
	}
	record := reproduction.Record{
		ProjectID: "project_1", Repo: "acme/looper", IssueNumber: 41, Command: command,
		Files: hashes, CommitSHA: "abc", RecordedAt: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}
	if err := reproduction.AppendRecord(context.Background(), repos, record); err != nil {
		t.Fatalf("AppendRecord() error = %v", err)
	}
	if err := reproduction.WriteManifest(worktree, reproduction.Manifest{
		Version: reproduction.ManifestVersion, Repo: record.Repo, IssueNumber: record.IssueNumber,
		Command: command, Files: hashes,
	}); err != nil {
		t.Fatalf("WriteManifest() error = %v", err)
	}
	return record
}

func newFixerGateRepos(t *testing.T) *storage.Repositories {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "fixer-gate.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	stamp := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: stamp, UpdatedAt: stamp,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	return repos
}

func fixerGateLoop(t *testing.T, repos *storage.Repositories) storage.LoopRecord {
	t.Helper()
	stamp := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	repo := "acme/looper"
	prNumber := int64(9)
	loop := storage.LoopRecord{
		ID: "loop_fixer", Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request",
		Repo: &repo, PRNumber: &prNumber, Status: "running", CreatedAt: stamp, UpdatedAt: stamp,
	}
	if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	return loop
}

func TestFixerReproductionGateIsInertWithoutARecord(t *testing.T) {
	t.Parallel()
	repos := newFixerGateRepos(t)
	runner := New(Options{Repos: repos})
	input := stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Repo: "acme/looper", Loop: fixerGateLoop(t, repos)}
	if err := runner.enforceReproductionGate(context.Background(), input, t.TempDir()); err != nil {
		t.Fatalf("enforceReproductionGate() error = %v, want inert without a Reproduction Record", err)
	}
}

func TestFixerReproductionGateFailsOnATamperedReproduction(t *testing.T) {
	t.Parallel()
	repos := newFixerGateRepos(t)
	worktree := t.TempDir()
	seedFixerReproduction(t, repos, worktree, "run-reproduction")
	if err := os.WriteFile(filepath.Join(worktree, "bug_test.go"), []byte("t.Skip()"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runner := New(Options{Repos: repos})
	input := stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Repo: "acme/looper", Loop: fixerGateLoop(t, repos)}

	err := runner.enforceReproductionGate(context.Background(), input, worktree)
	if err == nil {
		t.Fatalf("enforceReproductionGate() error = nil, want a tamper failure")
	}
	loopErr, ok := err.(*loopError)
	if !ok || loopErr.kind != FailureManualIntervention {
		t.Fatalf("enforceReproductionGate() error = %#v, want a manual-intervention loopError", err)
	}
	if !strings.Contains(loopErr.message, string(reproduction.ReasonTestModified)) {
		t.Fatalf("message = %q, want the distinct tamper reason", loopErr.message)
	}
}

// Fixer learns which record governs it from the manifest and pins the Issue, so
// deleting the manifest on a later pass cannot escape the gate.
func TestFixerPinsTheGovernedIssueSoManifestDeletionCannotEscapeTheGate(t *testing.T) {
	t.Parallel()
	repos := newFixerGateRepos(t)
	worktree := t.TempDir()
	seedFixerReproduction(t, repos, worktree, "run-reproduction")
	// Tampered up front so the gate resolves and pins without executing the
	// repository-supplied command.
	if err := os.WriteFile(filepath.Join(worktree, "bug_test.go"), []byte("t.Skip()"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runner := New(Options{Repos: repos})
	loop := fixerGateLoop(t, repos)
	input := stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Repo: "acme/looper", Loop: loop}

	// First pass fails on the tampered reproduction, and pins the Issue.
	if err := runner.enforceReproductionGate(context.Background(), input, worktree); err == nil {
		t.Fatalf("enforceReproductionGate() error = nil, want the tampered reproduction to fail")
	}
	stored, err := repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil || stored == nil {
		t.Fatalf("Loops.GetByID() = %v, %v", stored, err)
	}
	metadata := map[string]any{}
	if stored.MetadataJSON != nil {
		if err := json.Unmarshal([]byte(*stored.MetadataJSON), &metadata); err != nil {
			t.Fatalf("decode loop metadata: %v", err)
		}
	}
	if reproduction.GovernedIssueNumber(metadata) != 41 {
		t.Fatalf("loop metadata = %#v, want the governing Issue pinned", metadata)
	}

	// Second pass with the manifest removed still gates, via the pin.
	if err := os.Remove(reproduction.ManifestPath(worktree)); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	input.Loop = *stored
	if err := runner.enforceReproductionGate(context.Background(), input, worktree); err == nil {
		t.Fatalf("enforceReproductionGate() error = nil after manifest deletion, want the gate to still apply")
	}
}
