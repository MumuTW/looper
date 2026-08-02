package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIssueClaimAdmissionSerializesWorkerAndPRLoopPublication(t *testing.T) {
	t.Parallel()

	for _, first := range []string{"worker", "reviewer"} {
		t.Run(first+"-first", func(t *testing.T) {
			coordinator := openMigratedCoordinatorForRepositories(t)
			ctx := context.Background()
			startedAt := "2026-07-31T00:00:00.000Z"
			repo := "acme/looper"
			if err := NewRepositories(coordinator.DB()).Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			prNumber := int64(77)
			prTarget := "pr:acme/looper:77"
			metadata := `{"worker":{"repo":"acme/looper","issueNumber":19}}`
			if err := NewRepositories(coordinator.DB()).Loops.Upsert(ctx, LoopRecord{ID: "source_worker", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
				t.Fatalf("insert source worker: %v", err)
			}

			issueTarget := "issue:acme/looper:19"
			worker := LoopRecord{ID: "new_worker", ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &issueTarget, Repo: &repo, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt}
			reviewer := LoopRecord{ID: "new_reviewer", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt}
			publish := func(record LoopRecord) error {
				return WithTransaction(ctx, coordinator.DB(), nil, func(tx *sql.Tx) error {
					repos := NewRepositories(tx)
					if err := repos.Loops.AssertIssueClaimAdmission(ctx, record, false); err != nil {
						return err
					}
					seq, err := repos.Loops.AllocateSeq(ctx)
					if err != nil {
						return err
					}
					record.Seq = seq
					return repos.Loops.Upsert(ctx, record)
				})
			}

			var second LoopRecord
			if first == "worker" {
				if err := publish(worker); err != nil {
					t.Fatalf("publish worker first: %v", err)
				}
				second = reviewer
			} else {
				if err := publish(reviewer); err != nil {
					t.Fatalf("publish reviewer first: %v", err)
				}
				second = worker
			}
			secondErr := publish(second)
			if _, ok := IsIssueClaimConflictError(secondErr); !ok {
				err := secondErr
				t.Fatalf("second publication error = %v, want source-issue conflict", err)
			}
		})
	}
}

func TestParseIssueTargetIDNormalizesWhitespace(t *testing.T) {
	repo, number, ok := parseIssueTargetID("  issue: acme/looper :19  ")
	if !ok || repo != "acme/looper" || number != 19 {
		t.Fatalf("parseIssueTargetID() = (%q, %d, %v), want (acme/looper, 19, true)", repo, number, ok)
	}
}

func TestIssueClaimAdmissionKeepsPersistedPRSourceWorkerIdentity(t *testing.T) {
	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	startedAt := "2026-04-11T12:00:00.000Z"
	repo := "acme/looper"
	prNumber := int64(77)
	prTarget := "pr:acme/looper:77"
	issueA := "issue:acme/looper:19"
	issueB := "issue:acme/looper:20"
	workerA := `{"worker":{"repo":"acme/looper","issueNumber":19}}`
	workerB := `{"worker":{"repo":"acme/looper","issueNumber":20}}`
	reviewerA := `{"sourceWorkerId":"worker_a"}`
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for _, record := range []LoopRecord{
		{ID: "worker_a", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &workerA, CreatedAt: startedAt, UpdatedAt: startedAt},
		{ID: "reviewer_a", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &reviewerA, CreatedAt: startedAt, UpdatedAt: startedAt},
		{ID: "worker_b", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &workerB, CreatedAt: startedAt, UpdatedAt: "2026-04-11T12:01:00.000Z"},
	} {
		if err := repos.Loops.Upsert(ctx, record); err != nil {
			t.Fatalf("seed %s: %v", record.ID, err)
		}
	}
	candidate := LoopRecord{ID: "new_worker_a", Seq: 4, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &issueA, Repo: &repo, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt}
	if err := repos.Loops.Upsert(ctx, candidate); err == nil {
		t.Fatal("Upsert() error = nil, want source issue conflict")
	} else if conflict, ok := IsIssueClaimConflictError(err); !ok || conflict.LoopID != "reviewer_a" {
		t.Fatalf("Upsert() error = %v, want reviewer_a source issue conflict", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "new_worker_b", Seq: 5, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &issueB, Repo: &repo, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Upsert(issue B) error = %v, want no conflict for distinct source issue", err)
	}
}

func TestIssueClaimAdmissionConcurrentWritersPublishOnlyOne(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	startedAt := "2026-07-31T00:00:00.000Z"
	repo := "acme/looper"
	if err := NewRepositories(coordinator.DB()).Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	prNumber := int64(77)
	prTarget := "pr:acme/looper:77"
	metadata := `{"worker":{"repo":"acme/looper","issueNumber":19}}`
	if err := NewRepositories(coordinator.DB()).Loops.Upsert(ctx, LoopRecord{ID: "source_worker", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
		t.Fatalf("insert source worker: %v", err)
	}
	issueTarget := "issue:acme/looper:19"
	workers := []LoopRecord{
		{ID: "new_worker", ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &issueTarget, Repo: &repo, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt},
		{ID: "new_reviewer", ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt},
	}
	start := make(chan struct{})
	errs := make(chan error, len(workers))
	for _, candidate := range workers {
		go func(record LoopRecord) {
			<-start
			errs <- WithTransaction(ctx, coordinator.DB(), nil, func(tx *sql.Tx) error {
				repos := NewRepositories(tx)
				if err := repos.Loops.AssertIssueClaimAdmission(ctx, record, false); err != nil {
					return err
				}
				seq, err := repos.Loops.AllocateSeq(ctx)
				if err != nil {
					return err
				}
				record.Seq = seq
				return repos.Loops.Upsert(ctx, record)
			})
		}(candidate)
	}
	close(start)

	successes := 0
	conflicts := 0
	for range workers {
		err := <-errs
		if err == nil {
			successes++
		} else if _, ok := IsIssueClaimConflictError(err); ok {
			conflicts++
		} else {
			t.Fatalf("concurrent publication error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent publications = %d successes, %d conflicts; want one of each", successes, conflicts)
	}
}

func TestLoopUpsertGuardsReactivationAndRetargetedWorkerIssueClaims(t *testing.T) {
	t.Parallel()

	t.Run("reactivation", func(t *testing.T) {
		coordinator := openMigratedCoordinatorForRepositories(t)
		repos := NewRepositories(coordinator.DB())
		ctx := context.Background()
		startedAt := "2026-07-31T00:00:00.000Z"
		repo := "acme/looper"
		prNumber := int64(77)
		prTarget := "pr:acme/looper:77"
		issueTarget := "issue:acme/looper:19"
		metadata := `{"worker":{"repo":"acme/looper","issueNumber":19}}`
		if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
			t.Fatalf("seed project: %v", err)
		}

		for _, record := range []LoopRecord{
			{ID: "source_worker", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "completed", MetadataJSON: &metadata, CreatedAt: startedAt, UpdatedAt: startedAt},
			{ID: "inactive_reviewer", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "failed", CreatedAt: startedAt, UpdatedAt: startedAt},
			{ID: "active_worker", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &issueTarget, Repo: &repo, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt},
		} {
			if err := repos.Loops.Upsert(ctx, record); err != nil {
				t.Fatalf("seed %s: %v", record.ID, err)
			}
		}

		reviewer, err := repos.Loops.GetByID(ctx, "inactive_reviewer")
		if err != nil || reviewer == nil {
			t.Fatalf("load inactive reviewer: %v, %#v", err, reviewer)
		}
		reviewer.Status = "queued"
		reviewer.UpdatedAt = "2026-07-31T00:01:00.000Z"
		if err := repos.Loops.Upsert(ctx, *reviewer); err == nil {
			t.Fatal("reactivating reviewer error = nil, want source-issue conflict")
		} else if conflict, ok := IsIssueClaimConflictError(err); !ok || conflict.LoopID != "active_worker" {
			t.Fatalf("reactivating reviewer error = %v, want active worker conflict", err)
		}
		persisted, err := repos.Loops.GetByID(ctx, "inactive_reviewer")
		if err != nil || persisted == nil || persisted.Status != "failed" {
			t.Fatalf("reactivated reviewer = %#v, %v; want failed record unchanged", persisted, err)
		}
	})

	t.Run("retargeted worker", func(t *testing.T) {
		coordinator := openMigratedCoordinatorForRepositories(t)
		repos := NewRepositories(coordinator.DB())
		ctx := context.Background()
		startedAt := "2026-07-31T00:00:00.000Z"
		repo := "acme/looper"
		prNumber := int64(77)
		prTarget := "pr:acme/looper:77"
		issueTarget := "issue:acme/looper:19"
		metadata := `{"worker":{"repo":"acme/looper","issueNumber":19}}`
		if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		retargeted := LoopRecord{ID: "retargeted_worker", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &prTarget, Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: startedAt, UpdatedAt: startedAt}
		if err := repos.Loops.Upsert(ctx, retargeted); err != nil {
			t.Fatalf("seed retargeted worker: %v", err)
		}
		candidate := LoopRecord{ID: "new_worker", Seq: 2, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &issueTarget, Repo: &repo, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt}
		if err := repos.Loops.Upsert(ctx, candidate); err == nil {
			t.Fatal("new issue worker error = nil, want retargeted worker conflict")
		} else if conflict, ok := IsIssueClaimConflictError(err); !ok || conflict.LoopID != retargeted.ID {
			t.Fatalf("new issue worker error = %v, want retargeted worker conflict", err)
		}
	})

	t.Run("active loop changes source issue", func(t *testing.T) {
		coordinator := openMigratedCoordinatorForRepositories(t)
		repos := NewRepositories(coordinator.DB())
		ctx := context.Background()
		startedAt := "2026-07-31T00:00:00.000Z"
		repo := "acme/looper"
		claimPRNumber := int64(77)
		claimPRTarget := "pr:acme/looper:77"
		movingIssueTarget := "issue:acme/looper:19"
		movingPRNumber := int64(99)
		movingPRTarget := "pr:acme/looper:99"
		claimMetadata := `{"worker":{"repo":"acme/looper","issueNumber":20}}`
		movingMetadata := `{"worker":{"repo":"acme/looper","issueNumber":19}}`
		if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		for _, record := range []LoopRecord{
			{ID: "claim_source", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &claimPRTarget, Repo: &repo, PRNumber: &claimPRNumber, Status: "completed", MetadataJSON: &claimMetadata, CreatedAt: startedAt, UpdatedAt: startedAt},
			{ID: "claim_fixer", Seq: 2, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &claimPRTarget, Repo: &repo, PRNumber: &claimPRNumber, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt},
			{ID: "moving_worker", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "issue", TargetID: &movingIssueTarget, Repo: &repo, Status: "queued", MetadataJSON: &movingMetadata, CreatedAt: startedAt, UpdatedAt: startedAt},
		} {
			if err := repos.Loops.Upsert(ctx, record); err != nil {
				t.Fatalf("seed %s: %v", record.ID, err)
			}
		}

		moving, err := repos.Loops.GetByID(ctx, "moving_worker")
		if err != nil || moving == nil {
			t.Fatalf("load moving worker: %v, %#v", err, moving)
		}
		moving.TargetType = "pull_request"
		moving.TargetID = &movingPRTarget
		moving.PRNumber = &movingPRNumber
		moving.MetadataJSON = &claimMetadata
		moving.UpdatedAt = "2026-07-31T00:01:00.000Z"
		if err := repos.Loops.Upsert(ctx, *moving); err == nil {
			t.Fatal("retarget active worker error = nil, want source-issue conflict")
		} else if conflict, ok := IsIssueClaimConflictError(err); !ok || conflict.LoopID != "claim_fixer" {
			t.Fatalf("retarget active worker error = %v, want claim_fixer conflict", err)
		}
		persisted, err := repos.Loops.GetByID(ctx, "moving_worker")
		if err != nil || persisted == nil || persisted.TargetType != "issue" || loopString(persisted.TargetID) != movingIssueTarget {
			t.Fatalf("moving worker after rejected retarget = %#v, %v; want original issue target", persisted, err)
		}
	})

	t.Run("forced worker authorizes PR descendants", func(t *testing.T) {
		coordinator := openMigratedCoordinatorForRepositories(t)
		repos := NewRepositories(coordinator.DB())
		ctx := context.Background()
		startedAt := "2026-07-31T00:00:00.000Z"
		repo := "acme/looper"
		firstPRNumber := int64(77)
		firstPRTarget := "pr:acme/looper:77"
		secondPRNumber := int64(78)
		secondPRTarget := "pr:acme/looper:78"
		claimMetadata := `{"worker":{"repo":"acme/looper","issueNumber":19}}`
		forcedMetadata := `{"worker":{"repo":"acme/looper","issueNumber":19,"issueClaimOverride":true}}`
		if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: t.TempDir(), CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		for _, record := range []LoopRecord{
			{ID: "first_source", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &firstPRTarget, Repo: &repo, PRNumber: &firstPRNumber, Status: "completed", MetadataJSON: &claimMetadata, CreatedAt: startedAt, UpdatedAt: startedAt},
			{ID: "first_fixer", Seq: 2, ProjectID: "project_1", Type: "fixer", TargetType: "pull_request", TargetID: &firstPRTarget, Repo: &repo, PRNumber: &firstPRNumber, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt},
		} {
			if err := repos.Loops.Upsert(ctx, record); err != nil {
				t.Fatalf("seed %s: %v", record.ID, err)
			}
		}
		if err := repos.Loops.UpsertForcingIssueClaimAdmission(ctx, LoopRecord{ID: "forced_source", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "pull_request", TargetID: &secondPRTarget, Repo: &repo, PRNumber: &secondPRNumber, Status: "queued", MetadataJSON: &forcedMetadata, CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
			t.Fatalf("seed forced source worker: %v", err)
		}
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "forced_reviewer", Seq: 4, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", TargetID: &secondPRTarget, Repo: &repo, PRNumber: &secondPRNumber, Status: "queued", CreatedAt: startedAt, UpdatedAt: startedAt}); err != nil {
			t.Fatalf("publish forced worker descendant: %v", err)
		}
	})
}

func TestAgentExecutionTerminalObservationCannotRegressToActive(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	ctx := context.Background()
	startedAt := "2026-07-16T12:00:00.000Z"
	endedAt := "2026-07-16T12:01:00.000Z"
	terminal := AgentExecutionRecord{
		ID: "agent_terminal_monotonic", Vendor: "codex", Status: "completed",
		StartedAt: startedAt, EndedAt: &endedAt, CreatedAt: startedAt, UpdatedAt: endedAt,
	}
	if err := repos.AgentExecutions.Upsert(ctx, terminal); err != nil {
		t.Fatalf("Upsert(terminal) error = %v", err)
	}

	staleLive := terminal
	staleLive.Status = "running"
	staleLive.EndedAt = nil
	staleLive.UpdatedAt = startedAt
	err := repos.AgentExecutions.Upsert(ctx, staleLive)
	if !errors.Is(err, ErrAgentExecutionConflict) {
		t.Fatalf("Upsert(stale live) error = %v, want ErrAgentExecutionConflict", err)
	}

	got, err := repos.AgentExecutions.GetByID(ctx, terminal.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got == nil || got.Status != "completed" || got.EndedAt == nil {
		t.Fatalf("execution = %#v, want completed terminal observation", got)
	}
}

func TestWorktreesAdoptPathRollsBackRetirementWhenAdoptionFails(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-30T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project", Name: "Project", RepoPath: "/tmp/repo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	attached := WorktreeRecord{ID: "attached", ProjectID: "project", RepoPath: "/tmp/repo", WorktreePath: "/tmp/attached", Branch: "feature/fixer", Status: "active", CreatedAt: now, UpdatedAt: now}
	shared := WorktreeRecord{ID: "shared", ProjectID: "project", RepoPath: "/tmp/repo", WorktreePath: "/tmp/shared", Branch: "reviewer/pr-42", Status: "active", CreatedAt: now, UpdatedAt: now}
	for _, record := range []WorktreeRecord{attached, shared} {
		if err := repos.Worktrees.Upsert(ctx, record); err != nil {
			t.Fatalf("Worktrees.Upsert(%q) error = %v", record.ID, err)
		}
	}
	if _, err := coordinator.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_shared_path_adoption
		BEFORE UPDATE ON worktrees WHEN OLD.id = 'shared'
		BEGIN SELECT RAISE(ABORT, 'adoption blocked'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	adopting := shared
	adopting.Branch = "feature/fixer"
	adopting.UpdatedAt = "2026-07-30T12:01:00.000Z"
	if err := repos.Worktrees.AdoptPath(ctx, adopting); err == nil || !strings.Contains(err.Error(), "adoption blocked") {
		t.Fatalf("AdoptPath() error = %v, want blocked adoption", err)
	}
	storedAttached, err := repos.Worktrees.GetByID(ctx, attached.ID)
	if err != nil || storedAttached == nil || storedAttached.Branch != "feature/fixer" {
		t.Fatalf("attached row after failed adoption = %#v, %v; want original branch", storedAttached, err)
	}
	storedShared, err := repos.Worktrees.GetByID(ctx, shared.ID)
	if err != nil || storedShared == nil || storedShared.Branch != "reviewer/pr-42" {
		t.Fatalf("shared row after failed adoption = %#v, %v; want original branch", storedShared, err)
	}
}

func TestWorktreesGetByPathAcceptsPrivateVarAlias(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("/var and /private/var are distinct paths off macOS")
	}

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-30T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project", Name: "Project", RepoPath: "/tmp/repo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	record := WorktreeRecord{ID: "private-var", ProjectID: "project", RepoPath: "/tmp/repo", WorktreePath: "/private/var/folders/looper/worktree", Branch: "feature/fixer", Status: "active", CreatedAt: now, UpdatedAt: now}
	if err := repos.Worktrees.Upsert(ctx, record); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}
	got, err := repos.Worktrees.GetByPath(ctx, "/var/folders/looper/worktree")
	if err != nil || got == nil || got.ID != record.ID {
		t.Fatalf("GetByPath(/var alias) = %#v, %v; want %q", got, err, record.ID)
	}
}

func TestAgentExecutionTerminalToDifferentTerminalIsConflict(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	ctx := context.Background()
	startedAt := "2026-07-16T12:00:00.000Z"
	endedAt := "2026-07-16T12:01:00.000Z"
	terminal := AgentExecutionRecord{
		ID: "agent_terminal_to_terminal", Vendor: "codex", Status: "completed",
		StartedAt: startedAt, EndedAt: &endedAt, CreatedAt: startedAt, UpdatedAt: endedAt,
	}
	if err := repos.AgentExecutions.Upsert(ctx, terminal); err != nil {
		t.Fatalf("Upsert(completed) error = %v", err)
	}

	otherTerminal := terminal
	otherTerminal.Status = "killed"
	otherTerminal.UpdatedAt = "2026-07-16T12:02:00.000Z"
	err := repos.AgentExecutions.Upsert(ctx, otherTerminal)
	if !errors.Is(err, ErrAgentExecutionConflict) {
		t.Fatalf("Upsert(killed over completed) error = %v, want ErrAgentExecutionConflict", err)
	}

	got, err := repos.AgentExecutions.GetByID(ctx, terminal.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got == nil || got.Status != "completed" {
		t.Fatalf("status = %q, want completed (first terminal wins)", got.Status)
	}
}

func TestAgentExecutionSameTerminalFieldEnrichmentAllowed(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	ctx := context.Background()
	startedAt := "2026-07-16T12:00:00.000Z"
	endedAt := "2026-07-16T12:01:00.000Z"
	terminal := AgentExecutionRecord{
		ID: "agent_terminal_enrich", Vendor: "codex", Status: "completed",
		StartedAt: startedAt, EndedAt: &endedAt, CreatedAt: startedAt, UpdatedAt: endedAt,
	}
	if err := repos.AgentExecutions.Upsert(ctx, terminal); err != nil {
		t.Fatalf("Upsert(completed) error = %v", err)
	}

	enriched := terminal
	resumeStatus := "failed"
	resumeErr := "native resume unavailable"
	enriched.NativeResumeStatus = &resumeStatus
	enriched.NativeResumeError = &resumeErr
	enriched.UpdatedAt = "2026-07-16T12:02:00.000Z"
	if err := repos.AgentExecutions.Upsert(ctx, enriched); err != nil {
		t.Fatalf("Upsert(same terminal enrichment) error = %v", err)
	}

	got, err := repos.AgentExecutions.GetByID(ctx, terminal.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got == nil || got.Status != "completed" {
		t.Fatalf("status = %#v, want completed", got)
	}
	if got.NativeResumeStatus == nil || *got.NativeResumeStatus != "failed" {
		t.Fatalf("NativeResumeStatus = %#v, want failed", got.NativeResumeStatus)
	}
}

func TestAgentExecutionActiveToTerminalAndCancellingAllowed(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	ctx := context.Background()
	startedAt := "2026-07-16T12:00:00.000Z"
	live := AgentExecutionRecord{
		ID: "agent_active_transitions", Vendor: "codex", Status: "running",
		StartedAt: startedAt, CreatedAt: startedAt, UpdatedAt: startedAt,
	}
	if err := repos.AgentExecutions.Upsert(ctx, live); err != nil {
		t.Fatalf("Upsert(running) error = %v", err)
	}

	cancelling := live
	cancelling.Status = "cancelling"
	cancelling.UpdatedAt = "2026-07-16T12:00:30.000Z"
	if err := repos.AgentExecutions.Upsert(ctx, cancelling); err != nil {
		t.Fatalf("Upsert(cancelling) error = %v", err)
	}

	endedAt := "2026-07-16T12:01:00.000Z"
	killed := cancelling
	killed.Status = "killed"
	killed.EndedAt = &endedAt
	killed.UpdatedAt = endedAt
	if err := repos.AgentExecutions.Upsert(ctx, killed); err != nil {
		t.Fatalf("Upsert(killed) error = %v", err)
	}

	got, err := repos.AgentExecutions.GetByID(ctx, live.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got == nil || got.Status != "killed" {
		t.Fatalf("status = %#v, want killed", got)
	}
}

func TestRepositoriesRoundTripForProjectsLoopsRunsAndRuntimeMetadata(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	mainBranch := "main"
	baseBranch := "main"
	projectMeta := `{"tier":"mvp"}`
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:           "project_1",
		Name:         "Looper",
		RepoPath:     "/tmp/looper",
		BaseBranch:   &baseBranch,
		Archived:     false,
		MetadataJSON: &projectMeta,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	targetID := "pr:42"
	repo := "acme/looper"
	prNumber := int64(42)
	config := `{"priority":"normal"}`
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID:         "loop_1",
		Seq:        1,
		ProjectID:  "project_1",
		Type:       "reviewer",
		TargetType: "pull_request",
		TargetID:   &targetID,
		Repo:       &repo,
		PRNumber:   &prNumber,
		Status:     "idle",
		ConfigJSON: &config,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	running := "running"
	if err := repos.Runs.Upsert(ctx, RunRecord{
		ID:              "run_1",
		LoopID:          "loop_1",
		Status:          running,
		StartedAt:       now,
		LastHeartbeatAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	agentCommand := `{"command":"codex","args":["exec","prompt"]}`
	agentCWD := "/tmp/looper"
	agentOutput := `{"stdout":"ok","stderr":""}`
	nativeSessionID := "codex-session-1"
	nativeResumeMode := "native_resume"
	nativeResumeStatus := "pending"
	pid := int64(12345)
	if err := repos.AgentExecutions.Upsert(ctx, AgentExecutionRecord{
		ID:                 "agent_1",
		RunID:              strPtr("run_1"),
		LoopID:             strPtr("loop_1"),
		ProjectID:          strPtr("project_1"),
		Vendor:             "codex",
		Status:             "running",
		PID:                &pid,
		CommandJSON:        &agentCommand,
		CWD:                &agentCWD,
		HeartbeatCount:     2,
		OutputJSON:         &agentOutput,
		NativeSessionID:    &nativeSessionID,
		NativeResumeMode:   &nativeResumeMode,
		NativeResumeStatus: &nativeResumeStatus,
		StartedAt:          now,
		LastHeartbeatAt:    &now,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}

	headSHA := "abc123"
	if err := repos.PullRequestSnapshots.Upsert(ctx, PullRequestSnapshotRecord{
		ID:         "snapshot_1",
		ProjectID:  "project_1",
		Repo:       repo,
		PRNumber:   prNumber,
		HeadSHA:    headSHA,
		CapturedAt: now,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("PullRequestSnapshots.Upsert() error = %v", err)
	}

	entityType := "loop"
	entityID := "loop_1"
	correlationID := "corr_1"
	causationID := "cause_1"
	actorType := "agent"
	actorID := "reviewer_1"
	actorDisplayName := "Reviewer"
	payloadJSON := `{"status":"idle"}`
	projectID := "project_1"
	runID := "run_1"
	loopID := "loop_1"
	if err := repos.Events.Append(ctx, EventLogRecord{
		ID:               "event_1",
		EventType:        "loop.created",
		ProjectID:        &projectID,
		LoopID:           &loopID,
		RunID:            &runID,
		EntityType:       &entityType,
		EntityID:         &entityID,
		CorrelationID:    &correlationID,
		CausationID:      &causationID,
		ActorType:        &actorType,
		ActorID:          &actorID,
		ActorDisplayName: &actorDisplayName,
		PayloadJSON:      payloadJSON,
		CreatedAt:        now,
	}); err != nil {
		t.Fatalf("Events.Append() error = %v", err)
	}

	notificationSubtitle := "runtime"
	notificationDedupe := "looperd.started:system:looperd"
	notificationPayload := `{"title":"Looper"}`
	if err := repos.Notifications.Upsert(ctx, NotificationRecord{
		ID:          "notification_1",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		RunID:       &runID,
		EntityType:  strPtr("notification"),
		EntityID:    strPtr("looperd.started"),
		Channel:     "in_app",
		Level:       "success",
		Title:       "Looper",
		Subtitle:    &notificationSubtitle,
		Body:        "Started",
		Status:      "success",
		DedupeKey:   &notificationDedupe,
		PayloadJSON: &notificationPayload,
		SentAt:      &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("Notifications.Upsert() error = %v", err)
	}

	lockReason := "reviewer"
	acquired, err := repos.Locks.Acquire(ctx, LockRecord{
		Key:       "pr:acme/looper:42",
		Owner:     "reviewer-loop",
		Reason:    &lockReason,
		ExpiresAt: "2026-04-11T12:05:00.000Z",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want true")
	}

	worktreePath := "/tmp/looper-worktrees/feature-loop-1"
	headWorktreeSHA := "def456"
	if err := repos.Worktrees.Upsert(ctx, WorktreeRecord{
		ID:           "wt_1",
		ProjectID:    "project_1",
		RepoPath:     "/tmp/looper",
		WorktreePath: worktreePath,
		Branch:       "feature/loop-1",
		BaseBranch:   &mainBranch,
		Status:       "active",
		HeadSHA:      &headWorktreeSHA,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Worktrees.Upsert() error = %v", err)
	}

	loopBySeq, err := repos.Loops.GetBySeq(ctx, 1)
	if err != nil {
		t.Fatalf("Loops.GetBySeq() error = %v", err)
	}
	if loopBySeq == nil || loopBySeq.ID != "loop_1" {
		t.Fatalf("Loops.GetBySeq() = %#v, want loop_1", loopBySeq)
	}

	latestRun, err := repos.Runs.GetLatestByLoopID(ctx, "loop_1")
	if err != nil {
		t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil || latestRun.ID != "run_1" {
		t.Fatalf("Runs.GetLatestByLoopID() = %#v, want run_1", latestRun)
	}

	agentExecution, err := repos.AgentExecutions.GetByID(ctx, "agent_1")
	if err != nil {
		t.Fatalf("AgentExecutions.GetByID() error = %v", err)
	}
	if agentExecution == nil || agentExecution.ID != "agent_1" || agentExecution.HeartbeatCount != 2 || agentExecution.NativeSessionID == nil || *agentExecution.NativeSessionID != nativeSessionID {
		t.Fatalf("AgentExecutions.GetByID() = %#v, want agent_1 with heartbeat_count=2 and native session", agentExecution)
	}

	latestExecution, err := repos.AgentExecutions.GetLatestByRunID(ctx, "run_1")
	if err != nil {
		t.Fatalf("AgentExecutions.GetLatestByRunID() error = %v", err)
	}
	if latestExecution == nil || latestExecution.ID != "agent_1" {
		t.Fatalf("AgentExecutions.GetLatestByRunID() = %#v, want agent_1", latestExecution)
	}

	activeExecutions, err := repos.AgentExecutions.ListActive(ctx)
	if err != nil {
		t.Fatalf("AgentExecutions.ListActive() error = %v", err)
	}
	if len(activeExecutions) != 1 || activeExecutions[0].ID != "agent_1" {
		t.Fatalf("AgentExecutions.ListActive() = %#v, want [agent_1]", activeExecutions)
	}

	snapshot, err := repos.PullRequestSnapshots.GetLatest(ctx, repo, prNumber)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.GetLatest() error = %v", err)
	}
	if snapshot == nil || snapshot.HeadSHA != headSHA {
		t.Fatalf("PullRequestSnapshots.GetLatest() = %#v, want headSha %q", snapshot, headSHA)
	}

	projectSnapshot, err := repos.PullRequestSnapshots.GetLatestByProject(ctx, "project_1", repo, prNumber)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.GetLatestByProject() error = %v", err)
	}
	if projectSnapshot == nil || projectSnapshot.HeadSHA != headSHA {
		t.Fatalf("PullRequestSnapshots.GetLatestByProject() = %#v, want headSha %q", projectSnapshot, headSHA)
	}

	events, err := repos.Events.ListByEntity(ctx, "loop", "loop_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 1 || events[0].ActorID == nil || *events[0].ActorID != "reviewer_1" {
		t.Fatalf("Events.ListByEntity() = %#v, want actorId reviewer_1", events)
	}

	lock, err := repos.Locks.Get(ctx, "pr:acme/looper:42")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock == nil || lock.Owner != "reviewer-loop" {
		t.Fatalf("Locks.Get() = %#v, want owner reviewer-loop", lock)
	}

	notifications, err := repos.Notifications.List(ctx, 10)
	if err != nil {
		t.Fatalf("Notifications.List() error = %v", err)
	}
	if len(notifications) != 1 || notifications[0].DedupeKey == nil || *notifications[0].DedupeKey != notificationDedupe {
		t.Fatalf("Notifications.List() = %#v, want notification with dedupe key %q", notifications, notificationDedupe)
	}

	latestNotification, err := repos.Notifications.GetLatestByDedupe(ctx, "in_app", notificationDedupe)
	if err != nil {
		t.Fatalf("Notifications.GetLatestByDedupe() error = %v", err)
	}
	if latestNotification == nil || latestNotification.ID != "notification_1" {
		t.Fatalf("Notifications.GetLatestByDedupe() = %#v, want notification_1", latestNotification)
	}

	worktree, err := repos.Worktrees.GetByBranch(ctx, "project_1", "feature/loop-1")
	if err != nil {
		t.Fatalf("Worktrees.GetByBranch() error = %v", err)
	}
	if worktree == nil || worktree.WorktreePath != worktreePath {
		t.Fatalf("Worktrees.GetByBranch() = %#v, want %q", worktree, worktreePath)
	}
	if worktree.BaseBranch == nil || *worktree.BaseBranch != mainBranch {
		t.Fatalf("Worktrees.GetByBranch().BaseBranch = %#v, want %q", worktree.BaseBranch, mainBranch)
	}
}

func TestPullRequestLookupsStayScopedAndReturnLatestSnapshotPerProject(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	const repo = "acme/looper"
	const prNumber int64 = 42

	for _, projectID := range []string{"github", "second", "unrelated"} {
		if err := repos.Projects.Upsert(ctx, ProjectRecord{
			ID:        projectID,
			Name:      projectID,
			RepoPath:  "/tmp/" + projectID,
			CreatedAt: "2026-07-13T09:00:00.000Z",
			UpdatedAt: "2026-07-13T09:00:00.000Z",
		}); err != nil {
			t.Fatalf("Projects.Upsert(%q) error = %v", projectID, err)
		}
	}

	snapshots := []PullRequestSnapshotRecord{
		{ID: "github-old", ProjectID: "github", Repo: repo, PRNumber: prNumber, HeadSHA: "github-old", CapturedAt: "2026-07-13T09:00:00.000Z", CreatedAt: "2026-07-13T09:00:00.000Z"},
		{ID: "github-new", ProjectID: "github", Repo: repo, PRNumber: prNumber, HeadSHA: "github-new", CapturedAt: "2026-07-13T10:00:00.000Z", CreatedAt: "2026-07-13T10:00:00.000Z"},
		{ID: "second-new", ProjectID: "second", Repo: repo, PRNumber: prNumber, HeadSHA: "second-new", CapturedAt: "2026-07-13T11:00:00.000Z", CreatedAt: "2026-07-13T11:00:00.000Z"},
		{ID: "other-pr", ProjectID: "unrelated", Repo: repo, PRNumber: 99, HeadSHA: "other-pr", CapturedAt: "2026-07-13T12:00:00.000Z", CreatedAt: "2026-07-13T12:00:00.000Z"},
		{ID: "other-repo", ProjectID: "unrelated", Repo: "acme/other", PRNumber: prNumber, HeadSHA: "other-repo", CapturedAt: "2026-07-13T13:00:00.000Z", CreatedAt: "2026-07-13T13:00:00.000Z"},
	}
	for _, snapshot := range snapshots {
		if err := repos.PullRequestSnapshots.Upsert(ctx, snapshot); err != nil {
			t.Fatalf("PullRequestSnapshots.Upsert(%q) error = %v", snapshot.ID, err)
		}
	}

	gotSnapshots, err := repos.PullRequestSnapshots.ListLatestByRepoAndPR(ctx, "Acme/Looper", prNumber)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.ListLatestByRepoAndPR() error = %v", err)
	}
	if got := snapshotIDs(gotSnapshots); !reflect.DeepEqual(got, []string{"second-new", "github-new"}) {
		t.Fatalf("PullRequestSnapshots.ListLatestByRepoAndPR() IDs = %v, want [second-new github-new]", got)
	}

	gotLatest, err := repos.PullRequestSnapshots.ListLatest(ctx)
	if err != nil {
		t.Fatalf("PullRequestSnapshots.ListLatest() error = %v", err)
	}
	if got := snapshotIDs(gotLatest); !reflect.DeepEqual(got, []string{"other-repo", "other-pr", "second-new", "github-new"}) {
		t.Fatalf("PullRequestSnapshots.ListLatest() IDs = %v, want one newest row per project/repo/PR", got)
	}

	matchingRepo := "Acme/Looper"
	matchingPR := prNumber
	otherPR := int64(99)
	for _, loop := range []LoopRecord{
		{ID: "github-loop", Seq: 1, ProjectID: "github", Type: "reviewer", TargetType: "pull_request", Repo: &matchingRepo, PRNumber: &matchingPR, Status: "idle", CreatedAt: "2026-07-13T09:00:00.000Z", UpdatedAt: "2026-07-13T09:00:00.000Z"},
		{ID: "other-loop", Seq: 2, ProjectID: "unrelated", Type: "reviewer", TargetType: "pull_request", Repo: &matchingRepo, PRNumber: &otherPR, Status: "idle", CreatedAt: "2026-07-13T09:00:00.000Z", UpdatedAt: "2026-07-13T09:00:00.000Z"},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%q) error = %v", loop.ID, err)
		}
	}

	gotLoops, err := repos.Loops.ListByRepoAndPR(ctx, repo, prNumber)
	if err != nil {
		t.Fatalf("Loops.ListByRepoAndPR() error = %v", err)
	}
	if len(gotLoops) != 1 || gotLoops[0].ID != "github-loop" {
		t.Fatalf("Loops.ListByRepoAndPR() = %#v, want github-loop", gotLoops)
	}
}

func snapshotIDs(records []PullRequestSnapshotRecord) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func TestRepositoriesStatusAggregates(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	for _, loop := range []LoopRecord{
		{ID: "loop_reviewer_running", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_reviewer_waiting", Seq: 2, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Status: "waiting", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_worker_failed", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	largeCheckpoint := make([]byte, 64*1024)
	for i := range largeCheckpoint {
		largeCheckpoint[i] = 'x'
	}
	checkpointJSON := string(largeCheckpoint)
	for _, run := range []RunRecord{
		{ID: "run_running", LoopID: "loop_reviewer_running", Status: "running", CheckpointJSON: &checkpointJSON, StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_failed", LoopID: "loop_worker_failed", Status: "failed", CheckpointJSON: &checkpointJSON, StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_completed", LoopID: "loop_reviewer_waiting", Status: "completed", CheckpointJSON: &checkpointJSON, StartedAt: now, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	for _, item := range []QueueItemRecord{
		{ID: "queue_queued", Type: "reviewer", TargetType: "pull_request", TargetID: "pr:1", DedupeKey: "reviewer:1", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_running", Type: "worker", TargetType: "project", TargetID: "project:1", DedupeKey: "worker:1", Priority: 1, Status: "running", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_failed", Type: "fixer", TargetType: "project", TargetID: "project:2", DedupeKey: "fixer:2", Priority: 1, Status: "failed", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	loopCounts, err := repos.Loops.CountByTypeAndStatus(ctx)
	if err != nil {
		t.Fatalf("Loops.CountByTypeAndStatus() error = %v", err)
	}
	if got := loopCounts["reviewer"]["running"]; got != 1 {
		t.Fatalf("reviewer/running loop count = %d, want 1", got)
	}
	if got := loopCounts["reviewer"]["waiting"]; got != 1 {
		t.Fatalf("reviewer/waiting loop count = %d, want 1", got)
	}
	if got := loopCounts["worker"]["failed"]; got != 1 {
		t.Fatalf("worker/failed loop count = %d, want 1", got)
	}

	runCounts, err := repos.Runs.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("Runs.CountByStatus() error = %v", err)
	}
	if got := runCounts["running"]; got != 1 {
		t.Fatalf("running run count = %d, want 1", got)
	}
	if got := runCounts["failed"]; got != 1 {
		t.Fatalf("failed run count = %d, want 1", got)
	}
	if got := runCounts["completed"]; got != 1 {
		t.Fatalf("completed run count = %d, want 1", got)
	}

	queueCounts, err := repos.Queue.CountByAllStatuses(ctx)
	if err != nil {
		t.Fatalf("Queue.CountByAllStatuses() error = %v", err)
	}
	if got := queueCounts["queued"]; got != 1 {
		t.Fatalf("queued queue count = %d, want 1", got)
	}
	if got := queueCounts["running"]; got != 1 {
		t.Fatalf("running queue count = %d, want 1", got)
	}
	if got := queueCounts["failed"]; got != 1 {
		t.Fatalf("failed queue count = %d, want 1", got)
	}
}

func TestRunsGetLatestByLoopIDBreaksStartedAtTiesByCreatedAt(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:        "project_latest_run",
		Name:      "Looper",
		RepoPath:  "/tmp/looper",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID:         "loop_latest_run",
		Seq:        1,
		ProjectID:  "project_latest_run",
		Type:       "reviewer",
		TargetType: "project",
		Status:     "idle",
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	startedAt := "2026-04-11T12:00:01.000Z"
	olderCreatedAt := "2026-04-11T12:00:02.000Z"
	newerCreatedAt := "2026-04-11T12:00:03.000Z"
	for _, run := range []RunRecord{
		{ID: "z_run_older", LoopID: "loop_latest_run", Status: "completed", StartedAt: startedAt, CreatedAt: olderCreatedAt, UpdatedAt: olderCreatedAt},
		{ID: "a_run_newer", LoopID: "loop_latest_run", Status: "running", StartedAt: startedAt, CreatedAt: newerCreatedAt, UpdatedAt: newerCreatedAt},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	latestRun, err := repos.Runs.GetLatestByLoopID(ctx, "loop_latest_run")
	if err != nil {
		t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
	}
	if latestRun == nil || latestRun.ID != "a_run_newer" {
		t.Fatalf("Runs.GetLatestByLoopID() = %#v, want a_run_newer", latestRun)
	}
}

func TestRunsLatestRunSelectionBreaksIdenticalTimestampsByInsertionOrder(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:        "project_run_seq",
		Name:      "Looper",
		RepoPath:  "/tmp/looper",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID:         "loop_run_seq",
		Seq:        1,
		ProjectID:  "project_run_seq",
		Type:       "fixer",
		TargetType: "pull_request",
		Status:     "failed",
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	// Fast retries within one millisecond: identical started_at AND created_at.
	// IDs descend lexically so the old id-based tie-break would pick the wrong
	// (first-inserted) attempt.
	startedAt := "2026-04-11T12:00:01.000Z"
	checkpoint := `{"resumePolicy":"on_failure"}`
	for _, runID := range []string{"z_attempt_1", "m_attempt_2", "a_attempt_3"} {
		if err := repos.Runs.Upsert(ctx, RunRecord{
			ID:             runID,
			LoopID:         "loop_run_seq",
			Status:         "failed",
			CheckpointJSON: &checkpoint,
			StartedAt:      startedAt,
			CreatedAt:      startedAt,
			UpdatedAt:      startedAt,
		}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", runID, err)
		}
	}

	// Re-upserting an older attempt must not reassign its insertion sequence.
	if err := repos.Runs.Upsert(ctx, RunRecord{
		ID:        "z_attempt_1",
		LoopID:    "loop_run_seq",
		Status:    "interrupted",
		StartedAt: startedAt,
		CreatedAt: startedAt,
		UpdatedAt: "2026-04-11T12:00:02.000Z",
	}); err != nil {
		t.Fatalf("Runs.Upsert(z_attempt_1 update) error = %v", err)
	}

	for attempt := 0; attempt < 5; attempt++ {
		latestRun, err := repos.Runs.GetLatestByLoopID(ctx, "loop_run_seq")
		if err != nil {
			t.Fatalf("Runs.GetLatestByLoopID() error = %v", err)
		}
		if latestRun == nil || latestRun.ID != "a_attempt_3" {
			t.Fatalf("Runs.GetLatestByLoopID() attempt %d = %#v, want a_attempt_3", attempt, latestRun)
		}

		batch, err := repos.Runs.ListLatestByLoopIDs(ctx, []string{"loop_run_seq"})
		if err != nil {
			t.Fatalf("Runs.ListLatestByLoopIDs() error = %v", err)
		}
		if len(batch) != 1 || batch[0].ID != "a_attempt_3" {
			t.Fatalf("Runs.ListLatestByLoopIDs() attempt %d = %#v, want [a_attempt_3]", attempt, batch)
		}

		byPolicy, err := repos.Runs.ListLatestByLoopStatusesAndResumePolicy(ctx, []string{"failed"}, "on_failure")
		if err != nil {
			t.Fatalf("Runs.ListLatestByLoopStatusesAndResumePolicy() error = %v", err)
		}
		if len(byPolicy) != 1 || byPolicy[0].ID != "a_attempt_3" {
			t.Fatalf("Runs.ListLatestByLoopStatusesAndResumePolicy() attempt %d = %#v, want [a_attempt_3]", attempt, byPolicy)
		}
	}
}

func TestLoopsAllocateSeqSeedsFromMaxWhenCounterMissing(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID:        "project_1",
		Name:      "Looper",
		RepoPath:  "/tmp/looper",
		Archived:  false,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_3", Seq: 3, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_3) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_7", Seq: 7, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_7) error = %v", err)
	}

	if _, err := coordinator.DB().ExecContext(ctx, `DELETE FROM counters WHERE name = 'loop_seq'`); err != nil {
		t.Fatalf("DELETE counters error = %v", err)
	}

	seq1, err := repos.Loops.AllocateSeq(ctx)
	if err != nil {
		t.Fatalf("Loops.AllocateSeq() first error = %v", err)
	}
	if seq1 != 8 {
		t.Fatalf("Loops.AllocateSeq() first = %d, want 8", seq1)
	}

	seq2, err := repos.Loops.AllocateSeq(ctx)
	if err != nil {
		t.Fatalf("Loops.AllocateSeq() second error = %v", err)
	}
	if seq2 != 9 {
		t.Fatalf("Loops.AllocateSeq() second = %d, want 9", seq2)
	}
}

func TestRepositoriesRollbackTransactionalWrites(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	wantErr := errors.New("abort transaction")
	err := coordinator.WithTransaction(ctx, func(tx *sql.Tx) error {
		txRepos := NewRepositories(tx)
		if upsertErr := txRepos.Loops.Upsert(ctx, LoopRecord{ID: "loop_rollback", Seq: 1, ProjectID: "project_1", Type: "worker", TargetType: "project", Status: "queued", CreatedAt: now, UpdatedAt: now}); upsertErr != nil {
			return upsertErr
		}
		entityType := "loop"
		entityID := "loop_rollback"
		projectID := "project_1"
		if appendErr := txRepos.Events.Append(ctx, EventLogRecord{ID: "event_loop_rollback", EventType: "loop.created", ProjectID: &projectID, EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: now}); appendErr != nil {
			return appendErr
		}

		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTransaction() error = %v, want %v", err, wantErr)
	}

	got, err := repos.Loops.GetByID(ctx, "loop_rollback")
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if got != nil {
		t.Fatalf("Loops.GetByID(loop_rollback) = %#v, want nil", got)
	}

	events, err := repos.Events.ListByEntity(ctx, "loop", "loop_rollback")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("Events.ListByEntity(loop_rollback) = %#v, want none", events)
	}
}

func TestLocksAcquireRequiresExpiryBeforeReplacement(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	repos.Locks.SetNow(func() time.Time {
		return time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	})

	now := "2026-04-11T12:00:00.000Z"
	reason := "initial"
	acquired, err := repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-a",
		Reason:    &reason,
		ExpiresAt: "2026-04-11T12:05:00.000Z",
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(initial) error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire(initial) = false, want true")
	}

	replacementReason := "takeover"
	acquired, err = repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-b",
		Reason:    &replacementReason,
		ExpiresAt: "2026-04-11T12:20:00.000Z",
		CreatedAt: "2026-04-11T12:01:00.000Z",
		UpdatedAt: "2026-04-11T12:01:00.000Z",
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(replacement blocked) error = %v", err)
	}
	if acquired {
		t.Fatal("Locks.Acquire(replacement blocked) = true, want false")
	}

	repos.Locks.SetNow(func() time.Time {
		return time.Date(2026, time.April, 11, 12, 10, 0, 0, time.UTC)
	})
	acquired, err = repos.Locks.Acquire(ctx, LockRecord{
		Key:       "task:123",
		Owner:     "worker-b",
		Reason:    &replacementReason,
		ExpiresAt: "2026-04-11T12:20:00.000Z",
		CreatedAt: "2026-04-11T12:10:00.000Z",
		UpdatedAt: "2026-04-11T12:10:00.000Z",
	})
	if err != nil {
		t.Fatalf("Locks.Acquire(replacement after expiry) error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire(replacement after expiry) = false, want true")
	}

	lock, err := repos.Locks.Get(ctx, "task:123")
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock == nil || lock.Owner != "worker-b" {
		t.Fatalf("Locks.Get() = %#v, want owner worker-b", lock)
	}
}

func TestEventsListOrdersAndDefaults(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	entityType := "run"
	entityID := "run_1"
	for _, event := range []EventLogRecord{
		{ID: "event_1", EventType: "worker.started", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"step":1}`, CreatedAt: "2026-04-11T12:00:00.000Z"},
		{ID: "event_2", EventType: "worker.progress", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"step":2}`, CreatedAt: "2026-04-11T12:01:00.000Z"},
		{ID: "event_3", EventType: "worker.completed", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{"step":3}`, CreatedAt: "2026-04-11T12:02:00.000Z"},
	} {
		if err := repos.Events.Append(ctx, event); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", event.ID, err)
		}
	}

	listed, err := repos.Events.List(ctx, 2)
	if err != nil {
		t.Fatalf("Events.List() error = %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("len(Events.List()) = %d, want 2", len(listed))
	}
	if listed[0].ID != "event_3" || listed[1].ID != "event_2" {
		t.Fatalf("Events.List() order = %#v, want [event_3 event_2]", listed)
	}

	allByDefault, err := repos.Events.List(ctx, 0)
	if err != nil {
		t.Fatalf("Events.List(default) error = %v", err)
	}
	if len(allByDefault) != 3 {
		t.Fatalf("len(Events.List(default)) = %d, want 3", len(allByDefault))
	}

	byEntity, err := repos.Events.ListByEntity(ctx, "run", "run_1")
	if err != nil {
		t.Fatalf("Events.ListByEntity() error = %v", err)
	}
	if len(byEntity) != 3 {
		t.Fatalf("len(Events.ListByEntity()) = %d, want 3", len(byEntity))
	}
	if byEntity[0].ID != "event_1" || byEntity[1].ID != "event_2" || byEntity[2].ID != "event_3" {
		t.Fatalf("Events.ListByEntity() order = %#v, want [event_1 event_2 event_3]", byEntity)
	}
}

func TestEventsListByEntityAndEventTypesFiltersByType(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	entityType := "pull_request"
	entityID := "acme/looper#42"
	for _, event := range []EventLogRecord{
		{ID: "gate_1", EventType: "gate.report", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: "2026-04-11T12:00:00.000Z"},
		{ID: "review_1", EventType: "pr.review.posted", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: "2026-04-11T12:01:00.000Z"},
		{ID: "gate_2", EventType: "gate.report", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: "2026-04-11T12:02:00.000Z"},
		{ID: "review_2", EventType: "pr.review.posted", EntityType: &entityType, EntityID: &entityID, PayloadJSON: `{}`, CreatedAt: "2026-04-11T12:03:00.000Z"},
	} {
		if err := repos.Events.Append(ctx, event); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", event.ID, err)
		}
	}

	filtered, err := repos.Events.ListByEntityAndEventTypes(ctx, "pull_request", "acme/looper#42", []string{"pr.review.posted"})
	if err != nil {
		t.Fatalf("Events.ListByEntityAndEventTypes() error = %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("len(ListByEntityAndEventTypes) = %d, want 2 (only pr.review.posted)", len(filtered))
	}
	if filtered[0].ID != "review_1" || filtered[1].ID != "review_2" {
		t.Fatalf("ListByEntityAndEventTypes() = %#v, want [review_1 review_2]", filtered)
	}

	empty, err := repos.Events.ListByEntityAndEventTypes(ctx, "pull_request", "acme/looper#42", nil)
	if err != nil {
		t.Fatalf("Events.ListByEntityAndEventTypes(nil types) error = %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("len(ListByEntityAndEventTypes(nil)) = %d, want 0", len(empty))
	}
}

func TestRunsListByStatusOrdersByStartedAtThenIDDesc(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for index, loopID := range []string{"loop_1", "loop_2", "loop_3"} {
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(index + 1), ProjectID: "project_1", Type: "reviewer", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
	}

	for _, run := range []RunRecord{
		{ID: "run_1", LoopID: "loop_1", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_3", LoopID: "loop_3", Status: "running", StartedAt: "2026-04-11T12:00:00.000Z", CreatedAt: now, UpdatedAt: now},
		{ID: "run_2", LoopID: "loop_2", Status: "running", StartedAt: "2026-04-11T12:01:00.000Z", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	running, err := repos.Runs.ListByStatus(ctx, "running")
	if err != nil {
		t.Fatalf("Runs.ListByStatus() error = %v", err)
	}
	if len(running) != 3 {
		t.Fatalf("len(Runs.ListByStatus()) = %d, want 3", len(running))
	}

	got := []string{running[0].ID, running[1].ID, running[2].ID}
	want := []string{"run_2", "run_3", "run_1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Runs.ListByStatus() order = %v, want %v", got, want)
		}
	}
}

func TestRunsEnforceOneRunningRunPerLoop(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_running_unique", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_running_unique", ProjectID: "project_running_unique", Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_running_unique_1", LoopID: "loop_running_unique", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("first Runs.Upsert() error = %v", err)
	}
	if hasRunning, err := repos.Runs.HasRunningByLoopID(ctx, "loop_running_unique"); err != nil || !hasRunning {
		t.Fatalf("HasRunningByLoopID() = %v, %v; want true, nil", hasRunning, err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_running_unique_2", LoopID: "loop_running_unique", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err == nil {
		t.Fatal("second running Runs.Upsert() error = nil, want unique constraint failure")
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_running_unique_2", LoopID: "loop_running_unique", Status: "success", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("terminal Runs.Upsert() error = %v", err)
	}
}

func TestQueueRoundTripBasics(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_q", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_q", Seq: 1, ProjectID: "project_q", Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	repo := "acme/looper"
	prNumber := int64(7)
	payload := `{"kind":"review"}`
	projectID := "project_q"
	loopID := "loop_q"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID:          "qi_1",
		ProjectID:   &projectID,
		LoopID:      &loopID,
		Type:        "reviewer",
		TargetType:  "pull_request",
		TargetID:    "pr:7",
		Repo:        &repo,
		PRNumber:    &prNumber,
		DedupeKey:   "reviewer:acme/looper:7",
		Priority:    10,
		Status:      "queued",
		AvailableAt: now,
		Attempts:    0,
		MaxAttempts: 3,
		PayloadJSON: &payload,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	gotByID, err := repos.Queue.GetByID(ctx, "qi_1")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if gotByID == nil || gotByID.Type != "reviewer" || gotByID.TargetID != "pr:7" {
		t.Fatalf("Queue.GetByID() = %#v, want reviewer/pr:7", gotByID)
	}

	activeByDedupe, err := repos.Queue.FindActiveByDedupe(ctx, "reviewer:acme/looper:7")
	if err != nil {
		t.Fatalf("Queue.FindActiveByDedupe() error = %v", err)
	}
	if activeByDedupe == nil || activeByDedupe.ID != "qi_1" {
		t.Fatalf("Queue.FindActiveByDedupe() = %#v, want qi_1", activeByDedupe)
	}

	scheduled, err := repos.Queue.ListScheduled(ctx, now, 10)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 1 || scheduled[0].ID != "qi_1" {
		t.Fatalf("Queue.ListScheduled() = %#v, want [qi_1]", scheduled)
	}

	all, err := repos.Queue.List(ctx)
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(all) != 1 || all[0].ID != "qi_1" {
		t.Fatalf("Queue.List() = %#v, want [qi_1]", all)
	}
}

func TestQueueRequeueFailedByIDRequiresMatchingLoopAndNoActiveQueue(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_requeue", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for seq, loopID := range []string{"loop_a", "loop_b"} {
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(seq + 1), ProjectID: "project_requeue", Type: "reviewer", TargetType: "pull_request", Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
	}
	loopA := "loop_a"
	loopB := "loop_b"
	lastError := "PR head changed before publish"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "failed_a", LoopID: &loopA, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:a", DedupeKey: "reviewer:a", Priority: QueuePriorityReviewer, Status: "failed", AvailableAt: now, Attempts: 3, MaxAttempts: 3, LastError: &lastError, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(failed_a) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "active_b", LoopID: &loopB, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:b", DedupeKey: "reviewer:b", Priority: QueuePriorityReviewer, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(active_b) error = %v", err)
	}

	affected, err := repos.Queue.RequeueFailedByID(ctx, loopB, "failed_a", now)
	if err != nil {
		t.Fatalf("RequeueFailedByID(wrong loop) error = %v", err)
	}
	if affected != 0 {
		t.Fatalf("RequeueFailedByID(wrong loop) affected = %d, want 0", affected)
	}
	item, _ := repos.Queue.GetByID(ctx, "failed_a")
	if item == nil || item.Status != "failed" {
		t.Fatalf("failed_a status = %#v, want still failed", item)
	}

	affected, err = repos.Queue.RequeueFailedByID(ctx, loopA, "failed_a", now)
	if err != nil {
		t.Fatalf("RequeueFailedByID(correct loop) error = %v", err)
	}
	if affected != 1 {
		t.Fatalf("RequeueFailedByID(correct loop) affected = %d, want 1", affected)
	}
	item, _ = repos.Queue.GetByID(ctx, "failed_a")
	if item == nil || item.Status != "queued" || item.LastError != nil || item.Attempts != 0 {
		t.Fatalf("failed_a = %#v, want requeued with cleared failure metadata", item)
	}
}

func TestQueueRequeueFailedByIDWithAttemptsPreservesAttemptBudget(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_requeue_attempts", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_requeue_attempts"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_requeue_attempts", Type: "reviewer", TargetType: "pull_request", Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastError := "reviewer agent timed out"
	lastErrorKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "failed_attempts", LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:a", DedupeKey: "reviewer:attempts", Priority: QueuePriorityReviewer, Status: "failed", AvailableAt: now, Attempts: 3, MaxAttempts: 5, LastError: &lastError, LastErrorKind: &lastErrorKind, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	affected, err := repos.Queue.RequeueFailedByIDWithAttempts(ctx, loopID, "failed_attempts", now, 3)
	if err != nil {
		t.Fatalf("RequeueFailedByIDWithAttempts() error = %v", err)
	}
	if affected != 1 {
		t.Fatalf("RequeueFailedByIDWithAttempts() affected = %d, want 1", affected)
	}
	item, _ := repos.Queue.GetByID(ctx, "failed_attempts")
	if item == nil || item.Status != "queued" || item.Attempts != 3 || item.LastError != nil || item.LastErrorKind != nil {
		t.Fatalf("failed_attempts = %#v, want queued with preserved attempts and cleared failure metadata", item)
	}
}

func TestQueueCreateOrGetActiveByDedupeAllowsNewActiveAfterInactiveHistory(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_history"
	loopID := "loop_history"
	repoName := "acme/looper"
	prNumber := int64(42)
	dedupeKey := "reviewer:project_history:loop_history:acme/looper:42"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "failed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	for _, item := range []QueueItemRecord{
		{ID: "queue_completed", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "completed", AvailableAt: now, Attempts: 1, MaxAttempts: 3, FinishedAt: &now, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_failed", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "failed", AvailableAt: now, Attempts: 3, MaxAttempts: 3, FinishedAt: &now, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_cancelled", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "cancelled", AvailableAt: now, Attempts: 0, MaxAttempts: 3, FinishedAt: &now, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	created, didCreate, err := repos.Queue.CreateOrGetActiveByDedupe(ctx, QueueItemRecord{ID: "queue_new", ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: QueuePriorityReviewer, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("Queue.CreateOrGetActiveByDedupe() error = %v", err)
	}
	if !didCreate || created.ID != "queue_new" {
		t.Fatalf("Queue.CreateOrGetActiveByDedupe() = (%#v, %v), want newly created queue_new", created, didCreate)
	}

	active, err := repos.Queue.FindActiveByDedupe(ctx, dedupeKey)
	if err != nil {
		t.Fatalf("Queue.FindActiveByDedupe() error = %v", err)
	}
	if active == nil || active.ID != "queue_new" {
		t.Fatalf("Queue.FindActiveByDedupe() = %#v, want queue_new", active)
	}
}

func TestQueueCreateOrGetActiveByDedupeKeepsOneActiveItemUnderConcurrency(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		dedupeKey string
		queueType string
		priority  int64
	}{
		{name: "reviewer", dedupeKey: "reviewer:project_1:loop_1:acme/looper:42", queueType: "reviewer", priority: QueuePriorityReviewer},
		{name: "fixer", dedupeKey: "fixer:project_1:loop_1:acme/looper:42:head-1:hash-1", queueType: "fixer", priority: QueuePriorityFixer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := openMigratedCoordinatorForRepositories(t)
			ctx := context.Background()
			repos := NewRepositories(coordinator.DB())

			now := "2026-04-11T12:00:00.000Z"
			projectID := "project_1"
			loopID := "loop_1"
			repoName := "acme/looper"
			prNumber := int64(42)
			if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: tc.queueType, TargetType: "pull_request", Status: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}

			const attempts = 8
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make(chan QueueItemRecord, attempts)
			errs := make(chan error, attempts)
			createdCount := make(chan bool, attempts)
			for i := 0; i < attempts; i++ {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()
					<-start
					item, didCreate, err := repos.Queue.CreateOrGetActiveByDedupe(ctx, QueueItemRecord{ID: fmt.Sprintf("queue_%d", index), ProjectID: &projectID, LoopID: &loopID, Type: tc.queueType, TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: tc.dedupeKey, Priority: tc.priority, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now})
					if err != nil {
						errs <- err
						return
					}
					createdCount <- didCreate
					results <- item
				}(i)
			}
			close(start)
			wg.Wait()
			close(errs)
			close(results)
			close(createdCount)

			for err := range errs {
				if err != nil {
					t.Fatalf("Queue.CreateOrGetActiveByDedupe() error = %v", err)
				}
			}
			winnerID := ""
			for item := range results {
				if winnerID == "" {
					winnerID = item.ID
					continue
				}
				if item.ID != winnerID {
					t.Fatalf("concurrent item ID = %q, want %q", item.ID, winnerID)
				}
			}
			creates := 0
			for didCreate := range createdCount {
				if didCreate {
					creates++
				}
			}
			if creates != 1 {
				t.Fatalf("created count = %d, want 1", creates)
			}

			items, err := repos.Queue.List(ctx)
			if err != nil {
				t.Fatalf("Queue.List() error = %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("len(Queue.List()) = %d, want 1", len(items))
			}
			if items[0].DedupeKey != tc.dedupeKey || items[0].Status != "queued" {
				t.Fatalf("Queue.List()[0] = %#v, want queued item for %q", items[0], tc.dedupeKey)
			}
		})
	}
}

func TestQueueUpsertActiveByDedupeOrGetExistingReturnsActiveReviewerOrFixer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		queueType string
		priority  int64
	}{
		{name: "reviewer", queueType: "reviewer", priority: QueuePriorityReviewer},
		{name: "fixer", queueType: "fixer", priority: QueuePriorityFixer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := openMigratedCoordinatorForRepositories(t)
			ctx := context.Background()
			repos := NewRepositories(coordinator.DB())

			now := "2026-04-11T12:00:00.000Z"
			projectID := "project_1"
			loopID := "loop_1"
			repoName := "acme/looper"
			prNumber := int64(42)
			dedupeKey := fmt.Sprintf("%s:project_1:loop_1:acme/looper:42", tc.queueType)
			if tc.queueType == "fixer" {
				dedupeKey = "fixer:project_1:loop_1:acme/looper:42:head-1:hash-1"
			}
			if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: tc.queueType, TargetType: "pull_request", Status: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Loops.Upsert() error = %v", err)
			}
			existing := QueueItemRecord{ID: "queue_existing", ProjectID: &projectID, LoopID: &loopID, Type: tc.queueType, TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: tc.priority, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}
			if err := repos.Queue.Upsert(ctx, existing); err != nil {
				t.Fatalf("Queue.Upsert(existing) error = %v", err)
			}

			persisted, didPersist, err := repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, QueueItemRecord{ID: "queue_racing", ProjectID: &projectID, LoopID: &loopID, Type: tc.queueType, TargetType: "pull_request", TargetID: "pr:42", Repo: &repoName, PRNumber: &prNumber, DedupeKey: dedupeKey, Priority: tc.priority, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatalf("Queue.UpsertActiveByDedupeOrGetExisting() error = %v", err)
			}
			if didPersist {
				t.Fatal("Queue.UpsertActiveByDedupeOrGetExisting() persisted = true, want false")
			}
			if persisted.ID != existing.ID {
				t.Fatalf("Queue.UpsertActiveByDedupeOrGetExisting() = %#v, want existing queue item", persisted)
			}
		})
	}
}

func TestQueueClaimOrderingAndBlockers(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_sched", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_active", Seq: 1, ProjectID: "project_sched", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_active) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_paused", Seq: 2, ProjectID: "project_sched", Type: "worker", TargetType: "project", Status: "paused", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_paused) error = %v", err)
	}

	repo := "acme/looper"
	pr := int64(42)
	loopActive := "loop_active"
	loopPaused := "loop_paused"
	queueItems := []QueueItemRecord{
		{ID: "lock_running", LoopID: &loopActive, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:lock-running", DedupeKey: "d_lock_running", Priority: 1, Status: "running", AvailableAt: "2026-04-11T11:50:00.000Z", Attempts: 1, MaxAttempts: 3, LockKey: strPtr("repo:acme/looper"), CreatedAt: "2026-04-11T11:40:00.000Z", UpdatedAt: now},
		{ID: "lock_blocked", LoopID: &loopActive, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:lock-blocked", DedupeKey: "d_lock_blocked", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, LockKey: strPtr("repo:acme/looper"), CreatedAt: "2026-04-11T11:41:00.000Z", UpdatedAt: now},
		{ID: "reviewer_blocker", LoopID: &loopActive, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repo, PRNumber: &pr, DedupeKey: "d_reviewer", Priority: 2, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:42:00.000Z", UpdatedAt: now},
		{ID: "fixer_blocked", LoopID: &loopActive, Type: "fixer", TargetType: "pull_request", TargetID: "pr:42", Repo: &repo, PRNumber: &pr, DedupeKey: "d_fixer_blocked", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:43:00.000Z", UpdatedAt: now},
		{ID: "paused_excluded", LoopID: &loopPaused, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:paused", DedupeKey: "d_paused", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:44:00.000Z", UpdatedAt: now},
		{ID: "eligible_1", LoopID: &loopActive, Type: "planner", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_eligible_1", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:45:00.000Z", UpdatedAt: now},
		{ID: "eligible_2", LoopID: &loopActive, Type: "planner", TargetType: "project", TargetID: "project_sched", DedupeKey: "d_eligible_2", Priority: 3, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: "2026-04-11T11:46:00.000Z", UpdatedAt: now},
	}

	for _, item := range queueItems {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	scheduled, err := repos.Queue.ListScheduled(ctx, now, 10)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 3 {
		t.Fatalf("len(Queue.ListScheduled()) = %d, want 3", len(scheduled))
	}
	if scheduled[0].ID != "eligible_1" || scheduled[1].ID != "reviewer_blocker" || scheduled[2].ID != "eligible_2" {
		t.Fatalf("Queue.ListScheduled() order = %#v, want eligible_1/reviewer_blocker/eligible_2", []string{scheduled[0].ID, scheduled[1].ID, scheduled[2].ID})
	}

	claimed, err := repos.Queue.ClaimNext(ctx, now, "worker-a")
	if err != nil {
		t.Fatalf("Queue.ClaimNext() error = %v", err)
	}
	if claimed == nil || claimed.ID != "eligible_1" || claimed.Status != "running" {
		t.Fatalf("Queue.ClaimNext() = %#v, want eligible_1 running", claimed)
	}
	if claimed.ClaimedBy == nil || *claimed.ClaimedBy != "worker-a" || claimed.ClaimedAt == nil || *claimed.ClaimedAt != now {
		t.Fatalf("claimed metadata = %#v, want claimed_by worker-a and claimed_at %q", claimed, now)
	}

	claimedType, err := repos.Queue.ClaimNextOfType(ctx, now, "worker-b", "planner")
	if err != nil {
		t.Fatalf("Queue.ClaimNextOfType() error = %v", err)
	}
	if claimedType == nil || claimedType.ID != "eligible_2" {
		t.Fatalf("Queue.ClaimNextOfType() = %#v, want eligible_2", claimedType)
	}
	if claimedType.ClaimedBy == nil || *claimedType.ClaimedBy != "worker-b" || claimedType.ClaimedAt == nil || *claimedType.ClaimedAt != now {
		t.Fatalf("claimedType metadata = %#v, want claimed_by worker-b and claimed_at %q", claimedType, now)
	}
}

func TestQueueLongTermRetryClaimsAfterFreshWork(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_retry_order", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_retry_order", Seq: 1, ProjectID: "project_retry_order", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loopID := "loop_retry_order"
	retryKind := "retryable_transient"
	items := []QueueItemRecord{
		{ID: "long_retry", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_retry_order", DedupeKey: "d_long_retry", Priority: QueuePriorityPlanner, Status: "queued", AvailableAt: now, Attempts: QueueLongTermRetryAttemptThreshold, MaxAttempts: -1, LastErrorKind: &retryKind, CreatedAt: "2026-04-11T11:00:00.000Z", UpdatedAt: now},
		{ID: "fresh", LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: "project_retry_order", DedupeKey: "d_fresh", Priority: QueuePriorityWorker, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: -1, CreatedAt: "2026-04-11T11:30:00.000Z", UpdatedAt: now},
	}
	for _, item := range items {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	scheduled, err := repos.Queue.ListScheduled(ctx, now, 2)
	if err != nil {
		t.Fatalf("Queue.ListScheduled() error = %v", err)
	}
	if len(scheduled) != 2 || scheduled[0].ID != "fresh" || scheduled[1].ID != "long_retry" {
		t.Fatalf("Queue.ListScheduled() order = %#v, want fresh before long_retry", scheduled)
	}
	claimed, err := repos.Queue.ClaimNext(ctx, now, "worker-a")
	if err != nil {
		t.Fatalf("Queue.ClaimNext() error = %v", err)
	}
	if claimed == nil || claimed.ID != "fresh" {
		t.Fatalf("Queue.ClaimNext() = %#v, want fresh", claimed)
	}
	claimedLong, err := repos.Queue.ClaimNextLongTermRetry(ctx, now, "worker-b")
	if err != nil {
		t.Fatalf("Queue.ClaimNextLongTermRetry() error = %v", err)
	}
	if claimedLong == nil || claimedLong.ID != "long_retry" {
		t.Fatalf("Queue.ClaimNextLongTermRetry() = %#v, want long_retry", claimedLong)
	}
}

func TestQueueRecoveredRetryWithoutErrorKindStaysNonLongTerm(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_retry_recovered", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_retry_recovered"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_retry_recovered", Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	lastError := "reviewer agent timed out"
	lastErrorKind := "retryable_transient"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "recovered_retry", LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:a", DedupeKey: "reviewer:recovered", Priority: QueuePriorityReviewer, Status: "failed", AvailableAt: now, Attempts: QueueLongTermRetryAttemptThreshold, MaxAttempts: -1, LastError: &lastError, LastErrorKind: &lastErrorKind, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}

	affected, err := repos.Queue.RequeueFailedByIDWithAttempts(ctx, loopID, "recovered_retry", now, QueueLongTermRetryAttemptThreshold)
	if err != nil {
		t.Fatalf("RequeueFailedByIDWithAttempts() error = %v", err)
	}
	if affected != 1 {
		t.Fatalf("RequeueFailedByIDWithAttempts() affected = %d, want 1", affected)
	}

	claimedLong, err := repos.Queue.ClaimNextLongTermRetry(ctx, now, "worker-a")
	if err != nil {
		t.Fatalf("Queue.ClaimNextLongTermRetry() error = %v", err)
	}
	if claimedLong != nil {
		t.Fatalf("Queue.ClaimNextLongTermRetry() = %#v, want nil", claimedLong)
	}

	claimed, err := repos.Queue.ClaimNextNonLongTermRetry(ctx, now, "worker-b")
	if err != nil {
		t.Fatalf("Queue.ClaimNextNonLongTermRetry() error = %v", err)
	}
	if claimed == nil || claimed.ID != "recovered_retry" {
		t.Fatalf("Queue.ClaimNextNonLongTermRetry() = %#v, want recovered_retry", claimed)
	}
	if claimed.LastErrorKind != nil {
		t.Fatalf("claimed.LastErrorKind = %#v, want nil", claimed.LastErrorKind)
	}
}

func TestQueueBoundedNonRetryableRetryMovesToLongTermLane(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_non_retryable_long_term", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loopID := "loop_non_retryable_long_term"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_non_retryable_long_term", Type: "planner", TargetType: "issue", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	nonRetryableKind := "non_retryable"
	items := []QueueItemRecord{
		{ID: "fresh_retryable", LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: "issue:1", DedupeKey: "planner:fresh", Priority: QueuePriorityPlanner, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 8, CreatedAt: "2026-04-11T11:30:00.000Z", UpdatedAt: now},
		{ID: "bounded_non_retryable", LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: "issue:2", DedupeKey: "planner:bounded_non_retryable", Priority: QueuePriorityPlanner, Status: "queued", AvailableAt: now, Attempts: QueueLongTermRetryAttemptThreshold, MaxAttempts: 8, LastErrorKind: &nonRetryableKind, CreatedAt: "2026-04-11T11:00:00.000Z", UpdatedAt: now},
	}
	for _, item := range items {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	claimed, err := repos.Queue.ClaimNextNonLongTermRetry(ctx, now, "worker-a")
	if err != nil {
		t.Fatalf("Queue.ClaimNextNonLongTermRetry() error = %v", err)
	}
	if claimed == nil || claimed.ID != "fresh_retryable" {
		t.Fatalf("Queue.ClaimNextNonLongTermRetry() = %#v, want fresh_retryable", claimed)
	}

	claimedLong, err := repos.Queue.ClaimNextLongTermRetry(ctx, now, "worker-b")
	if err != nil {
		t.Fatalf("Queue.ClaimNextLongTermRetry() error = %v", err)
	}
	if claimedLong == nil || claimedLong.ID != "bounded_non_retryable" {
		t.Fatalf("Queue.ClaimNextLongTermRetry() = %#v, want bounded_non_retryable", claimedLong)
	}
	if claimedLong.LastErrorKind == nil || *claimedLong.LastErrorKind != nonRetryableKind {
		t.Fatalf("claimedLong.LastErrorKind = %#v, want %q", claimedLong.LastErrorKind, nonRetryableKind)
	}
}

func TestQueueRetryFailCompleteTransitions(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_tr", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_tr", Seq: 1, ProjectID: "project_tr", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	loopID := "loop_tr"
	claimedBy := "worker-a"
	claimedAt := "2026-04-11T12:00:10.000Z"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID:          "qi_retry",
		LoopID:      &loopID,
		Type:        "planner",
		TargetType:  "project",
		TargetID:    "project_tr",
		DedupeKey:   "d_retry",
		Priority:    1,
		Status:      "running",
		AvailableAt: now,
		Attempts:    1,
		MaxAttempts: 3,
		ClaimedBy:   &claimedBy,
		ClaimedAt:   &claimedAt,
		StartedAt:   &claimedAt,
		CreatedAt:   now,
		UpdatedAt:   claimedAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert(qi_retry) error = %v", err)
	}

	retryAt := "2026-04-11T12:05:00.000Z"
	errMsg := "temporary error"
	if err := repos.Queue.MarkRetry(ctx, QueueMarkRetryInput{ID: "qi_retry", AvailableAt: retryAt, Attempts: 2, ErrorMessage: &errMsg, ErrorKind: "retryable_transient", UpdatedAt: retryAt}); err != nil {
		t.Fatalf("Queue.MarkRetry() error = %v", err)
	}
	gotRetry, err := repos.Queue.GetByID(ctx, "qi_retry")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_retry) error = %v", err)
	}
	if gotRetry == nil || gotRetry.Status != "queued" || gotRetry.Attempts != 2 || gotRetry.ClaimedBy != nil || gotRetry.ClaimedAt != nil {
		t.Fatalf("Queue.GetByID(qi_retry) after markRetry = %#v, want queued attempts=2 unclaimed", gotRetry)
	}

	// MarkRetryIfRunning must not resurrect a terminal cancellation.
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID:          "qi_retry_cancelled",
		LoopID:      &loopID,
		Type:        "planner",
		TargetType:  "project",
		TargetID:    "project_tr",
		DedupeKey:   "d_retry_cancelled",
		Priority:    1,
		Status:      "cancelled",
		AvailableAt: now,
		Attempts:    1,
		MaxAttempts: 3,
		CreatedAt:   now,
		UpdatedAt:   claimedAt,
	}); err != nil {
		t.Fatalf("Queue.Upsert(qi_retry_cancelled) error = %v", err)
	}
	if err := repos.Queue.MarkRetryIfRunning(ctx, QueueMarkRetryInput{ID: "qi_retry_cancelled", AvailableAt: retryAt, Attempts: 2, ErrorMessage: &errMsg, ErrorKind: "retryable_transient", UpdatedAt: retryAt}); err != nil {
		t.Fatalf("Queue.MarkRetryIfRunning(cancelled) error = %v", err)
	}
	gotCancelled, err := repos.Queue.GetByID(ctx, "qi_retry_cancelled")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_retry_cancelled) error = %v", err)
	}
	if gotCancelled == nil || gotCancelled.Status != "cancelled" {
		t.Fatalf("Queue.MarkRetryIfRunning on cancelled = %#v, want status still cancelled (zero-row no-op)", gotCancelled)
	}
	// Unrestricted MarkRetry remains runner authority: may requeue after mid-run pause cancel.
	if err := repos.Queue.MarkRetry(ctx, QueueMarkRetryInput{ID: "qi_retry_cancelled", AvailableAt: retryAt, Attempts: 2, ErrorMessage: &errMsg, ErrorKind: "retryable_transient", UpdatedAt: retryAt}); err != nil {
		t.Fatalf("Queue.MarkRetry(cancelled) error = %v", err)
	}
	gotResurrected, err := repos.Queue.GetByID(ctx, "qi_retry_cancelled")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_retry_cancelled) after MarkRetry error = %v", err)
	}
	if gotResurrected == nil || gotResurrected.Status != "queued" || gotResurrected.Attempts != 2 {
		t.Fatalf("Queue.MarkRetry on cancelled = %#v, want queued attempts=2", gotResurrected)
	}

	finished := "2026-04-11T12:06:00.000Z"
	if err := repos.Queue.Complete(ctx, "qi_retry", finished); err != nil {
		t.Fatalf("Queue.Complete() error = %v", err)
	}
	gotCompleted, err := repos.Queue.GetByID(ctx, "qi_retry")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_retry) completed error = %v", err)
	}
	if gotCompleted == nil || gotCompleted.Status != "completed" || gotCompleted.FinishedAt == nil || *gotCompleted.FinishedAt != finished {
		t.Fatalf("Queue.GetByID(qi_retry) after complete = %#v, want completed with finished_at", gotCompleted)
	}

	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID:          "qi_fail",
		LoopID:      &loopID,
		Type:        "planner",
		TargetType:  "project",
		TargetID:    "project_tr",
		DedupeKey:   "d_fail",
		Priority:    1,
		Status:      "running",
		AvailableAt: now,
		Attempts:    3,
		MaxAttempts: 3,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("Queue.Upsert(qi_fail) error = %v", err)
	}

	failReason := "needs human"
	if err := repos.Queue.Fail(ctx, QueueFailInput{ID: "qi_fail", Attempts: 4, FinishedAt: finished, ErrorMessage: &failReason, ErrorKind: "manual_intervention", UpdatedAt: finished}); err != nil {
		t.Fatalf("Queue.Fail() error = %v", err)
	}
	gotFailed, err := repos.Queue.GetByID(ctx, "qi_fail")
	if err != nil {
		t.Fatalf("Queue.GetByID(qi_fail) error = %v", err)
	}
	if gotFailed == nil || gotFailed.Status != "manual_intervention" || gotFailed.LastError == nil || *gotFailed.LastError != failReason || gotFailed.Attempts != 4 {
		t.Fatalf("Queue.GetByID(qi_fail) after fail = %#v, want manual_intervention", gotFailed)
	}
}

func TestQueueTerminalWritersDoNotOverwriteCancelledProjectItems(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_archived"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopID := "loop_archived"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	claimedBy := "worker-a"
	claimedAt := "2026-04-11T12:00:10.000Z"
	for _, item := range []QueueItemRecord{
		{ID: "qi_cancelled_complete", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "d_cancelled_complete", Priority: 1, Status: "running", AvailableAt: now, Attempts: 1, MaxAttempts: 3, ClaimedBy: &claimedBy, ClaimedAt: &claimedAt, StartedAt: &claimedAt, CreatedAt: now, UpdatedAt: claimedAt},
		{ID: "qi_cancelled_fail", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "d_cancelled_fail", Priority: 1, Status: "running", AvailableAt: now, Attempts: 1, MaxAttempts: 3, ClaimedBy: &claimedBy, ClaimedAt: &claimedAt, StartedAt: &claimedAt, CreatedAt: now, UpdatedAt: claimedAt},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	archivedAt := "2026-04-11T12:01:00.000Z"
	reason := "project archived"
	if _, err := repos.Queue.CancelByProject(ctx, projectID, archivedAt, &reason); err != nil {
		t.Fatalf("Queue.CancelByProject() error = %v", err)
	}

	if err := repos.Queue.Complete(ctx, "qi_cancelled_complete", "2026-04-11T12:02:00.000Z"); !errors.Is(err, ErrQueueItemNotActive) {
		t.Fatalf("Queue.Complete() error = %v, want ErrQueueItemNotActive", err)
	}
	failReason := "late failure"
	if err := repos.Queue.Fail(ctx, QueueFailInput{ID: "qi_cancelled_fail", Attempts: 2, FinishedAt: "2026-04-11T12:02:30.000Z", ErrorMessage: &failReason, ErrorKind: "manual_intervention", UpdatedAt: "2026-04-11T12:02:30.000Z"}); err != nil {
		t.Fatalf("Queue.Fail() error = %v", err)
	}

	for _, id := range []string{"qi_cancelled_complete", "qi_cancelled_fail"} {
		item, err := repos.Queue.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, err)
		}
		if item == nil || item.Status != "cancelled" {
			t.Fatalf("Queue.GetByID(%s) = %#v, want cancelled", id, item)
		}
		if item.FinishedAt == nil || *item.FinishedAt != archivedAt {
			t.Fatalf("Queue.GetByID(%s).FinishedAt = %#v, want %q", id, item.FinishedAt, archivedAt)
		}
		if item.LastError == nil || *item.LastError != reason {
			t.Fatalf("Queue.GetByID(%s).LastError = %#v, want %q", id, item.LastError, reason)
		}
	}
}

func TestTargetedListRepositories(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	later := "2026-04-11T12:10:00.000Z"
	projectID := "project_targeted"
	loopQueued := "loop_queued"
	loopManual := "loop_manual"
	loopDone := "loop_done"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for _, loop := range []LoopRecord{
		{ID: loopQueued, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: now, UpdatedAt: now},
		{ID: loopManual, Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: later},
		{ID: loopDone, Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "completed", CreatedAt: now, UpdatedAt: later},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	manualKind := "manual_intervention"
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	for _, item := range []QueueItemRecord{
		{ID: "queue_queued", ProjectID: &projectID, LoopID: &loopQueued, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "queued", Priority: QueuePriorityWorker, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_manual", ProjectID: &projectID, LoopID: &loopManual, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "manual", Priority: QueuePriorityWorker, Status: "manual_intervention", AvailableAt: now, Attempts: 1, MaxAttempts: 3, LastErrorKind: &manualKind, CreatedAt: later, UpdatedAt: later},
		{ID: "queue_done_manual_old", ProjectID: &projectID, LoopID: &loopDone, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "done_manual_old", Priority: QueuePriorityWorker, Status: "manual_intervention", AvailableAt: now, Attempts: 1, MaxAttempts: 3, LastErrorKind: &manualKind, CreatedAt: now, UpdatedAt: now},
		{ID: "queue_done", ProjectID: &projectID, LoopID: &loopDone, Type: "worker", TargetType: "project", TargetID: projectID, DedupeKey: "done", Priority: QueuePriorityWorker, Status: "completed", AvailableAt: now, Attempts: 1, MaxAttempts: 3, CreatedAt: later, UpdatedAt: later},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}
	for _, run := range []RunRecord{
		{ID: "run_manual_old", LoopID: loopManual, Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: "run_manual_latest", LoopID: loopManual, Status: "failed", CheckpointJSON: &checkpoint, StartedAt: later, CreatedAt: later, UpdatedAt: later},
		{ID: "run_done", LoopID: loopDone, Status: "completed", StartedAt: later, CreatedAt: later, UpdatedAt: later},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	loopsByStatus, err := repos.Loops.ListByStatuses(ctx, []string{"queued", "running"})
	if err != nil {
		t.Fatalf("Loops.ListByStatuses() error = %v", err)
	}
	if len(loopsByStatus) != 1 || loopsByStatus[0].ID != loopQueued {
		t.Fatalf("Loops.ListByStatuses() = %#v, want queued loop only", loopsByStatus)
	}

	queueByStatus, err := repos.Queue.ListByStatuses(ctx, []string{"queued", "manual_intervention"})
	if err != nil {
		t.Fatalf("Queue.ListByStatuses() error = %v", err)
	}
	if len(queueByStatus) != 3 {
		t.Fatalf("len(Queue.ListByStatuses()) = %d, want 3", len(queueByStatus))
	}

	latestQueueByStatus, err := repos.Queue.ListLatestByLoopStatuses(ctx, []string{"queued", "manual_intervention"})
	if err != nil {
		t.Fatalf("Queue.ListLatestByLoopStatuses() error = %v", err)
	}
	if len(latestQueueByStatus) != 2 {
		t.Fatalf("len(Queue.ListLatestByLoopStatuses()) = %d, want 2", len(latestQueueByStatus))
	}
	latestQueueIDs := []string{latestQueueByStatus[0].ID, latestQueueByStatus[1].ID}
	sort.Strings(latestQueueIDs)
	if want := []string{"queue_manual", "queue_queued"}; !reflect.DeepEqual(latestQueueIDs, want) {
		t.Fatalf("Queue.ListLatestByLoopStatuses() ids = %#v, want %#v", latestQueueIDs, want)
	}

	latestQueueByIDs, err := repos.Queue.ListLatestByLoopIDs(ctx, []string{loopManual, loopDone, loopQueued})
	if err != nil {
		t.Fatalf("Queue.ListLatestByLoopIDs() error = %v", err)
	}
	if len(latestQueueByIDs) != 3 {
		t.Fatalf("len(Queue.ListLatestByLoopIDs()) = %d, want 3", len(latestQueueByIDs))
	}
	latestByLoop := map[string]string{}
	for _, item := range latestQueueByIDs {
		if item.LoopID == nil {
			t.Fatalf("Queue.ListLatestByLoopIDs() item missing loop id: %#v", item)
		}
		latestByLoop[*item.LoopID] = item.ID
	}
	if latestByLoop[loopManual] != "queue_manual" || latestByLoop[loopQueued] != "queue_queued" || latestByLoop[loopDone] != "queue_done" {
		t.Fatalf("Queue.ListLatestByLoopIDs() by loop = %#v, want latest ids per loop", latestByLoop)
	}

	latestRuns, err := repos.Runs.ListLatestByLoopIDs(ctx, []string{loopManual, loopDone})
	if err != nil {
		t.Fatalf("Runs.ListLatestByLoopIDs() error = %v", err)
	}
	gotRunIDs := []string{latestRuns[0].ID, latestRuns[1].ID}
	sort.Strings(gotRunIDs)
	if want := []string{"run_done", "run_manual_latest"}; !reflect.DeepEqual(gotRunIDs, want) {
		t.Fatalf("Runs.ListLatestByLoopIDs() ids = %#v, want %#v", gotRunIDs, want)
	}

}

func TestRunsListLatestByLoopStatusesAndResumePolicySkipsMalformedCheckpointJSON(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	later := "2026-04-11T12:10:00.000Z"
	projectID := "project_resume_policy"
	checkpoint := `{"resumePolicy":"manual_intervention"}`
	malformedCheckpoint := `{"resumePolicy":`

	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for _, loop := range []LoopRecord{
		{ID: "loop_valid", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_malformed", Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "failed", CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	for _, run := range []RunRecord{
		{ID: "run_valid", LoopID: "loop_valid", Status: "failed", CheckpointJSON: &checkpoint, StartedAt: later, CreatedAt: later, UpdatedAt: later},
		{ID: "run_malformed", LoopID: "loop_malformed", Status: "failed", CheckpointJSON: &malformedCheckpoint, StartedAt: later, CreatedAt: later, UpdatedAt: later},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	runs, err := repos.Runs.ListLatestByLoopStatusesAndResumePolicy(ctx, []string{"failed"}, "manual_intervention")
	if err != nil {
		t.Fatalf("Runs.ListLatestByLoopStatusesAndResumePolicy() error = %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("len(Runs.ListLatestByLoopStatusesAndResumePolicy()) = %d, want 1", len(runs))
	}
	if runs[0].ID != "run_valid" {
		t.Fatalf("Runs.ListLatestByLoopStatusesAndResumePolicy()[0].ID = %q, want run_valid", runs[0].ID)
	}
}

func TestRepositoriesListByIDHelpersChunkLargeInput(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_chunk", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	loopIDs := make([]string, 0, sqliteMaxVariables+5)
	for i := 0; i < sqliteMaxVariables+5; i++ {
		loopID := fmt.Sprintf("loop_chunk_%04d", i)
		loopIDs = append(loopIDs, loopID)
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(i + 1), ProjectID: "project_chunk", Type: "worker", TargetType: "project", Status: "paused", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loopID, err)
		}
		runID := fmt.Sprintf("run_chunk_%04d", i)
		if err := repos.Runs.Upsert(ctx, RunRecord{ID: runID, LoopID: loopID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", runID, err)
		}
	}

	loopsList, err := repos.Loops.ListByIDs(ctx, loopIDs)
	if err != nil {
		t.Fatalf("Loops.ListByIDs() error = %v", err)
	}
	if got := len(loopsList); got != len(loopIDs) {
		t.Fatalf("len(Loops.ListByIDs()) = %d, want %d", got, len(loopIDs))
	}

	latestRuns, err := repos.Runs.ListLatestByLoopIDs(ctx, loopIDs)
	if err != nil {
		t.Fatalf("Runs.ListLatestByLoopIDs() error = %v", err)
	}
	if got := len(latestRuns); got != len(loopIDs) {
		t.Fatalf("len(Runs.ListLatestByLoopIDs()) = %d, want %d", got, len(loopIDs))
	}
}

func TestQueueRecoveryHelpersRequeueAndCancelByLoop(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_rec", Name: "Looper", RepoPath: "/tmp/looper", Archived: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_rec", Seq: 1, ProjectID: "project_rec", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	loopID := "loop_rec"
	runningAt := "2026-04-11T12:01:00.000Z"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "qi_run_1", LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: "project_rec", DedupeKey: "d_run_1", Priority: 1, Status: "running", AvailableAt: now, Attempts: 1, MaxAttempts: 3, ClaimedBy: strPtr("worker-a"), ClaimedAt: &runningAt, StartedAt: &runningAt, CreatedAt: now, UpdatedAt: runningAt}); err != nil {
		t.Fatalf("Queue.Upsert(qi_run_1) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "qi_run_2", LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: "project_rec", DedupeKey: "d_run_2", Priority: 2, Status: "running", AvailableAt: now, Attempts: 1, MaxAttempts: 3, ClaimedBy: strPtr("worker-b"), ClaimedAt: &runningAt, StartedAt: &runningAt, CreatedAt: now, UpdatedAt: runningAt}); err != nil {
		t.Fatalf("Queue.Upsert(qi_run_2) error = %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "qi_queued", LoopID: &loopID, Type: "planner", TargetType: "project", TargetID: "project_rec", DedupeKey: "d_queued", Priority: 2, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Queue.Upsert(qi_queued) error = %v", err)
	}

	requeuedAt := "2026-04-11T12:10:00.000Z"
	requeuedCount, err := repos.Queue.RequeueRunningByLoop(ctx, "loop_rec", requeuedAt)
	if err != nil {
		t.Fatalf("Queue.RequeueRunningByLoop() error = %v", err)
	}
	if requeuedCount != 2 {
		t.Fatalf("Queue.RequeueRunningByLoop() = %d, want 2", requeuedCount)
	}

	for _, id := range []string{"qi_run_1", "qi_run_2"} {
		got, getErr := repos.Queue.GetByID(ctx, id)
		if getErr != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, getErr)
		}
		if got == nil || got.Status != "queued" || got.ClaimedBy != nil || got.StartedAt != nil || got.AvailableAt != requeuedAt {
			t.Fatalf("Queue.GetByID(%s) after requeue = %#v, want queued/cleared/requeued time", id, got)
		}
	}

	reason := "loop paused"
	cancelledCount, err := repos.Queue.CancelByLoop(ctx, "loop_rec", requeuedAt, &reason)
	if err != nil {
		t.Fatalf("Queue.CancelByLoop() error = %v", err)
	}
	if cancelledCount != 3 {
		t.Fatalf("Queue.CancelByLoop() = %d, want 3", cancelledCount)
	}

	for _, id := range []string{"qi_run_1", "qi_run_2", "qi_queued"} {
		got, getErr := repos.Queue.GetByID(ctx, id)
		if getErr != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, getErr)
		}
		if got == nil || got.Status != "cancelled" || got.LastError == nil || *got.LastError != reason {
			t.Fatalf("Queue.GetByID(%s) after cancel = %#v, want cancelled with reason", id, got)
		}
	}

	revivedAt := "2026-04-11T12:11:00.000Z"
	revivedCount, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, "loop_rec", revivedAt)
	if err != nil {
		t.Fatalf("Queue.RequeueLatestCancelledByLoop() error = %v", err)
	}
	if revivedCount != 1 {
		t.Fatalf("Queue.RequeueLatestCancelledByLoop() = %d, want 1", revivedCount)
	}

	revivedID := ""
	for _, id := range []string{"qi_run_1", "qi_run_2", "qi_queued"} {
		got, getErr := repos.Queue.GetByID(ctx, id)
		if getErr != nil {
			t.Fatalf("Queue.GetByID(%s) after revive error = %v", id, getErr)
		}
		if got != nil && got.Status == "queued" {
			revivedID = id
			if got.FinishedAt != nil || got.LastError != nil || got.AvailableAt != revivedAt {
				t.Fatalf("Queue.GetByID(%s) after revive = %#v, want queued with cleared terminal state", id, got)
			}
		}
	}
	if revivedID == "" {
		t.Fatal("expected one cancelled queue item to be requeued")
	}
}

func TestQueueClaimNextOfTypeSkipsTerminatedAndStoppedLoops(t *testing.T) {
	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_queue_terminal"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Queue Terminal", RepoPath: "/tmp/repo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for index, status := range []string{"terminated", "stopped"} {
		loopID := "loop_" + status
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: int64(index + 1), ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: status, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", status, err)
		}
		if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: "queue_" + status, ProjectID: &projectID, LoopID: &loopID, Type: "reviewer", TargetType: "pull_request", TargetID: "pr:" + status, DedupeKey: "d_" + status, Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", status, err)
		}
	}

	claimed, err := repos.Queue.ClaimNextOfType(ctx, now, "worker", "reviewer")
	if err != nil {
		t.Fatalf("Queue.ClaimNextOfType() error = %v", err)
	}
	if claimed != nil {
		t.Fatalf("Queue.ClaimNextOfType() = %#v, want nil", claimed)
	}
}

func TestQueueClaimNextSkipsArchivedProjects(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	activeProjectID := "project_active"
	archivedProjectID := "project_archived"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: activeProjectID, Name: "Active", RepoPath: "/tmp/active", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert(active) error = %v", err)
	}
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: archivedProjectID, Name: "Archived", RepoPath: "/tmp/archived", Archived: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert(archived) error = %v", err)
	}
	for index, tc := range []struct{ projectID, loopID, queueID string }{{archivedProjectID, "loop_archived", "queue_archived"}, {activeProjectID, "loop_active", "queue_active"}} {
		if err := repos.Loops.Upsert(ctx, LoopRecord{ID: tc.loopID, Seq: int64(index + 1), ProjectID: tc.projectID, Type: "planner", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", tc.loopID, err)
		}
		if err := repos.Queue.Upsert(ctx, QueueItemRecord{ID: tc.queueID, ProjectID: &tc.projectID, LoopID: &tc.loopID, Type: "planner", TargetType: "project", TargetID: tc.projectID, DedupeKey: tc.queueID, Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", tc.queueID, err)
		}
	}

	claimed, err := repos.Queue.ClaimNext(ctx, now, "scheduler")
	if err != nil {
		t.Fatalf("Queue.ClaimNext() error = %v", err)
	}
	if claimed == nil || claimed.ID != "queue_active" {
		t.Fatalf("Queue.ClaimNext() = %#v, want queue_active", claimed)
	}
}

func TestQueueClaimNextRefusesAlreadyCancelledContextWithoutMutating(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-19T12:00:00.000Z"
	projectID := "project_claim_cancel"
	loopID := "loop_claim_cancel"
	queueID := "queue_claim_cancel"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "ClaimCancel", RepoPath: "/tmp/claim-cancel", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID: queueID, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "worker:claim_cancel", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Queue.Upsert: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	claimed, err := repos.Queue.ClaimNext(cancelCtx, now, "scheduler")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ClaimNext error = %v, want context.Canceled", err)
	}
	if claimed != nil {
		t.Fatalf("ClaimNext = %#v, want nil when context already cancelled", claimed)
	}
	got, err := repos.Queue.GetByID(ctx, queueID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Status != "queued" {
		t.Fatalf("queue item = %#v, want still queued (no durable claim)", got)
	}

	// Live context still claims successfully (detached path returns the row).
	claimed, err = repos.Queue.ClaimNext(ctx, now, "scheduler")
	if err != nil || claimed == nil || claimed.ID != queueID {
		t.Fatalf("ClaimNext live = (%#v, %v), want %s", claimed, err, queueID)
	}
}

func TestClaimCtxForDurableClaimPreservesDeadlineAndDetachesCancel(t *testing.T) {
	t.Parallel()

	deadline := time.Now().Add(150 * time.Millisecond)
	parent, cancelParent := context.WithDeadline(context.Background(), deadline)
	defer cancelParent()

	claimCtx, stop, err := claimCtxForDurableClaim(parent)
	if err != nil {
		t.Fatalf("claimCtxForDurableClaim: %v", err)
	}
	defer stop()

	gotDeadline, ok := claimCtx.Deadline()
	if !ok {
		t.Fatal("claim context must retain the caller deadline")
	}
	if !gotDeadline.Equal(deadline) {
		t.Fatalf("deadline = %v, want %v", gotDeadline, deadline)
	}

	// Parent cancel must not cancel the claim context (committed-but-unscanned case).
	cancelParent()
	if err := claimCtx.Err(); err != nil {
		t.Fatalf("claimCtx.Err after parent cancel = %v, want nil", err)
	}

	// Deadline must still fire on the detached claim context.
	select {
	case <-claimCtx.Done():
		if !errors.Is(claimCtx.Err(), context.DeadlineExceeded) {
			t.Fatalf("claimCtx.Err = %v, want DeadlineExceeded", claimCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("claim context did not honor retained deadline")
	}
}

func TestClaimCtxForDurableClaimNoDeadlineWithoutParentDeadline(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	claimCtx, stop, err := claimCtxForDurableClaim(parent)
	if err != nil {
		t.Fatalf("claimCtxForDurableClaim: %v", err)
	}
	defer stop()
	if _, ok := claimCtx.Deadline(); ok {
		t.Fatal("claim context must not invent a deadline when parent has none")
	}
	cancelParent()
	if err := claimCtx.Err(); err != nil {
		t.Fatalf("claimCtx.Err after parent cancel = %v, want nil", err)
	}
}

func TestQueueListRunningClaimedBy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-19T12:00:00.000Z"
	projectID := "project_list_claimed"
	loopID := "loop_list_claimed"
	claimedBy := "scheduler"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "ListClaimed", RepoPath: "/tmp/list-claimed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert: %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "queued", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert: %v", err)
	}
	for _, id := range []string{"qi_list_a", "qi_list_b"} {
		if err := repos.Queue.Upsert(ctx, QueueItemRecord{
			ID: id, ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
			DedupeKey: id, Priority: 1, Status: "running", AvailableAt: now, MaxAttempts: 3,
			ClaimedBy: &claimedBy, ClaimedAt: &now, StartedAt: &now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("Queue.Upsert(%s): %v", id, err)
		}
	}
	// Different claimed_at must not match.
	otherAt := "2026-07-19T11:00:00.000Z"
	if err := repos.Queue.Upsert(ctx, QueueItemRecord{
		ID: "qi_list_other", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: projectID,
		DedupeKey: "qi_list_other", Priority: 1, Status: "running", AvailableAt: now, MaxAttempts: 3,
		ClaimedBy: &claimedBy, ClaimedAt: &otherAt, StartedAt: &otherAt, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Queue.Upsert(other): %v", err)
	}

	items, err := repos.Queue.ListRunningClaimedBy(ctx, claimedBy, now)
	if err != nil {
		t.Fatalf("ListRunningClaimedBy: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2; items=%#v", len(items), items)
	}
	if items[0].ID != "qi_list_a" || items[1].ID != "qi_list_b" {
		t.Fatalf("order = %s,%s want qi_list_a,qi_list_b", items[0].ID, items[1].ID)
	}
}

func TestQueueStatsAndCleanupStaleQueued(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())
	now := "2026-04-11T12:00:00.000Z"
	projectID := "project_queue_stats"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	for i, loop := range []LoopRecord{
		{ID: "loop_eligible", Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_future", Seq: 2, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_terminal", Seq: 3, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "completed", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_lock_wait", Seq: 4, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_lock_running", Seq: 5, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_reviewer", Seq: 6, ProjectID: projectID, Type: "reviewer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_fixer", Seq: 7, ProjectID: projectID, Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: now, UpdatedAt: now},
		{ID: "loop_stale", Seq: 8, ProjectID: projectID, Type: "worker", TargetType: "project", Status: "terminated", CreatedAt: now, UpdatedAt: now},
	} {
		loop.Seq = int64(i + 1)
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	lockKey := "repo:acme/looper"
	repo := "acme/looper"
	prNumber := int64(42)
	for _, item := range []QueueItemRecord{
		{ID: "qi_eligible", ProjectID: &projectID, LoopID: strPtr("loop_eligible"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "eligible", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_future", ProjectID: &projectID, LoopID: strPtr("loop_future"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "future", Priority: 1, Status: "queued", AvailableAt: "2026-04-11T12:10:00.000Z", MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_terminal", ProjectID: &projectID, LoopID: strPtr("loop_terminal"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "terminal", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_lock_running", ProjectID: &projectID, LoopID: strPtr("loop_lock_running"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "lock-running", Priority: 1, Status: "running", AvailableAt: now, LockKey: &lockKey, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_lock_wait", ProjectID: &projectID, LoopID: strPtr("loop_lock_wait"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "lock-wait", Priority: 1, Status: "queued", AvailableAt: now, LockKey: &lockKey, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_reviewer", ProjectID: &projectID, LoopID: strPtr("loop_reviewer"), Type: "reviewer", TargetType: "pull_request", TargetID: "acme/looper#42", Repo: &repo, PRNumber: &prNumber, DedupeKey: "reviewer", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_fixer", ProjectID: &projectID, LoopID: strPtr("loop_fixer"), Type: "fixer", TargetType: "pull_request", TargetID: "acme/looper#42", Repo: &repo, PRNumber: &prNumber, DedupeKey: "fixer", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_stale", ProjectID: &projectID, LoopID: strPtr("loop_stale"), Type: "worker", TargetType: "project", TargetID: "project_queue_stats", DedupeKey: "stale", Priority: 1, Status: "queued", AvailableAt: now, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	stats, err := repos.Queue.Stats(ctx, now)
	if err != nil {
		t.Fatalf("Queue.Stats() error = %v", err)
	}
	if stats.TotalQueued != 7 || stats.EligibleQueued != 2 || stats.BlockedByTerminalOrPausedLoop != 2 || stats.BlockedByLockKey != 1 || stats.BlockedByReviewerFixerDependency != 1 || stats.ScheduledForFuture != 1 || stats.StaleQueued != 2 {
		t.Fatalf("Queue.Stats() = %#v, want total=7 eligible=2 terminal=2 lock=1 dependency=1 future=1 stale=2", stats)
	}

	cleaned, err := repos.Queue.CleanupStaleQueued(ctx, now, "stale queue item attached to terminal loop")
	if err != nil {
		t.Fatalf("Queue.CleanupStaleQueued() error = %v", err)
	}
	if cleaned != 2 {
		t.Fatalf("Queue.CleanupStaleQueued() = %d, want 2", cleaned)
	}
	for _, id := range []string{"qi_terminal", "qi_stale"} {
		item, err := repos.Queue.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("Queue.GetByID(%s) error = %v", id, err)
		}
		if item == nil || item.Status != "cancelled" || item.LastErrorKind == nil || *item.LastErrorKind != "non_retryable" {
			t.Fatalf("Queue.GetByID(%s) after cleanup = %#v, want cancelled non_retryable", id, item)
		}
	}
}

func TestLoopsListFilteredAndCountFiltered(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	coordinator := openMigratedCoordinatorForRepositories(t)
	repos := NewRepositories(coordinator.DB())

	for _, project := range []ProjectRecord{
		{ID: "project_a", Name: "A", RepoPath: "/tmp/a", CreatedAt: "2026-04-11T12:00:00.000Z", UpdatedAt: "2026-04-11T12:00:00.000Z"},
		{ID: "project_b", Name: "B", RepoPath: "/tmp/b", CreatedAt: "2026-04-11T12:00:00.000Z", UpdatedAt: "2026-04-11T12:00:00.000Z"},
	} {
		if err := repos.Projects.Upsert(ctx, project); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", project.ID, err)
		}
	}

	// Distinct updated_at so ORDER BY updated_at DESC, seq DESC is deterministic.
	loops := []LoopRecord{
		{ID: "loop_1", Seq: 1, ProjectID: "project_a", Type: "worker", TargetType: "project", Status: "running", CreatedAt: "2026-04-11T12:00:01.000Z", UpdatedAt: "2026-04-11T12:00:01.000Z"},
		{ID: "loop_2", Seq: 2, ProjectID: "project_a", Type: "worker", TargetType: "project", Status: "paused", CreatedAt: "2026-04-11T12:00:02.000Z", UpdatedAt: "2026-04-11T12:00:02.000Z"},
		{ID: "loop_3", Seq: 3, ProjectID: "project_a", Type: "worker", TargetType: "project", Status: "running", CreatedAt: "2026-04-11T12:00:03.000Z", UpdatedAt: "2026-04-11T12:00:03.000Z"},
		{ID: "loop_4", Seq: 4, ProjectID: "project_b", Type: "worker", TargetType: "project", Status: "running", CreatedAt: "2026-04-11T12:00:04.000Z", UpdatedAt: "2026-04-11T12:00:04.000Z"},
		{ID: "loop_5", Seq: 5, ProjectID: "project_b", Type: "worker", TargetType: "project", Status: "failed", CreatedAt: "2026-04-11T12:00:05.000Z", UpdatedAt: "2026-04-11T12:00:05.000Z"},
	}
	for _, loop := range loops {
		if err := repos.Loops.Upsert(ctx, loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}

	all, err := repos.Loops.ListFiltered(ctx, ListLoopsOptions{})
	if err != nil {
		t.Fatalf("ListFiltered(unlimited) error = %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("ListFiltered(unlimited) len = %d, want 5", len(all))
	}
	if all[0].ID != "loop_5" || all[4].ID != "loop_1" {
		t.Fatalf("ListFiltered(unlimited) order = %v, want newest-first", loopIDs(all))
	}

	total, err := repos.Loops.CountFiltered(ctx, ListLoopsOptions{})
	if err != nil {
		t.Fatalf("CountFiltered(all) error = %v", err)
	}
	if total != 5 {
		t.Fatalf("CountFiltered(all) = %d, want 5", total)
	}

	page, err := repos.Loops.ListFiltered(ctx, ListLoopsOptions{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("ListFiltered(limit/offset) error = %v", err)
	}
	if got := loopIDs(page); !reflect.DeepEqual(got, []string{"loop_4", "loop_3"}) {
		t.Fatalf("ListFiltered(limit=2,offset=1) = %v, want [loop_4 loop_3]", got)
	}

	// Offset without limit must still skip rows (SQLite LIMIT -1 OFFSET n).
	offsetOnly, err := repos.Loops.ListFiltered(ctx, ListLoopsOptions{Offset: 2})
	if err != nil {
		t.Fatalf("ListFiltered(offset-only) error = %v", err)
	}
	if got := loopIDs(offsetOnly); !reflect.DeepEqual(got, []string{"loop_3", "loop_2", "loop_1"}) {
		t.Fatalf("ListFiltered(offset=2) = %v, want [loop_3 loop_2 loop_1]", got)
	}

	running, err := repos.Loops.ListFiltered(ctx, ListLoopsOptions{Status: "running"})
	if err != nil {
		t.Fatalf("ListFiltered(status) error = %v", err)
	}
	if got := loopIDs(running); !reflect.DeepEqual(got, []string{"loop_4", "loop_3", "loop_1"}) {
		t.Fatalf("ListFiltered(status=running) = %v, want [loop_4 loop_3 loop_1]", got)
	}
	runningTotal, err := repos.Loops.CountFiltered(ctx, ListLoopsOptions{Status: "running"})
	if err != nil {
		t.Fatalf("CountFiltered(status) error = %v", err)
	}
	if runningTotal != 3 {
		t.Fatalf("CountFiltered(status=running) = %d, want 3", runningTotal)
	}

	projectB, err := repos.Loops.ListFiltered(ctx, ListLoopsOptions{ProjectID: "project_b"})
	if err != nil {
		t.Fatalf("ListFiltered(projectId) error = %v", err)
	}
	if got := loopIDs(projectB); !reflect.DeepEqual(got, []string{"loop_5", "loop_4"}) {
		t.Fatalf("ListFiltered(projectId=project_b) = %v, want [loop_5 loop_4]", got)
	}

	combined, err := repos.Loops.ListFiltered(ctx, ListLoopsOptions{Status: "running", ProjectID: "project_a", Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("ListFiltered(combined) error = %v", err)
	}
	if got := loopIDs(combined); !reflect.DeepEqual(got, []string{"loop_3"}) {
		t.Fatalf("ListFiltered(status+project+limit) = %v, want [loop_3]", got)
	}
	combinedTotal, err := repos.Loops.CountFiltered(ctx, ListLoopsOptions{Status: "running", ProjectID: "project_a"})
	if err != nil {
		t.Fatalf("CountFiltered(combined) error = %v", err)
	}
	if combinedTotal != 2 {
		t.Fatalf("CountFiltered(status+project) = %d, want 2", combinedTotal)
	}
}

func loopIDs(items []LoopRecord) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestRunsUpsertPreservesAgentSnapshotOnUpdate(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-18T12:00:00.000Z"
	baseBranch := "main"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID: "project_snapshot", Name: "Snapshot", RepoPath: "/tmp/snapshot", BaseBranch: &baseBranch,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID: "loop_snapshot", Seq: 1, ProjectID: "project_snapshot", Type: "worker",
		TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	snapshot := `{"vendor":"codex","model":"gpt-5","profileId":"fast"}`
	if err := repos.Runs.Upsert(ctx, RunRecord{
		ID: "run_snapshot", LoopID: "loop_snapshot", Status: "running",
		StartedAt: now, CreatedAt: now, UpdatedAt: now, AgentSnapshotJSON: &snapshot,
	}); err != nil {
		t.Fatalf("Runs.Upsert(insert) error = %v", err)
	}
	got, err := repos.Runs.GetByID(ctx, "run_snapshot")
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got == nil || got.AgentSnapshotJSON == nil || *got.AgentSnapshotJSON != snapshot {
		t.Fatalf("after insert AgentSnapshotJSON = %#v, want %q", got, snapshot)
	}

	// Subsequent upsert without snapshot must not clobber the insert-only column.
	if err := repos.Runs.Upsert(ctx, RunRecord{
		ID: "run_snapshot", LoopID: "loop_snapshot", Status: "success",
		StartedAt: now, CreatedAt: now, UpdatedAt: now, Summary: strPtr("done"),
	}); err != nil {
		t.Fatalf("Runs.Upsert(update) error = %v", err)
	}
	got, err = repos.Runs.GetByID(ctx, "run_snapshot")
	if err != nil {
		t.Fatalf("GetByID() after update error = %v", err)
	}
	if got == nil || got.Status != "success" {
		t.Fatalf("after update status = %#v, want success", got)
	}
	if got.AgentSnapshotJSON == nil || *got.AgentSnapshotJSON != snapshot {
		t.Fatalf("after update AgentSnapshotJSON = %#v, want preserved %q", got.AgentSnapshotJSON, snapshot)
	}
}

func TestRunRepositoryProjectionIgnoresAdditionalColumn(t *testing.T) {
	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	now := "2026-07-31T00:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{
		ID: "project_projection", Name: "Projection", RepoPath: "/tmp/projection",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{
		ID: "loop_projection", Seq: 1, ProjectID: "project_projection", Type: "worker",
		TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{
		ID: "run_projection", LoopID: "loop_projection", Status: "running",
		StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	// A forward-compatible schema change appends a column that is not part of
	// the record scan contract. A wildcard projection would now return one value too many.
	if _, err := coordinator.DB().ExecContext(ctx, `ALTER TABLE runs ADD COLUMN future_runtime_column TEXT`); err != nil {
		t.Fatalf("ALTER TABLE runs error = %v", err)
	}
	got, err := repos.Runs.GetByID(ctx, "run_projection")
	if err != nil {
		t.Fatalf("Runs.GetByID() after additional column error = %v", err)
	}
	if got == nil || got.ID != "run_projection" || got.LoopID != "loop_projection" {
		t.Fatalf("Runs.GetByID() = %#v, want run_projection linked to loop_projection", got)
	}
}

func strPtr(value string) *string {
	return &value
}

func openMigratedCoordinatorForRepositories(t *testing.T) *SQLiteCoordinator {
	t.Helper()

	root := t.TempDir()
	coordinator, err := OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), SQLiteCoordinatorOptions{
		Migrations: EmbeddedMigrations,
		BackupDir:  filepath.Join(root, "backups"),
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := coordinator.Close(); closeErr != nil {
			t.Fatalf("coordinator.Close() error = %v", closeErr)
		}
	})

	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("MigrationRunner.RunPending() error = %v", err)
	}

	return coordinator
}

func TestRunNewerBreaksTimestampTiesBySeq(t *testing.T) {
	t.Parallel()

	older := RunRecord{ID: "z_first", StartedAt: "2026-04-11T12:00:01.000Z", CreatedAt: "2026-04-11T12:00:01.000Z", Seq: 1}
	newer := RunRecord{ID: "a_second", StartedAt: "2026-04-11T12:00:01.000Z", CreatedAt: "2026-04-11T12:00:01.000Z", Seq: 2}
	if !RunNewer(newer, older) {
		t.Fatal("RunNewer(newer, older) = false, want true for higher seq on tied timestamps")
	}
	if RunNewer(older, newer) {
		t.Fatal("RunNewer(older, newer) = true, want false for lower seq on tied timestamps")
	}

	laterStart := RunRecord{ID: "b", StartedAt: "2026-04-11T12:00:02.000Z", CreatedAt: "2026-04-11T12:00:02.000Z", Seq: 1}
	if !RunNewer(laterStart, newer) {
		t.Fatal("RunNewer(laterStart, newer) = false, want true: timestamps outrank seq")
	}
}

func TestQueueStickySnapshotClaimInspectsNewestTiedRun(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_sticky", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_sticky_eligible", Seq: 1, ProjectID: "project_sticky", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_sticky_eligible) error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_sticky_ineligible", Seq: 2, ProjectID: "project_sticky", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(loop_sticky_ineligible) error = %v", err)
	}

	// Same-millisecond attempts: only the newest insert decides eligibility.
	// Eligible loop: newest insert failed with a snapshot; older tied insert
	// completed without one. Ineligible loop: the mirror image.
	snapshot := `{"vendor":"claude"}`
	tieAt := "2026-04-11T11:59:00.000Z"
	for _, run := range []RunRecord{
		{ID: "z_done", LoopID: "loop_sticky_eligible", Status: "completed", StartedAt: tieAt, CreatedAt: tieAt, UpdatedAt: tieAt},
		{ID: "a_retry", LoopID: "loop_sticky_eligible", Status: "failed", AgentSnapshotJSON: &snapshot, StartedAt: tieAt, CreatedAt: tieAt, UpdatedAt: tieAt},
		{ID: "z_retry", LoopID: "loop_sticky_ineligible", Status: "failed", AgentSnapshotJSON: &snapshot, StartedAt: tieAt, CreatedAt: tieAt, UpdatedAt: tieAt},
		{ID: "a_done", LoopID: "loop_sticky_ineligible", Status: "completed", StartedAt: tieAt, CreatedAt: tieAt, UpdatedAt: tieAt},
	} {
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			t.Fatalf("Runs.Upsert(%s) error = %v", run.ID, err)
		}
	}

	loopEligible := "loop_sticky_eligible"
	loopIneligible := "loop_sticky_ineligible"
	for _, item := range []QueueItemRecord{
		{ID: "qi_eligible", LoopID: &loopEligible, Type: "worker", TargetType: "project", TargetID: "project_sticky", DedupeKey: "d_sticky_eligible", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_ineligible", LoopID: &loopIneligible, Type: "worker", TargetType: "project", TargetID: "project_sticky", DedupeKey: "d_sticky_ineligible", Priority: 2, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	claimed, err := repos.Queue.ClaimNextNonLongTermRetryAmongTypeSets(ctx, now, "worker-a", nil, []string{"worker"})
	if err != nil {
		t.Fatalf("ClaimNextNonLongTermRetryAmongTypeSets() error = %v", err)
	}
	if claimed == nil || claimed.ID != "qi_eligible" {
		t.Fatalf("ClaimNextNonLongTermRetryAmongTypeSets() = %#v, want qi_eligible", claimed)
	}

	second, err := repos.Queue.ClaimNextNonLongTermRetryAmongTypeSets(ctx, now, "worker-a", nil, []string{"worker"})
	if err != nil {
		t.Fatalf("second ClaimNextNonLongTermRetryAmongTypeSets() error = %v", err)
	}
	if second != nil {
		t.Fatalf("second ClaimNextNonLongTermRetryAmongTypeSets() = %#v, want nil: newest tied run is completed", second)
	}
}

// TestQueueClaimStickyOnlyProjectScopePreservesQuarantinedRetries verifies that
// stickyOnlyProjectIDs admits sticky-snapshot retries for quarantined projects
// (absent from the runnable project list) without admitting fresh work for
// them. A quarantined project's fresh item must stay unclaimed even when the
// role is unrestricted, while its sticky retry is claimed; a runnable project's
// fresh item is claimed normally.
func TestQueueClaimStickyOnlyProjectScopePreservesQuarantinedRetries(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	for _, pid := range []string{"project_runnable", "project_quarantined"} {
		if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: pid, Name: pid, RepoPath: "/tmp/" + pid, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", pid, err)
		}
	}

	snapshot := `{"vendor":"claude"}`
	// Quarantined project loop: latest run is a failed sticky snapshot.
	quarantinedLoop := "loop_quarantined_sticky"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: quarantinedLoop, Seq: 1, ProjectID: "project_quarantined", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(quarantined) error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_quarantined_failed", LoopID: quarantinedLoop, Status: "failed", AgentSnapshotJSON: &snapshot, StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert(quarantined) error = %v", err)
	}
	// Quarantined project fresh loop: latest run completed (no sticky snapshot).
	quarantinedFreshLoop := "loop_quarantined_fresh"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: quarantinedFreshLoop, Seq: 2, ProjectID: "project_quarantined", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(quarantined_fresh) error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_quarantined_done", LoopID: quarantinedFreshLoop, Status: "completed", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert(quarantined_fresh) error = %v", err)
	}
	// Runnable project fresh loop: latest run completed (fresh work claimable).
	runnableFreshLoop := "loop_runnable_fresh"
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: runnableFreshLoop, Seq: 3, ProjectID: "project_runnable", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert(runnable_fresh) error = %v", err)
	}
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_runnable_done", LoopID: runnableFreshLoop, Status: "completed", StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert(runnable_fresh) error = %v", err)
	}

	quarantinedProjectID := "project_quarantined"
	runnableProjectID := "project_runnable"
	for _, item := range []QueueItemRecord{
		{ID: "qi_quarantined_sticky", ProjectID: &quarantinedProjectID, LoopID: &quarantinedLoop, Type: "worker", TargetType: "project", TargetID: "project_quarantined", DedupeKey: "d_q_sticky", Priority: 1, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_quarantined_fresh", ProjectID: &quarantinedProjectID, LoopID: &quarantinedFreshLoop, Type: "worker", TargetType: "project", TargetID: "project_quarantined", DedupeKey: "d_q_fresh", Priority: 2, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: "qi_runnable_fresh", ProjectID: &runnableProjectID, LoopID: &runnableFreshLoop, Type: "worker", TargetType: "project", TargetID: "project_runnable", DedupeKey: "d_r_fresh", Priority: 3, Status: "queued", AvailableAt: now, Attempts: 0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	} {
		if err := repos.Queue.Upsert(ctx, item); err != nil {
			t.Fatalf("Queue.Upsert(%s) error = %v", item.ID, err)
		}
	}

	// Without stickyOnlyProjectIDs (the pre-fix behavior), the quarantined
	// project's sticky retry is excluded: only the runnable project's fresh
	// item is claimable.
	withoutSticky, err := repos.Queue.ClaimNextNonLongTermRetryAmongTypeSetsForProjects(ctx, now, "worker-a", []string{"worker"}, nil, []string{"fixer", "worker"}, []string{"project_runnable"}, nil)
	if err != nil {
		t.Fatalf("without-sticky claim error = %v", err)
	}
	if withoutSticky == nil || withoutSticky.ID != "qi_runnable_fresh" {
		t.Fatalf("without-sticky claim = %#v, want qi_runnable_fresh: quarantined sticky retry must be excluded when no sticky-only scope is provided", withoutSticky)
	}
	withoutStickySecond, err := repos.Queue.ClaimNextNonLongTermRetryAmongTypeSetsForProjects(ctx, now, "worker-a", []string{"worker"}, nil, []string{"fixer", "worker"}, []string{"project_runnable"}, nil)
	if err != nil {
		t.Fatalf("without-sticky second claim error = %v", err)
	}
	if withoutStickySecond != nil {
		t.Fatalf("without-sticky second claim = %#v, want nil: quarantined items must stay excluded without sticky-only scope", withoutStickySecond)
	}

	// With stickyOnlyProjectIDs preserving the quarantined project, its sticky
	// retry becomes claimable while fresh work for it stays blocked.
	claimed, err := repos.Queue.ClaimNextNonLongTermRetryAmongTypeSetsForProjects(ctx, now, "worker-b", []string{"worker"}, nil, []string{"fixer", "worker"}, []string{"project_runnable"}, []string{"project_quarantined"})
	if err != nil {
		t.Fatalf("ClaimNextNonLongTermRetryAmongTypeSetsForProjects() error = %v", err)
	}
	if claimed == nil || claimed.ID != "qi_quarantined_sticky" {
		t.Fatalf("first claim = %#v, want qi_quarantined_sticky: quarantined sticky retry must be claimable via sticky-only scope", claimed)
	}

	// The quarantined project's fresh item must NOT be claimed even with the
	// sticky-only scope: fresh work for quarantined projects is never admitted.
	second, err := repos.Queue.ClaimNextNonLongTermRetryAmongTypeSetsForProjects(ctx, now, "worker-b", []string{"worker"}, nil, []string{"fixer", "worker"}, []string{"project_runnable"}, []string{"project_quarantined"})
	if err != nil {
		t.Fatalf("second claim error = %v", err)
	}
	if second != nil {
		t.Fatalf("second claim = %#v, want nil: fresh work for a quarantined project must never be admitted", second)
	}
}

func TestRunsTouchHeartbeatOnlyMovesForward(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	now := "2026-04-11T12:00:00.000Z"
	if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: "project_touch_hb", Name: "Looper", RepoPath: "/tmp/looper", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	if err := repos.Loops.Upsert(ctx, LoopRecord{ID: "loop_touch_hb", Seq: 1, ProjectID: "project_touch_hb", Type: "worker", TargetType: "project", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	checkpoint := `{"resumePolicy":"always"}`
	if err := repos.Runs.Upsert(ctx, RunRecord{ID: "run_touch_hb", LoopID: "loop_touch_hb", Status: "running", CheckpointJSON: &checkpoint, StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	// NULL heartbeat: first touch sets it.
	t2 := "2026-04-11T12:00:02.000Z"
	if err := repos.Runs.TouchHeartbeat(ctx, "run_touch_hb", t2); err != nil {
		t.Fatalf("TouchHeartbeat(t2) error = %v", err)
	}
	run, err := repos.Runs.GetByID(ctx, "run_touch_hb")
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run.LastHeartbeatAt == nil || *run.LastHeartbeatAt != t2 {
		t.Fatalf("LastHeartbeatAt = %v, want %q", run.LastHeartbeatAt, t2)
	}

	// A stale writer cannot move liveness evidence backward.
	t1 := "2026-04-11T12:00:01.000Z"
	if err := repos.Runs.TouchHeartbeat(ctx, "run_touch_hb", t1); err != nil {
		t.Fatalf("TouchHeartbeat(t1) error = %v", err)
	}
	run, err = repos.Runs.GetByID(ctx, "run_touch_hb")
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run.LastHeartbeatAt == nil || *run.LastHeartbeatAt != t2 {
		t.Fatalf("LastHeartbeatAt after stale touch = %v, want %q retained", run.LastHeartbeatAt, t2)
	}
	if run.UpdatedAt != t2 {
		t.Fatalf("UpdatedAt after stale touch = %q, want %q retained", run.UpdatedAt, t2)
	}

	// Forward touch advances, and no other column is disturbed.
	t3 := "2026-04-11T12:00:03.000Z"
	if err := repos.Runs.TouchHeartbeat(ctx, "run_touch_hb", t3); err != nil {
		t.Fatalf("TouchHeartbeat(t3) error = %v", err)
	}
	run, err = repos.Runs.GetByID(ctx, "run_touch_hb")
	if err != nil || run == nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run.LastHeartbeatAt == nil || *run.LastHeartbeatAt != t3 {
		t.Fatalf("LastHeartbeatAt = %v, want %q", run.LastHeartbeatAt, t3)
	}
	if run.Status != "running" || run.CheckpointJSON == nil || *run.CheckpointJSON != checkpoint || run.StartedAt != now {
		t.Fatalf("TouchHeartbeat disturbed other columns: %#v", run)
	}
}

func TestEventsListLatestByEntityTypeAndEventTypesKeepsNewestPerEntity(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())

	pullRequest := "pull_request"
	issue := "github_issue"
	first := "acme/widgets#1"
	second := "acme/widgets#2"
	other := "acme/widgets#9"
	for _, event := range []EventLogRecord{
		{ID: "gate_1_old", EventType: "pull_request.merge_gate.evaluated", EntityType: &pullRequest, EntityID: &first, PayloadJSON: `{"n":1}`, CreatedAt: "2026-04-11T12:00:00.000Z"},
		{ID: "gate_1_new", EventType: "pull_request.merge_gate.evaluated", EntityType: &pullRequest, EntityID: &first, PayloadJSON: `{"n":2}`, CreatedAt: "2026-04-11T12:05:00.000Z"},
		{ID: "gate_2_z", EventType: "pull_request.merge_gate.evaluated", EntityType: &pullRequest, EntityID: &second, PayloadJSON: `{"n":3}`, CreatedAt: "2026-04-11T12:07:00.000Z"},
		// Same created_at as gate_2_z: the later insert wins even though its ID sorts first.
		{ID: "gate_2_a", EventType: "pull_request.merge_gate.evaluated", EntityType: &pullRequest, EntityID: &second, PayloadJSON: `{"n":4}`, CreatedAt: "2026-04-11T12:07:00.000Z"},
		// Neither the other event type nor the other entity type may leak in.
		{ID: "gate_1_other_type", EventType: "pull_request.snapshot.captured", EntityType: &pullRequest, EntityID: &first, PayloadJSON: `{"n":5}`, CreatedAt: "2026-04-11T12:09:00.000Z"},
		{ID: "gate_issue", EventType: "pull_request.merge_gate.evaluated", EntityType: &issue, EntityID: &other, PayloadJSON: `{"n":6}`, CreatedAt: "2026-04-11T12:09:00.000Z"},
	} {
		if err := repos.Events.Append(ctx, event); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", event.ID, err)
		}
	}

	latest, err := repos.Events.ListLatestByEntityTypeAndEventTypes(ctx, "pull_request", []string{"pull_request.merge_gate.evaluated"})
	if err != nil {
		t.Fatalf("Events.ListLatestByEntityTypeAndEventTypes() error = %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("len(Events.ListLatestByEntityTypeAndEventTypes()) = %d, want 2: %#v", len(latest), latest)
	}
	if latest[0].ID != "gate_1_new" || latest[1].ID != "gate_2_a" {
		t.Fatalf("Events.ListLatestByEntityTypeAndEventTypes() = [%s %s], want [gate_1_new gate_2_a]", latest[0].ID, latest[1].ID)
	}

	empty, err := repos.Events.ListLatestByEntityTypeAndEventTypes(ctx, "pull_request", nil)
	if err != nil {
		t.Fatalf("Events.ListLatestByEntityTypeAndEventTypes(no types) error = %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("Events.ListLatestByEntityTypeAndEventTypes(no types) = %#v, want none", empty)
	}
}

func TestEventsListLatestPartitionsByProjectAndEntity(t *testing.T) {
	t.Parallel()

	coordinator := openMigratedCoordinatorForRepositories(t)
	ctx := context.Background()
	repos := NewRepositories(coordinator.DB())
	pullRequest, entityID := "pull_request", "acme/widgets#42"
	projectOne, projectTwo := "project-one", "project-two"
	for _, projectID := range []string{projectOne, projectTwo} {
		if err := repos.Projects.Upsert(ctx, ProjectRecord{ID: projectID, Name: projectID, RepoPath: "/tmp/" + projectID, CreatedAt: "2026-08-02T09:00:00.000Z", UpdatedAt: "2026-08-02T09:00:00.000Z"}); err != nil {
			t.Fatalf("Projects.Upsert(%s) error = %v", projectID, err)
		}
	}
	for _, event := range []EventLogRecord{
		{ID: "project_one_old", EventType: "pull_request.merge_gate.evaluated", ProjectID: &projectOne, EntityType: &pullRequest, EntityID: &entityID, PayloadJSON: `{"project":"one-old"}`, CreatedAt: "2026-08-02T10:00:00.000Z"},
		{ID: "project_one_new", EventType: "pull_request.merge_gate.evaluated", ProjectID: &projectOne, EntityType: &pullRequest, EntityID: &entityID, PayloadJSON: `{"project":"one-new"}`, CreatedAt: "2026-08-02T10:05:00.000Z"},
		{ID: "project_two_new", EventType: "pull_request.merge_gate.evaluated", ProjectID: &projectTwo, EntityType: &pullRequest, EntityID: &entityID, PayloadJSON: `{"project":"two-new"}`, CreatedAt: "2026-08-02T10:06:00.000Z"},
	} {
		if err := repos.Events.Append(ctx, event); err != nil {
			t.Fatalf("Events.Append(%s) error = %v", event.ID, err)
		}
	}

	latest, err := repos.Events.ListLatestByEntityTypeAndEventTypes(ctx, pullRequest, []string{"pull_request.merge_gate.evaluated"})
	if err != nil {
		t.Fatalf("ListLatestByEntityTypeAndEventTypes() error = %v", err)
	}
	if len(latest) != 2 || latest[0].ID != "project_one_new" || latest[1].ID != "project_two_new" {
		t.Fatalf("latest project/entity events = %#v, want one newest row per project", latest)
	}
}
