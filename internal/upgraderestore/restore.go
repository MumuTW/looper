// Package upgraderestore restores the configuration and SQLite database from
// a verified upgrade backup bundle using a crash-recoverable file transaction.
//
// # Why a durable multi-phase journal exists
//
// Restore swaps live config + database paths that the operator needs after a
// failed cutover. A naive "copy then rename once" is insufficient: a crash
// between moving the original database aside and installing the bundle copy
// leaves both the old and new trees incomplete, and a pure fail-loud approach
// cannot re-enter the half-finished state without an operator inventing which
// of the partial files is authoritative.
//
// The journal (beside the database, named with journalSuffix) records each
// phase and the staged/undo paths so Recover can either finish the install or
// put originals back. Deleting the journal while a restore is mid-flight
// breaks that re-entry: Recover has nothing to follow, and the operator must
// manually reconcile staged/undo files under the database directory.
//
// Failures the journal still cannot catch:
//   - Kernel/disk loss that corrupts both the journal and the staged files
//   - Operators deleting staged/undo files by hand mid-restore
//   - A second concurrent restore against the same database path
//
// Those remain fail-loud outside this package's guarantees. The journal exists
// only for process interruption during an otherwise well-formed restore.
package upgraderestore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MumuTW/looper/internal/upgradebackup"
)

const (
	journalVersion = 1
	journalSuffix  = ".restore-journal"
)

type journalPhase string

const (
	phasePrepared          journalPhase = "prepared"
	phaseOriginalsMoved    journalPhase = "originals-moved"
	phaseDatabaseInstalled journalPhase = "database-installed"
	phaseCommitted         journalPhase = "committed"
)

const (
	entryDatabase = "database"
	entryWAL      = "database-wal"
	entrySHM      = "database-shm"
	entryConfig   = "config"
)

type restoreJournal struct {
	Version int            `json:"version"`
	Phase   journalPhase   `json:"phase"`
	Entries []journalEntry `json:"entries"`
}

type journalEntry struct {
	Name        string `json:"name"`
	TargetPath  string `json:"targetPath"`
	StagedPath  string `json:"stagedPath"`
	UndoPath    string `json:"undoPath"`
	HadOriginal bool   `json:"hadOriginal"`
}

type targetState struct {
	path    string
	present bool
	info    os.FileInfo
}

type fileOperations struct {
	rename func(string, string) error
}

var defaultFileOperations = fileOperations{rename: os.Rename}

// JournalPath returns the deterministic recovery-journal path for databasePath.
// The journal sits beside the database so a caller can find it after a crash.
func JournalPath(databasePath string) string {
	return filepath.Clean(databasePath) + journalSuffix
}

// Restore replaces Source's database and configuration with the matching files
// from bundleDirectory. bundleDirectory must already have passed
// upgradebackup.Verify and source must come from upgradebackup.RestoreSource.
//
// Existing database WAL and SHM sidecars are removed as part of the same
// transaction. Any ordinary pre-commit error rolls the filesystem back to its
// original state. A process interruption can be repaired with Recover.
func Restore(bundleDirectory string, source upgradebackup.Source) error {
	return restore(bundleDirectory, source, defaultFileOperations)
}

// Recover completes recovery for a journal left by an interrupted Restore.
// Uncommitted transactions are rolled back; committed transactions retain the
// restored targets and only remove staging and undo artifacts. A missing journal
// is already recovered and succeeds without doing anything.
func Recover(journalPath string) error {
	journalPath, err := normalizedAbsolutePath(journalPath, "restore journal")
	if err != nil {
		return err
	}
	info, err := os.Lstat(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect restore journal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("restore journal must be a regular file")
	}
	journal, err := readJournal(journalPath, info)
	if err != nil {
		return err
	}
	if err := validateJournal(journalPath, journal); err != nil {
		return err
	}
	if journal.Phase == phaseCommitted {
		if err := cleanupArtifactsAndJournal(journalPath, journal); err != nil {
			return fmt.Errorf("clean committed restore transaction: %w", err)
		}
		return nil
	}
	if err := rollback(journalPath, journal, defaultFileOperations); err != nil {
		return fmt.Errorf("roll back restore transaction: %w", err)
	}
	return nil
}

