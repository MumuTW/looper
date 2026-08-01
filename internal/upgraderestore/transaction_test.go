package upgraderestore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/upgradebackup"
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

func TestRestoreRefusesBundleMutatedAfterVerify(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	cli := filepath.Join(root, "looper")
	daemon := filepath.Join(root, "looperd")
	writeTestFile(t, configPath, "[server]\n", 0o600)
	writeTestFile(t, cli, "cli", 0o755)
	writeTestFile(t, daemon, "daemon", 0o755)
	result, err := upgradebackup.Create(context.Background(), upgradebackup.Input{
		RootDir: filepath.Join(root, "backups"), ConfigPath: configPath, DatabasePath: filepath.Join(root, "db.sqlite"),
		CLIBinaryPath: cli, DaemonBinaryPath: daemon,
		Snapshot: func(context.Context) (string, error) {
			path := filepath.Join(root, "snap.sqlite")
			writeTestFile(t, path, "sqlite-v1", 0o600)
			return path, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upgradebackup.Verify(result.Directory); err != nil {
		t.Fatalf("Verify before mutate: %v", err)
	}
	// Mutate after verify: restore must refuse rather than install torn bytes.
	if err := os.WriteFile(filepath.Join(result.Directory, "database.sqlite"), []byte("sqlite-MUTATED"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := upgradebackup.RestoreSource(result.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	source.ConfigPath = filepath.Join(root, "live-config.toml")
	source.DatabasePath = filepath.Join(root, "live.sqlite")
	err = Restore(result.Directory, source)
	if err == nil {
		t.Fatal("Restore() error = nil, want failure after bundle mutation")
	}
	// Verify may fail first, or staging may fail the manifest match — either is fail-closed.
	if !strings.Contains(err.Error(), "manifest") && !strings.Contains(err.Error(), "match") {
		t.Fatalf("Restore() error = %v, want manifest/match failure", err)
	}
}

func TestRestoreRollsBackMissingTargetUsingStagedIdentity(t *testing.T) {
	// Database target was absent; after it is installed from stage, config
	// install fails. Rollback must still remove the installed database using
	// the durable staged content hash (staged path is already gone).
	fixture := newRestoreFixture(t, false)
	injectedErr := errors.New("injected config install failure")
	operations := fileOperations{rename: func(sourcePath, targetPath string) error {
		if targetPath == fixture.configPath && strings.Contains(filepath.Base(sourcePath), ".looper-restore-stage-") {
			return injectedErr
		}
		return os.Rename(sourcePath, targetPath)
	}}
	err := restore(fixture.bundleDirectory, fixture.source, operations)
	if !errors.Is(err, injectedErr) {
		t.Fatalf("restore() error = %v, want injected failure", err)
	}
	assertPathMissing(t, fixture.databasePath)
	assertPathMissing(t, fixture.configPath)
	assertPathMissing(t, JournalPath(fixture.databasePath))
}

func TestDetachAndRemoveOwnedRestoreTargetRejectsContentSwapDuringDetach(t *testing.T) {
	// Ownership is re-checked after rename-aside so a TOCTOU recreate cannot
	// be deleted: rename a non-owned file, fail verify, put it back.
	dir := t.TempDir()
	target := filepath.Join(dir, "looper.sqlite")
	staged := filepath.Join(dir, "stage.sqlite")
	writeTestFile(t, staged, "staged-bytes", 0o660)
	writeTestFile(t, target, "staged-bytes", 0o660)
	hash, size, err := fileContentIdentity(staged)
	if err != nil {
		t.Fatal(err)
	}
	entry := journalEntry{Name: entryDatabase, TargetPath: target, StagedPath: staged, StagedSHA256: hash, StagedSize: size}
	// Replace target after identity is known but exercise detach on wrong content
	// by calling with a journal that expects staged while target was swapped.
	writeTestFile(t, target, "external-write", 0o660)
	err = detachAndRemoveOwnedRestoreTarget(entry, "database")
	if err == nil {
		t.Fatal("detachAndRemoveOwnedRestoreTarget() error = nil, want refuse external content")
	}
	assertFileContents(t, target, "external-write")
	// Owned path: matching content is detached and removed.
	writeTestFile(t, target, "staged-bytes", 0o660)
	if err := detachAndRemoveOwnedRestoreTarget(entry, "database"); err != nil {
		t.Fatal(err)
	}
	assertPathMissing(t, target)
}

func TestRollbackRefusesRecreatedTargetWhenUndoPresent(t *testing.T) {
	fixture := newRestoreFixture(t, true)
	// Build a prepared journal as if originals were moved to undo, then a new
	// file was created at the target before recovery.
	dbHash, dbSize, err := fileContentIdentity(filepath.Join(fixture.bundleDirectory, "database.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	cfgHash, cfgSize, err := fileContentIdentity(filepath.Join(fixture.bundleDirectory, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	dbUndo := fixture.databasePath + ".looper-restore-undo-test"
	cfgUndo := fixture.configPath + ".looper-restore-undo-test"
	if err := os.Rename(fixture.databasePath, dbUndo); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.configPath, cfgUndo); err != nil {
		t.Fatal(err)
	}
	// Recreated targets that are NOT the staged restore content.
	writeTestFile(t, fixture.databasePath, "recreated by app", 0o660)
	writeTestFile(t, fixture.configPath, "recreated by app", 0o640)
	journal := restoreJournal{
		Version: journalVersion,
		Phase:   phaseOriginalsMoved,
		Entries: []journalEntry{
			{Name: entryDatabase, TargetPath: fixture.databasePath, UndoPath: dbUndo, HadOriginal: true, StagedSHA256: dbHash, StagedSize: dbSize, StagedPath: filepath.Join(fixture.bundleDirectory, "database.sqlite")},
			{Name: entryConfig, TargetPath: fixture.configPath, UndoPath: cfgUndo, HadOriginal: true, StagedSHA256: cfgHash, StagedSize: cfgSize, StagedPath: filepath.Join(fixture.bundleDirectory, "config.toml")},
		},
	}
	journalPath := JournalPath(fixture.databasePath)
	if err := createJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}
	err = rollback(journalPath, journal, defaultFileOperations)
	if err == nil {
		t.Fatal("rollback() error = nil, want refuse recreated targets")
	}
	// Recreated files must remain; undo files must remain for operator recovery.
	assertFileContents(t, fixture.databasePath, "recreated by app")
	assertFileContents(t, fixture.configPath, "recreated by app")
	assertFileContents(t, dbUndo, "old database")
	assertFileContents(t, cfgUndo, "old config")
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
