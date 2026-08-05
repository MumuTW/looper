package storage

import (
	"context"
	"strings"
	"testing"
)

// checkpoint_json is arbitrary text, and both merge helpers use SQLite's json_set on
// it. That function has two distinct failure modes on non-object input: malformed
// content aborts the statement, and valid-but-non-object content (null, an array, a
// scalar) is returned unchanged while the UPDATE still reports a row affected -- a
// silent no-op. The Go read-modify-write path these helpers replaced normalized both
// to an empty checkpoint, so they have to as well.

func mergeFixture(t *testing.T) *Repositories {
	t.Helper()
	return NewRepositories(openMigratedCoordinatorForRepositories(t).DB())
}

func seedRunWithCheckpoint(t *testing.T, repos *Repositories, id, checkpointJSON string) {
	t.Helper()
	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(context.Background(), ProjectRecord{ID: "project_1", Name: "p", RepoPath: "/tmp/p", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	target := "pr:acme/looper:1"
	repo := "acme/looper"
	prNumber := int64(1)
	if err := repos.Loops.Upsert(context.Background(), LoopRecord{ID: "loop_" + id, Seq: 1, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &prNumber, Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	record := RunRecord{ID: id, LoopID: "loop_" + id, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}
	record.CheckpointJSON = &checkpointJSON
	if err := repos.Runs.Upsert(context.Background(), record); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
}

func storedCheckpoint(t *testing.T, repos *Repositories, id string) string {
	t.Helper()
	run, err := repos.Runs.GetByID(context.Background(), id)
	if err != nil || run == nil || run.CheckpointJSON == nil {
		t.Fatalf("Runs.GetByID(%s) = (%#v, %v)", id, run, err)
	}
	return *run.CheckpointJSON
}

func TestMergeRunResumePolicyNormalizesNonObjectCheckpoints(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ name, stored string }{
		{name: "malformed", stored: `{"resumePolicy":`},
		{name: "json null", stored: `null`},
		{name: "array", stored: `[1,2]`},
		{name: "scalar", stored: `7`},
		{name: "empty", stored: ``},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			repos := mergeFixture(t)
			id := "run_policy_" + testCase.name
			seedRunWithCheckpoint(t, repos, id, testCase.stored)

			if err := repos.Runs.MergeRunResumePolicy(context.Background(), id, "restart_from_discover", "2026-04-11T13:00:00.000Z"); err != nil {
				t.Fatalf("MergeRunResumePolicy() error = %v", err)
			}
			if got := storedCheckpoint(t, repos, id); got != `{"resumePolicy":"restart_from_discover"}` {
				t.Fatalf("checkpoint = %s, want the policy written onto a normalized object", got)
			}
		})
	}
}

func TestMergeRunResumePolicyPreservesOtherFields(t *testing.T) {
	t.Parallel()
	repos := mergeFixture(t)
	seedRunWithCheckpoint(t, repos, "run_policy_object", `{"resumePolicy":"advance_from_checkpoint","fixItemsHash":"h1"}`)

	if err := repos.Runs.MergeRunResumePolicy(context.Background(), "run_policy_object", "restart_from_discover", "2026-04-11T13:00:00.000Z"); err != nil {
		t.Fatalf("MergeRunResumePolicy() error = %v", err)
	}
	got := storedCheckpoint(t, repos, "run_policy_object")
	if !strings.Contains(got, `"resumePolicy":"restart_from_discover"`) || !strings.Contains(got, `"fixItemsHash":"h1"`) {
		t.Fatalf("checkpoint = %s, want the policy replaced and other fields kept", got)
	}
}

func TestClearTimeoutProgressSupersedesInterruptedObservation(t *testing.T) {
	t.Parallel()
	repos := mergeFixture(t)
	seedRunWithCheckpoint(t, repos, "run_timeout_observing", `{"execution":{"status":"timeout_observing","progressBeforeTimeout":{"headSha":"before"},"progressSnapshotError":"capture interrupted"},"continuation":{"mode":"timeout_observed","outcome":"observation failed","beforeTimeout":{"headSha":"before"}},"work":{"title":"keep"}}`)
	if err := repos.Runs.ClearTimeoutProgress(context.Background(), "run_timeout_observing", "2026-04-11T13:00:00.000Z"); err != nil {
		t.Fatalf("ClearTimeoutProgress() error = %v", err)
	}
	got := storedCheckpoint(t, repos, "run_timeout_observing")
	if strings.Contains(got, "timeout_observing") || strings.Contains(got, "progressBeforeTimeout") || strings.Contains(got, "progressSnapshotError") || strings.Contains(got, "continuation") {
		t.Fatalf("checkpoint = %s, want interrupted observation superseded", got)
	}
	if !strings.Contains(got, `"status":"timeout"`) || !strings.Contains(got, `"title":"keep"`) {
		t.Fatalf("checkpoint = %s, want timeout status and unrelated fields preserved", got)
	}
}

func TestMergeWorktreeCleanupTimestampsNormalizesNonObjectCheckpoints(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ name, stored string }{
		{name: "malformed", stored: `{"worktree":`},
		{name: "json null", stored: `null`},
		{name: "array", stored: `[]`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			repos := mergeFixture(t)
			id := "run_cleanup_" + testCase.name
			seedRunWithCheckpoint(t, repos, id, testCase.stored)

			if err := repos.Runs.MergeWorktreeCleanupTimestamps(context.Background(), id, "2026-04-11T13:00:00.000Z", "", "2026-04-11T13:00:00.000Z"); err != nil {
				t.Fatalf("MergeWorktreeCleanupTimestamps() error = %v", err)
			}
			got := storedCheckpoint(t, repos, id)
			if !strings.Contains(got, `"cleanupAttemptedAt":"2026-04-11T13:00:00.000Z"`) {
				t.Fatalf("checkpoint = %s, want the attempt recorded on a normalized object", got)
			}
		})
	}
}