func restore(bundleDirectory string, source upgradebackup.Source, operations fileOperations) error {
	if operations.rename == nil {
		return fmt.Errorf("rename operation is required")
	}
	bundleDirectory, source, err := normalizeInput(bundleDirectory, source)
	if err != nil {
		return err
	}

	configSource := filepath.Join(bundleDirectory, "config"+filepath.Ext(source.ConfigPath))
	databaseSource := filepath.Join(bundleDirectory, "database.sqlite")
	if err := requireRegularSource(bundleDirectory, "backup bundle directory", true); err != nil {
		return err
	}
	for _, item := range []struct {
		path        string
		description string
	}{{configSource, "bundle configuration"}, {databaseSource, "bundle database"}} {
		if err := requireRegularSource(item.path, item.description, false); err != nil {
			return err
		}
	}

	targetPaths := []string{
		source.DatabasePath,
		source.DatabasePath + "-wal",
		source.DatabasePath + "-shm",
		source.ConfigPath,
	}
	journalPath := JournalPath(source.DatabasePath)
	if err := rejectPathCollisions(targetPaths, []string{configSource, databaseSource, journalPath}); err != nil {
		return err
	}
	if err := requireJournalAbsent(journalPath); err != nil {
		return err
	}

	states := make([]targetState, 0, len(targetPaths))
	for index, path := range targetPaths {
		state, err := inspectTarget(path, targetDescription(index))
		if err != nil {
			return err
		}
		states = append(states, state)
	}

	databaseMode := targetMode(states[0])
	configMode := targetMode(states[3])
	databaseStage, err := stageSource(databaseSource, source.DatabasePath, databaseMode, "database")
	if err != nil {
		return err
	}
	staged := []string{databaseStage}
	cleanupBeforeJournal := func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}
	configStage, err := stageSource(configSource, source.ConfigPath, configMode, "configuration")
	if err != nil {
		cleanupBeforeJournal()
		return err
	}
	staged = append(staged, configStage)

	entries := []journalEntry{
		{Name: entryDatabase, TargetPath: states[0].path, StagedPath: databaseStage, HadOriginal: states[0].present},
		{Name: entryWAL, TargetPath: states[1].path, HadOriginal: states[1].present},
		{Name: entrySHM, TargetPath: states[2].path, HadOriginal: states[2].present},
		{Name: entryConfig, TargetPath: states[3].path, StagedPath: configStage, HadOriginal: states[3].present},
	}
	for index := range entries {
		undoPath, err := reserveUndoPath(entries[index].TargetPath)
		if err != nil {
			cleanupBeforeJournal()
			return err
		}
		entries[index].UndoPath = undoPath
	}
	for index, state := range states {
		if err := confirmTargetUnchanged(state, targetDescription(index)); err != nil {
			cleanupBeforeJournal()
			return err
		}
	}

	journal := restoreJournal{Version: journalVersion, Phase: phasePrepared, Entries: entries}
	if err := createJournal(journalPath, journal); err != nil {
		cleanupBeforeJournal()
		return err
	}
	fail := func(operationErr error) error {
		rollbackErr := rollback(journalPath, journal, operations)
		if rollbackErr != nil {
			return errors.Join(operationErr, fmt.Errorf("rollback restore transaction: %w", rollbackErr))
		}
		return operationErr
	}

	for _, entry := range journal.Entries {
		if !entry.HadOriginal {
			continue
		}
		if err := operations.rename(entry.TargetPath, entry.UndoPath); err != nil {
			return fail(fmt.Errorf("move original %s to undo path: %w", entry.Name, err))
		}
	}
	if err := syncEntryDirectories(journal.Entries); err != nil {
		return fail(fmt.Errorf("sync directories after moving originals: %w", err))
	}
	if err := persistPhase(journalPath, &journal, phaseOriginalsMoved, operations); err != nil {
		return fail(err)
	}

	if err := operations.rename(databaseStage, source.DatabasePath); err != nil {
		return fail(fmt.Errorf("install restored database: %w", err))
	}
	if err := syncDirectories(filepath.Dir(source.DatabasePath)); err != nil {
		return fail(fmt.Errorf("sync restored database directory: %w", err))
	}
	if err := persistPhase(journalPath, &journal, phaseDatabaseInstalled, operations); err != nil {
		return fail(err)
	}

	if err := operations.rename(configStage, source.ConfigPath); err != nil {
		return fail(fmt.Errorf("install restored configuration: %w", err))
	}
	if err := syncDirectories(filepath.Dir(source.DatabasePath), filepath.Dir(source.ConfigPath)); err != nil {
		return fail(fmt.Errorf("sync restored target directories: %w", err))
	}
	if err := persistPhase(journalPath, &journal, phaseCommitted, operations); err != nil {
		// Committed may already be durable on disk; never rollback after that.
		if journal.Phase == phaseCommitted {
			return fmt.Errorf("restore committed with incomplete journal sync; run Recover(%q): %w", journalPath, err)
		}
		return fail(err)
	}

	if err := cleanupArtifactsAndJournal(journalPath, journal); err != nil {
		return fmt.Errorf("restore committed but cleanup is incomplete; run Recover(%q): %w", journalPath, err)
	}
	return nil
}

