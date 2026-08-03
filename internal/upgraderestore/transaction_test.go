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
	operations := fileOperations{
		link: func(string, string) error { return errors.New("force exclusive path") },
		exclusivePublish: func(sourcePath, targetPath string) error {
			if !failed && targetPath == fixture.configPath && strings.Contains(filepath.Base(sourcePath), ".looper-restore-stage-") {
				failed = true
				journalObserved = true
				encoded, err := os.ReadFile(JournalPath(fixture.databasePath))
				if err != nil {
					t.Fatalf("read journal before config publish: %v", err)
				}
				var journal restoreJournal
				if err := json.Unmarshal(encoded, &journal); err != nil {
					t.Fatalf("decode journal before config publish: %v", err)
				}
				if journal.Phase != phaseDatabaseInstalled {
					t.Fatalf("journal phase before config publish = %q, want %q", journal.Phase, phaseDatabaseInstalled)
				}
				assertJournalArtifactsBesideTargets(t, journal)
				return injectedErr
			}
			return copyFileExclusive(sourcePath, targetPath)
		},
	}

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
	operations := fileOperations{
		link: func(string, string) error { return errors.New("force exclusive path") },
		exclusivePublish: func(sourcePath, targetPath string) error {
			if targetPath == fixture.configPath && strings.Contains(filepath.Base(sourcePath), ".looper-restore-stage-") {
				return injectedErr
			}
			return copyFileExclusive(sourcePath, targetPath)
		},
	}
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
	operations := fileOperations{
		link: func(string, string) error { return errors.New("force exclusive path") },
		exclusivePublish: func(sourcePath, targetPath string) error {
			if targetPath == fixture.configPath && strings.Contains(filepath.Base(sourcePath), ".looper-restore-stage-") {
				panic("simulated process interruption")
			}
			return copyFileExclusive(sourcePath, targetPath)
		},
	}
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

func TestRecoverCleansUndoAlreadyInstalledBeforeSourceRemoval(t *testing.T) {
	fixture := newRestoreFixture(t, true)
	type targetSpec struct {
		name string
		path string
		old  string
		mode os.FileMode
	}
	specs := []targetSpec{
		{name: entryDatabase, path: fixture.databasePath, old: "old database", mode: 0o660},
		{name: entryWAL, path: fixture.databasePath + "-wal", old: "old wal", mode: 0o604},
		{name: entrySHM, path: fixture.databasePath + "-shm", old: "old shm", mode: 0o620},
		{name: entryConfig, path: fixture.configPath, old: "old config", mode: 0o640},
	}
	entries := make([]journalEntry, 0, len(specs))
	for _, spec := range specs {
		undo := filepath.Join(filepath.Dir(spec.path), "."+filepath.Base(spec.path)+".looper-restore-undo-retry")
		if err := os.Rename(spec.path, undo); err != nil {
			t.Fatal(err)
		}
		entry := journalEntry{Name: spec.name, TargetPath: spec.path, UndoPath: undo, HadOriginal: true}
		if spec.name == entryDatabase || spec.name == entryConfig {
			stage := filepath.Join(filepath.Dir(spec.path), "."+filepath.Base(spec.path)+".looper-restore-stage-retry")
			writeTestFile(t, stage, "staged", 0o600)
			entry.StagedPath = stage
			entry.StagedSHA256, entry.StagedSize, _ = fileContentIdentity(stage)
			// Simulate an interruption after the undo was already copied back
			// but before the old source was removed.
			writeTestFile(t, spec.path, spec.old, spec.mode)
		}
		entries = append(entries, entry)
	}
	journalPath := JournalPath(fixture.databasePath)
	journal := restoreJournal{Version: journalVersion, Phase: phaseOriginalsMoved, Entries: entries}
	if err := createJournal(journalPath, journal); err != nil {
		t.Fatal(err)
	}

	if err := Recover(journalPath); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	fixture.assertOriginalTargets(t)
	assertPathMissing(t, journalPath)
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.configPath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.databasePath))
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

func TestRestorePreservesCommittedArtifactsWhenTargetDriftsBeforeCleanup(t *testing.T) {
	fixture := newRestoreFixture(t, true)
	journalPath := JournalPath(fixture.databasePath)
	drifted := false
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
					t.Fatalf("publish committed journal: %v", err)
				}
				// Simulate an external writer changing the live target after the
				// committed phase is durable but before cleanup starts.
				writeTestFile(t, fixture.databasePath, "external database", 0o660)
				drifted = true
				return nil
			}
		}
		return os.Rename(sourcePath, targetPath)
	}}

	err := restore(fixture.bundleDirectory, fixture.source, operations)
	if err == nil {
		t.Fatal("restore() error = nil, want committed-target drift failure")
	}
	if !strings.Contains(err.Error(), "cleanup is unsafe") {
		t.Fatalf("restore() error = %v, want cleanup safety failure", err)
	}
	if !drifted {
		t.Fatal("committed target drift was not injected")
	}
	assertFileContents(t, fixture.databasePath, "external database")
	assertFileContents(t, fixture.configPath, "new config")
	if _, err := os.Lstat(journalPath); err != nil {
		t.Fatalf("journal after committed-target drift: %v", err)
	}
	var journal restoreJournal
	encoded, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read preserved journal: %v", err)
	}
	if err := json.Unmarshal(encoded, &journal); err != nil {
		t.Fatalf("decode preserved journal: %v", err)
	}
	if journal.Phase != phaseCommitted {
		t.Fatalf("preserved journal phase = %q, want %q", journal.Phase, phaseCommitted)
	}
	for _, entry := range journal.Entries {
		if entry.Name == entryDatabase || entry.Name == entryConfig {
			if _, err := os.Lstat(entry.UndoPath); err != nil {
				t.Fatalf("preserved undo %s: %v", entry.Name, err)
			}
		}
	}
}

func TestPublishNoReplaceSyncsDestinationBeforeRemovingSource(t *testing.T) {
	for _, forceCopy := range []bool{false, true} {
		name := "hard link"
		if forceCopy {
			name = "exclusive copy"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			source := filepath.Join(directory, "source")
			destination := filepath.Join(directory, "destination")
			writeTestFile(t, source, "restored bytes", 0o600)
			synced := false
			operations := fileOperations{
				link: func(sourcePath, targetPath string) error {
					if forceCopy {
						return errors.New("force exclusive copy")
					}
					return os.Link(sourcePath, targetPath)
				},
				syncDirectory: func(path string) error {
					if path != directory {
						t.Fatalf("syncDirectory path = %q, want %q", path, directory)
					}
					if _, err := os.Stat(source); err != nil {
						t.Fatalf("source at sync = %v, want source to remain until directory sync", err)
					}
					if _, err := os.Stat(destination); err != nil {
						t.Fatalf("destination at sync = %v", err)
					}
					synced = true
					return nil
				},
			}

			if err := publishNoReplace(operations, source, destination, "test restore"); err != nil {
				t.Fatalf("publishNoReplace() error = %v", err)
			}
			if !synced {
				t.Fatal("publishNoReplace() did not sync destination directory")
			}
			assertPathMissing(t, source)
			assertFileContents(t, destination, "restored bytes")
		})
	}
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
