package upgraderestore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreRollsBackOrdinaryRenameFailure(t *testing.T) {
	fixture := newRestoreFixture(t, true)
	injectedErr := errors.New("injected config install failure")
	failed := false
	journalObserved := false
	operations := fileOperations{rename: func(sourcePath, targetPath string) error {
		if !failed && targetPath == fixture.configPath && strings.Contains(filepath.Base(sourcePath), ".looper-restore-stage-") {
			failed = true
			journalObserved = true
			encoded, err := os.ReadFile(JournalPath(fixture.databasePath))
			if err != nil {
				t.Fatalf("read journal before config rename: %v", err)
			}
			var journal restoreJournal
			if err := json.Unmarshal(encoded, &journal); err != nil {
				t.Fatalf("decode journal before config rename: %v", err)
			}
			if journal.Phase != phaseDatabaseInstalled {
				t.Fatalf("journal phase before config rename = %q, want %q", journal.Phase, phaseDatabaseInstalled)
			}
			assertJournalArtifactsBesideTargets(t, journal)
			return injectedErr
		}
		return os.Rename(sourcePath, targetPath)
	}}

	err := restore(fixture.bundleDirectory, fixture.source, operations)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("restore() error = %v, want injected failure", err)
	}
	if !failed || !journalObserved {
		t.Fatal("injected config rename failure was not reached with a durable journal")
	}
	fixture.assertOriginalTargets(t)
	assertFileMode(t, fixture.configPath, 0o640)
	assertFileMode(t, fixture.databasePath, 0o660)
	assertPathMissing(t, JournalPath(fixture.databasePath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.configPath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.databasePath))
}

func TestRecoverRollsBackUncommittedRestore(t *testing.T) {
	fixture := newRestoreFixture(t, true)
	crashed := false
	operations := fileOperations{rename: func(sourcePath, targetPath string) error {
		if targetPath == fixture.configPath && strings.Contains(filepath.Base(sourcePath), ".looper-restore-stage-") {
			panic("simulated process interruption")
		}
		return os.Rename(sourcePath, targetPath)
	}}
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				crashed = true
			}
		}()
		_ = restore(fixture.bundleDirectory, fixture.source, operations)
	}()
	if !crashed {
		t.Fatal("restore did not reach simulated interruption")
	}
	assertFileContents(t, fixture.databasePath, "new database")
	assertPathMissing(t, fixture.configPath)
	if _, err := os.Lstat(JournalPath(fixture.databasePath)); err != nil {
		t.Fatalf("journal after interruption: %v", err)
	}

	if err := Recover(JournalPath(fixture.databasePath)); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	fixture.assertOriginalTargets(t)
	assertPathMissing(t, JournalPath(fixture.databasePath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.configPath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.databasePath))
	if err := Recover(JournalPath(fixture.databasePath)); err != nil {
		t.Fatalf("second Recover() error = %v", err)
	}
}

func TestRecoverCleansCommittedRestoreWithoutRollingItBack(t *testing.T) {
	fixture := newRestoreFixture(t, true)
	crashed := false
	journalPath := JournalPath(fixture.databasePath)
	operations := fileOperations{rename: func(sourcePath, targetPath string) error {
		if targetPath == journalPath && strings.Contains(filepath.Base(sourcePath), ".looper-restore-journal-write-") {
			encoded, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatalf("read pending journal update: %v", err)
			}
			var journal restoreJournal
			if err := json.Unmarshal(encoded, &journal); err != nil {
				t.Fatalf("decode pending journal update: %v", err)
			}
			if journal.Phase == phaseCommitted {
				if err := os.Rename(sourcePath, targetPath); err != nil {
					t.Fatalf("publish simulated committed journal: %v", err)
				}
				panic("simulated interruption after commit")
			}
		}
		return os.Rename(sourcePath, targetPath)
	}}
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				crashed = true
			}
		}()
		_ = restore(fixture.bundleDirectory, fixture.source, operations)
	}()
	if !crashed {
		t.Fatal("restore did not reach simulated committed interruption")
	}

	if err := Recover(journalPath); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	assertFileContents(t, fixture.configPath, "new config")
	assertFileContents(t, fixture.databasePath, "new database")
	assertPathMissing(t, fixture.databasePath+"-wal")
	assertPathMissing(t, fixture.databasePath+"-shm")
	assertPathMissing(t, journalPath)
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.configPath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.databasePath))
}

func assertJournalArtifactsBesideTargets(t *testing.T, journal restoreJournal) {
	t.Helper()
	if len(journal.Entries) != 4 {
		t.Fatalf("journal entries = %d, want 4", len(journal.Entries))
	}
	for _, entry := range journal.Entries {
		if filepath.Dir(entry.UndoPath) != filepath.Dir(entry.TargetPath) {
			t.Errorf("undo path %s is not beside target %s", entry.UndoPath, entry.TargetPath)
		}
		if entry.StagedPath != "" && filepath.Dir(entry.StagedPath) != filepath.Dir(entry.TargetPath) {
			t.Errorf("staged path %s is not beside target %s", entry.StagedPath, entry.TargetPath)
		}
	}
}