func normalizeInput(bundleDirectory string, source upgradebackup.Source) (string, upgradebackup.Source, error) {
	if strings.TrimSpace(bundleDirectory) == "" {
		return "", upgradebackup.Source{}, fmt.Errorf("backup bundle directory path is required")
	}
	bundleDirectory, err := filepath.Abs(bundleDirectory)
	if err != nil {
		return "", upgradebackup.Source{}, fmt.Errorf("resolve backup bundle directory: %w", err)
	}
	bundleDirectory = filepath.Clean(bundleDirectory)
	configPath, err := normalizedAbsolutePath(source.ConfigPath, "configuration target")
	if err != nil {
		return "", upgradebackup.Source{}, err
	}
	databasePath, err := normalizedAbsolutePath(source.DatabasePath, "database target")
	if err != nil {
		return "", upgradebackup.Source{}, err
	}
	source.ConfigPath = configPath
	source.DatabasePath = databasePath
	return bundleDirectory, source, nil
}

func normalizedAbsolutePath(path, description string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s path is required", description)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s path must be absolute", description)
	}
	return filepath.Clean(path), nil
}

func requireRegularSource(path, description string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", description, err)
	}
	if directory {
		if !info.IsDir() {
			return fmt.Errorf("%s must be a directory and not a symlink", description)
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file and not a symlink", description)
	}
	return nil
}

func inspectTarget(path, description string) (targetState, error) {
	state := targetState{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return targetState{}, fmt.Errorf("inspect existing %s: %w", description, err)
	}
	if !info.Mode().IsRegular() {
		return targetState{}, fmt.Errorf("existing %s must be a regular file and not a symlink", description)
	}
	state.present = true
	state.info = info
	return state, nil
}

