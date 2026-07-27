package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// When core.ignoreCase=true, a tracked reserved fixture whose on-disk spelling
// differs only by case from the index must stay in the worktree through Prepare
// (icase index probe must not treat it as untracked scratch).
//
// Clean is intentionally not asserted: on case-sensitive filesystems Git still
// reports the index spelling as worktree-deleted (" D") even with
// core.ignoreCase=true and a case-folded basename present. That is ordinary
// tracked path dirt (M/D stays visible), not reserved-scratch dirt. On
// case-insensitive filesystems the rename is a no-op identity and status is
// often clean — both outcomes are fine as long as the payload is not quarantined.
func TestGatewayPrepareKeepsCaseOnlyTrackedReservedFixture(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-case-tracked-fixture"
	fixture.createRemoteRepo(t, branch)
	gateway := fixture.gateway()

	runGit(t, fixture.repoPath, "checkout", branch)
	trackedLower := ".looper-review-tracked.json"
	trackedPayload := `{"fixture":"tracked"}` + "\n"
	writeFile(t, filepath.Join(fixture.repoPath, trackedLower), trackedPayload)
	runGit(t, fixture.repoPath, "add", trackedLower)
	runGit(t, fixture.repoPath, "commit", "-m", "seed tracked reserved fixture")
	runGit(t, fixture.repoPath, "push", "origin", branch)
	runGit(t, fixture.repoPath, "config", "core.ignoreCase", "true")
	runGit(t, fixture.repoPath, "checkout", "main")

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1068,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	runGit(t, wt, "config", "core.ignoreCase", "true")

	// Case-only rename leaves the index spelling lowercase while the worktree
	// basename becomes uppercase (two-step rename for case-insensitive FS).
	trackedUpper := ".LOOPER-REVIEW-TRACKED.JSON"
	src := filepath.Join(wt, trackedLower)
	tmp := filepath.Join(wt, ".tmp-case-rename-tracked")
	if err := os.Rename(src, tmp); err != nil {
		t.Fatalf("rename to tmp: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(wt, trackedUpper)); err != nil {
		t.Fatalf("rename to upper: %v", err)
	}
	// Prove the case-sensitive literal probe misses while icase matches.
	present, err := gateway.isIndexPathPresent(ctx, wt, trackedUpper, false)
	if err != nil {
		t.Fatalf("isIndexPathPresent(literal upper) error = %v", err)
	}
	if present {
		// Some Git/FS pairs may still match; icase path is still required for
		// the ignoreCase=true contract exercised below.
		t.Log("literal upper already matched index; continuing with ignoreCase prepare")
	}
	present, err = gateway.isIndexPathPresent(ctx, wt, trackedUpper, true)
	if err != nil {
		t.Fatalf("isIndexPathPresent(icase upper) error = %v", err)
	}
	if !present {
		t.Fatal("isIndexPathPresent(icase upper) = false, want true for case-only tracked fixture")
	}

	if _, err := gateway.PrepareWorktree(ctx, PrepareWorktreeInput{WorktreePath: wt, Branch: branch}); err != nil {
		t.Fatalf("PrepareWorktree() error = %v", err)
	}
	// Tracked content must remain in the worktree under some EqualFold spelling.
	if alt := findCaseFoldedRootFile(t, wt, trackedLower); alt == "" {
		t.Fatal("tracked reserved fixture missing after Prepare; case-only match must not quarantine")
	} else {
		b, readErr := os.ReadFile(filepath.Join(wt, alt))
		if readErr != nil {
			t.Fatalf("read tracked fixture: %v", readErr)
		}
		if string(b) != trackedPayload {
			t.Fatalf("tracked payload = %q, want %q", b, trackedPayload)
		}
	}
	// Nothing from this fixture may land in quarantine.
	qdir := ReservedReviewScratchQuarantineDir(wt)
	if _, err := os.Stat(qdir); err == nil {
		if payloads := quarantinePayloadBytes(t, qdir); payloads[trackedPayload] {
			t.Fatalf("tracked reserved fixture was quarantined: %#v", payloads)
		}
	}
}

// Old-mtime scratch quarantined today must retain its recovery window after the
// worktree disappears outside CleanupWorktree (orphan prune must not use payload mtime).
func TestGatewayOrphanRetentionUsesQuarantineContainerAgeNotPayloadMtime(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	branch := "feature/review-orphan-container-age"
	fixture.createRemoteRepo(t, branch)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	gateway := New(Options{GitPath: "git", Repos: fixture.repos, Now: func() time.Time { return now }})

	worktree, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1069,
	})
	if err != nil {
		t.Fatalf("CreateWorktree() error = %v", err)
	}
	wt := worktree.WorktreePath
	payload := `{"body":"ancient then abandoned"}` + "\n"
	src := filepath.Join(wt, ".looper-review-1069.json")
	writeFile(t, src, payload)
	old := now.Add(-reservedReviewScratchQuarantineRetention - 48*time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatalf("Chtimes scratch: %v", err)
	}
	if err := gateway.relocateReservedReviewerScratch(ctx, wt); err != nil {
		t.Fatalf("relocate error = %v", err)
	}
	qdir := ReservedReviewScratchQuarantineDir(wt)
	if payloads := quarantinePayloadBytes(t, qdir); !payloads[payload] {
		t.Fatalf("expected quarantine before abandonment: %#v", payloads)
	}

	// Abandon the worktree without CleanupWorktree (operator rm / crash path).
	if err := os.RemoveAll(wt); err != nil {
		t.Fatalf("RemoveAll worktree: %v", err)
	}

	// Prepare/relocate elsewhere must not prune the freshly created orphan.
	other, err := gateway.CreateWorktree(ctx, CreateWorktreeInput{
		ProjectID: fixture.projectID, RepoPath: fixture.repoPath, WorktreeRoot: fixture.worktreeRoot,
		Branch: branch, BaseBranch: "main", PRNumber: 1070,
	})
	if err != nil {
		t.Fatalf("CreateWorktree(other) error = %v", err)
	}
	if err := gateway.relocateReservedReviewerScratch(ctx, other.WorktreePath); err != nil {
		t.Fatalf("relocate(other) error = %v", err)
	}
	if payloads := quarantinePayloadBytes(t, qdir); !payloads[payload] {
		t.Fatalf("fresh orphan quarantine pruned by payload mtime; want container-age retention: %#v", payloads)
	}
}