func confirmTargetUnchanged(state targetState, description string) error {
	current, err := os.Lstat(state.path)
	if errors.Is(err, os.ErrNotExist) {
		if state.present {
			return fmt.Errorf("existing %s changed while restore was staged", description)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reinspect existing %s: %w", description, err)
	}
	if !current.Mode().IsRegular() {
		return fmt.Errorf("existing %s changed to a non-regular file while restore was staged", description)
	}
	if !state.present || !os.SameFile(state.info, current) || state.info.Mode().Perm() != current.Mode().Perm() {
		return fmt.Errorf("existing %s changed while restore was staged", description)
	}
	return nil
}

func targetDescription(index int) string {
	switch index {
	case 0:
		return "database"
	case 1:
		return "database WAL sidecar"
	case 2:
		return "database SHM sidecar"
	default:
		return "configuration"
	}
}

func targetMode(state targetState) os.FileMode {
	if state.present {
		return state.info.Mode().Perm()
	}
	return 0o600
}

func rejectPathCollisions(targets, reserved []string) error {
	seen := make(map[string]string, len(targets)+len(reserved))
	for index, path := range targets {
		if prior, ok := seen[path]; ok {
			return fmt.Errorf("restore target path %q collides with %s", path, prior)
		}
		seen[path] = targetDescription(index)
	}
	for _, path := range reserved {
		if prior, ok := seen[path]; ok {
			return fmt.Errorf("restore path %q collides with %s", path, prior)
		}
		seen[path] = "reserved restore path"
	}
	return nil
}

func requireJournalAbsent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing restore journal: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("existing restore journal must be a regular file and not a symlink")
	}
	return fmt.Errorf("an incomplete restore journal already exists at %s; recover it before restoring", path)
}

func stageSource(sourcePath, targetPath string, mode os.FileMode, description string) (string, error) {
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("inspect bundle %s: %w", description, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return "", fmt.Errorf("bundle %s must be a regular file and not a symlink", description)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open bundle %s: %w", description, err)
	}
	defer source.Close()
	openedInfo, err := source.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect opened bundle %s: %w", description, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedInfo) {
		return "", fmt.Errorf("bundle %s changed while it was opened", description)
	}

	temporary, err := os.CreateTemp(filepath.Dir(targetPath), temporaryPattern(targetPath, "stage"))
	if err != nil {
		return "", fmt.Errorf("create staged %s beside target: %w", description, err)
	}
	temporaryPath := temporary.Name()
	fail := func(operationErr error) (string, error) {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return "", operationErr
	}
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return fail(fmt.Errorf("set staged %s permissions: %w", description, err))
	}
	if _, err := io.Copy(temporary, source); err != nil {
		return fail(fmt.Errorf("copy bundle %s to stage: %w", description, err))
	}
	if err := temporary.Sync(); err != nil {
		return fail(fmt.Errorf("sync staged %s: %w", description, err))
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", fmt.Errorf("close staged %s: %w", description, err)
	}
	return temporaryPath, nil
}

func reserveUndoPath(targetPath string) (string, error) {
	reservation, err := os.CreateTemp(filepath.Dir(targetPath), temporaryPattern(targetPath, "undo"))
	if err != nil {
		return "", fmt.Errorf("reserve undo path for %s: %w", targetPath, err)
	}
	path := reservation.Name()
	if err := reservation.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close undo reservation for %s: %w", targetPath, err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("release undo reservation for %s: %w", targetPath, err)
	}
	return path, nil
}

func temporaryPattern(targetPath, kind string) string {
	return "." + filepath.Base(targetPath) + ".looper-restore-" + kind + "-*"
}

func createJournal(path string, journal restoreJournal) error {
	encoded, err := encodeJournal(journal)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create restore journal: %w", err)
	}
	fail := func(operationErr error) error {
		_ = file.Close()
		_ = os.Remove(path)
		return operationErr
	}
	if _, err := file.Write(encoded); err != nil {
		return fail(fmt.Errorf("write restore journal: %w", err))
	}
	if err := file.Sync(); err != nil {
		return fail(fmt.Errorf("sync restore journal: %w", err))
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close restore journal: %w", err)
	}
	if err := syncDirectories(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync restore journal directory: %w", err)
	}
	return nil
}

func persistPhase(path string, journal *restoreJournal, phase journalPhase, operations fileOperations) error {
	next := *journal
	next.Phase = phase
	encoded, err := encodeJournal(next)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".looper-restore-journal-write-*")
	if err != nil {
		return fmt.Errorf("create restore journal update: %w", err)
	}
	temporaryPath := temporary.Name()
	fail := func(operationErr error) error {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return operationErr
	}
	if err := temporary.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("set restore journal permissions: %w", err))
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fail(fmt.Errorf("write restore journal phase %s: %w", phase, err))
	}
	if err := temporary.Sync(); err != nil {
		return fail(fmt.Errorf("sync restore journal phase %s: %w", phase, err))
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close restore journal phase %s: %w", phase, err)
	}
	if err := operations.rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("publish restore journal phase %s: %w", phase, err)
	}
	// The durable journal now records phase. Update memory before directory
	// sync so a later sync failure cannot leave callers rolling back against a
	// journal that already says committed (or any later phase).
	journal.Phase = phase
	if err := syncDirectories(filepath.Dir(path)); err != nil {
		if phase == phaseCommitted {
			// Committed is durable; surface incomplete fsync without starting
			// a rollback that would contradict Recover's committed handling.
			return fmt.Errorf("restore journal phase %s published but directory sync failed; do not rollback — run Recover(%q) after fixing the filesystem: %w", phase, path, err)
		}
		return fmt.Errorf("sync restore journal phase %s: %w", phase, err)
	}
	return nil
}

func encodeJournal(journal restoreJournal) ([]byte, error) {
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode restore journal: %w", err)
	}
	return append(encoded, '\n'), nil
}

func readJournal(path string, expectedInfo os.FileInfo) (restoreJournal, error) {
	file, err := os.Open(path)
	if err != nil {
		return restoreJournal{}, fmt.Errorf("open restore journal: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return restoreJournal{}, fmt.Errorf("inspect opened restore journal: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(expectedInfo, openedInfo) {
		return restoreJournal{}, fmt.Errorf("restore journal changed while it was opened")
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var journal restoreJournal
	if err := decoder.Decode(&journal); err != nil {
		return restoreJournal{}, fmt.Errorf("decode restore journal: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return restoreJournal{}, fmt.Errorf("decode restore journal: unexpected trailing JSON value")
		}
		return restoreJournal{}, fmt.Errorf("decode restore journal: %w", err)
	}
	return journal, nil
}

func validateJournal(path string, journal restoreJournal) error {
	if journal.Version != journalVersion {
		return fmt.Errorf("unsupported restore journal version %d", journal.Version)
	}
	switch journal.Phase {
	case phasePrepared, phaseOriginalsMoved, phaseDatabaseInstalled, phaseCommitted:
	default:
		return fmt.Errorf("unsupported restore journal phase %q", journal.Phase)
	}
	if len(journal.Entries) != 4 {
		return fmt.Errorf("restore journal must contain exactly four target entries")
	}
	entries := make(map[string]journalEntry, len(journal.Entries))
	allPaths := map[string]string{path: "journal"}
	for _, entry := range journal.Entries {
		if _, duplicate := entries[entry.Name]; duplicate {
			return fmt.Errorf("restore journal contains duplicate entry %q", entry.Name)
		}
		entries[entry.Name] = entry
		for field, candidate := range map[string]string{"target": entry.TargetPath, "undo": entry.UndoPath} {
			if err := validateJournalArtifactPath(candidate, field, entry); err != nil {
				return err
			}
			if prior, duplicate := allPaths[candidate]; duplicate {
				return fmt.Errorf("restore journal %s path for %s collides with %s", field, entry.Name, prior)
			}
			allPaths[candidate] = entry.Name + " " + field
		}
		if entry.StagedPath != "" {
			if err := validateJournalArtifactPath(entry.StagedPath, "stage", entry); err != nil {
				return err
			}
			if prior, duplicate := allPaths[entry.StagedPath]; duplicate {
				return fmt.Errorf("restore journal stage path for %s collides with %s", entry.Name, prior)
			}
			allPaths[entry.StagedPath] = entry.Name + " stage"
		}
	}
	database, okDatabase := entries[entryDatabase]
	wal, okWAL := entries[entryWAL]
	shm, okSHM := entries[entrySHM]
	config, okConfig := entries[entryConfig]
	if !okDatabase || !okWAL || !okSHM || !okConfig {
		return fmt.Errorf("restore journal must name database, database-wal, database-shm, and config")
	}
	if database.StagedPath == "" || config.StagedPath == "" || wal.StagedPath != "" || shm.StagedPath != "" {
		return fmt.Errorf("restore journal has invalid staged target entries")
	}
	if wal.TargetPath != database.TargetPath+"-wal" || shm.TargetPath != database.TargetPath+"-shm" {
		return fmt.Errorf("restore journal database sidecar paths do not match database target")
	}
	if filepath.Clean(path) != JournalPath(database.TargetPath) {
		return fmt.Errorf("restore journal is not beside its database target")
	}
	return nil
}

func validateJournalArtifactPath(path, field string, entry journalEntry) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("restore journal %s path for %s must be a clean absolute path", field, entry.Name)
	}
	if field == "target" {
		return nil
	}
	if filepath.Dir(path) != filepath.Dir(entry.TargetPath) {
		return fmt.Errorf("restore journal %s path for %s must be beside its target", field, entry.Name)
	}
	kind := "undo"
	if field == "stage" {
		kind = "stage"
	}
	wantPrefix := strings.TrimSuffix(temporaryPattern(entry.TargetPath, kind), "*")
	if !strings.HasPrefix(filepath.Base(path), wantPrefix) {
		return fmt.Errorf("restore journal %s path for %s is not a restore artifact", field, entry.Name)
	}
	return nil
}

func rollback(journalPath string, journal restoreJournal, operations fileOperations) error {
	var rollbackErrors []error
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := journal.Entries[index]
		undoInfo, undoPresent, err := inspectRegularArtifact(entry.UndoPath, "undo file for "+entry.Name)
		if err != nil {
			rollbackErrors = append(rollbackErrors, err)
			continue
		}
		if undoPresent {
			_ = undoInfo
			if err := removeRegularIfExists(entry.TargetPath, "current target for "+entry.Name); err != nil {
				rollbackErrors = append(rollbackErrors, err)
				continue
			}
			if err := operations.rename(entry.UndoPath, entry.TargetPath); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore original %s: %w", entry.Name, err))
			}
			continue
		}
		if !entry.HadOriginal {
			// Target was absent when the journal was prepared. If a file is now
			// present, delete it only when it is provably this transaction's
			// staged install; otherwise fail closed rather than destroying an
			// operator/application file created after the interruption.
			_, targetPresent, err := inspectRegularArtifact(entry.TargetPath, "new target for "+entry.Name)
			if err != nil {
				rollbackErrors = append(rollbackErrors, err)
				continue
			}
			if !targetPresent {
				continue
			}
			if strings.TrimSpace(entry.StagedPath) == "" {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("refusing to delete %s: target appeared after prepared journal with no staged identity", entry.Name))
				continue
			}
			_, stagedPresent, err := inspectRegularArtifact(entry.StagedPath, "staged file for "+entry.Name)
			if err != nil {
				rollbackErrors = append(rollbackErrors, err)
				continue
			}
			if !stagedPresent {
				// Staged already consumed (installed). Only remove target if it
				// still matches what we would have installed — compare to undo
				// absence + phase is insufficient; refuse without content proof.
				rollbackErrors = append(rollbackErrors, fmt.Errorf("refusing to delete %s: staged artifact is gone so target ownership cannot be proven", entry.Name))
				continue
			}
			same, err := sameRegularFileContent(entry.StagedPath, entry.TargetPath)
			if err != nil {
				rollbackErrors = append(rollbackErrors, err)
				continue
			}
			if !same {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("refusing to delete %s: target does not match staged restore artifact", entry.Name))
				continue
			}
			if err := removeRegularIfExists(entry.TargetPath, "new target for "+entry.Name); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
			continue
		}
		_, targetPresent, err := inspectRegularArtifact(entry.TargetPath, "original target for "+entry.Name)
		if err != nil {
			rollbackErrors = append(rollbackErrors, err)
			continue
		}
		if !targetPresent {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("original %s and its undo file are both missing", entry.Name))
		}
	}
	if err := syncEntryDirectories(journal.Entries); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("sync directories after rollback: %w", err))
	}
	if len(rollbackErrors) > 0 {
		return errors.Join(rollbackErrors...)
	}
	if err := cleanupArtifactsAndJournal(journalPath, journal); err != nil {
		return fmt.Errorf("clean rolled-back restore transaction: %w", err)
	}
	return nil
}

func inspectRegularArtifact(path, description string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s: %w", description, err)
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%s must be a regular file and not a symlink", description)
	}
	return info, true, nil
}

func sameRegularFileContent(left, right string) (bool, error) {
	leftInfo, err := os.Lstat(left)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", left, err)
	}
	rightInfo, err := os.Lstat(right)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", right, err)
	}
	if !leftInfo.Mode().IsRegular() || !rightInfo.Mode().IsRegular() {
		return false, fmt.Errorf("content compare requires regular files")
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	leftFile, err := os.Open(left)
	if err != nil {
		return false, err
	}
	defer leftFile.Close()
	rightFile, err := os.Open(right)
	if err != nil {
		return false, err
	}
	defer rightFile.Close()
	leftHash := sha256.New()
	rightHash := sha256.New()
	if _, err := io.Copy(leftHash, leftFile); err != nil {
		return false, err
	}
	if _, err := io.Copy(rightHash, rightFile); err != nil {
		return false, err
	}
	return string(leftHash.Sum(nil)) == string(rightHash.Sum(nil)), nil
}

func removeRegularIfExists(path, description string) error {
	_, present, err := inspectRegularArtifact(path, description)
	if err != nil || !present {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", description, err)
	}
	return nil
}

func cleanupArtifactsAndJournal(journalPath string, journal restoreJournal) error {
	artifacts := make([]string, 0, len(journal.Entries)*2)
	for _, entry := range journal.Entries {
		if entry.StagedPath != "" {
			artifacts = append(artifacts, entry.StagedPath)
		}
		artifacts = append(artifacts, entry.UndoPath)
	}
	for _, path := range artifacts {
		if _, _, err := inspectRegularArtifact(path, "restore artifact"); err != nil {
			return err
		}
	}
	for _, path := range artifacts {
		if err := removeRegularIfExists(path, "restore artifact"); err != nil {
			return err
		}
	}
	if err := syncEntryDirectories(journal.Entries); err != nil {
		return fmt.Errorf("sync restore artifact directories: %w", err)
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove restore journal: %w", err)
	}
	if err := syncDirectories(filepath.Dir(journalPath)); err != nil {
		return fmt.Errorf("sync restore journal removal: %w", err)
	}
	return nil
}

func syncEntryDirectories(entries []journalEntry) error {
	directories := make([]string, 0, len(entries))
	for _, entry := range entries {
		directories = append(directories, filepath.Dir(entry.TargetPath))
	}
	return syncDirectories(directories...)
}

func syncDirectories(paths ...string) error {
	unique := make(map[string]bool, len(paths))
	for _, path := range paths {
		unique[filepath.Clean(path)] = true
	}
	directories := make([]string, 0, len(unique))
	for path := range unique {
		directories = append(directories, path)
	}
	sort.Strings(directories)
	for _, path := range directories {
		directory, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open directory %s for sync: %w", path, err)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return fmt.Errorf("sync directory %s: %w", path, syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close synced directory %s: %w", path, closeErr)
		}
	}
	return nil
}
