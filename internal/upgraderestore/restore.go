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
	"encoding/hex"
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
	// StagedSHA256/StagedSize identify the staged bytes so rollback can prove
	// ownership after the staged file has been renamed into the target path.
	StagedSHA256 string `json:"stagedSha256,omitempty"`
	StagedSize   int64  `json:"stagedSize,omitempty"`
}

type targetState struct {
	path    string
	present bool
	info    os.FileInfo
	hash    string
	size    int64
}

type fileOperations struct {
	rename           func(string, string) error
	link             func(string, string) error
	exclusivePublish func(source, dest string) error
	syncDirectory    func(string) error
}

var defaultFileOperations = fileOperations{rename: os.Rename, link: os.Link}
var stageSourceAtForRestore = stageSourceAt

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
		// Before discarding undo files, prove committed targets still match the
		// staged install identity. Missing or rewritten targets must fail loud
		// so undo is not deleted while live data is absent/wrong.
		if err := confirmCommittedTargetsIntact(journal); err != nil {
			return err
		}
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
		operations.rename = os.Rename
	}
	if operations.link == nil {
		operations.link = os.Link
	}
	bundleDirectory, source, err := normalizeInput(bundleDirectory, source)
	if err != nil {
		return err
	}

	if err := requireRegularSource(bundleDirectory, "backup bundle directory", true); err != nil {
		return err
	}
	// Production bundles carry a v2 manifest; re-verify before staging. Hand-
	// built fixtures without a manifest still stage, but compare staged bytes
	// to the source files before moving originals.
	var verified *upgradebackup.Verification
	if _, err := os.Lstat(filepath.Join(bundleDirectory, "manifest.json")); err == nil {
		v, err := upgradebackup.Verify(bundleDirectory)
		if err != nil {
			return fmt.Errorf("re-verify backup bundle before restore: %w", err)
		}
		// Reject a replaced manifest that re-authorizes different artifacts for
		// the destinations extracted from an earlier Verify in the CLI.
		if err := requireRestoreSourceMatchesManifest(source, v.Manifest); err != nil {
			return err
		}
		verified = &v
	}
	configName := "config" + filepath.Ext(source.ConfigPath)
	if verified != nil {
		name, err := bundleConfigFileName(verified.Manifest)
		if err != nil {
			return err
		}
		configName = name
	} else if configName == "config" {
		configName = "config.toml"
	}
	// Prefer an existing config.* in the bundle when Ext(source) doesn't match.
	if _, err := os.Lstat(filepath.Join(bundleDirectory, configName)); err != nil {
		for _, candidate := range []string{"config.toml", "config.json", "config.yaml", "config.yml", "config"} {
			if _, err := os.Lstat(filepath.Join(bundleDirectory, candidate)); err == nil {
				configName = candidate
				break
			}
		}
	}
	configSource := filepath.Join(bundleDirectory, configName)
	databaseSource := filepath.Join(bundleDirectory, "database.sqlite")
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
	databaseStage, err := reserveRestoreArtifactPath(source.DatabasePath, "stage")
	if err != nil {
		return fmt.Errorf("reserve staged database beside target: %w", err)
	}
	configStage, err := reserveRestoreArtifactPath(source.ConfigPath, "stage")
	if err != nil {
		_ = os.Remove(databaseStage)
		return fmt.Errorf("reserve staged configuration beside target: %w", err)
	}
	entries := []journalEntry{
		{Name: entryDatabase, TargetPath: states[0].path, StagedPath: databaseStage, HadOriginal: states[0].present},
		{Name: entryWAL, TargetPath: states[1].path, HadOriginal: states[1].present},
		{Name: entrySHM, TargetPath: states[2].path, HadOriginal: states[2].present},
		{Name: entryConfig, TargetPath: states[3].path, StagedPath: configStage, HadOriginal: states[3].present},
	}
	reservedArtifacts := []string{databaseStage, configStage}
	cleanupBeforeJournal := func() {
		for _, path := range reservedArtifacts {
			_ = os.Remove(path)
		}
	}
	for index := range entries {
		undoPath, err := reserveUndoPath(entries[index].TargetPath)
		if err != nil {
			cleanupBeforeJournal()
			return err
		}
		entries[index].UndoPath = undoPath
		reservedArtifacts = append(reservedArtifacts, undoPath)
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
	if err := stageSourceAtForRestore(databaseSource, databaseStage, databaseMode, "database"); err != nil {
		return fail(err)
	}
	if err := stageSourceAtForRestore(configSource, configStage, configMode, "configuration"); err != nil {
		return fail(err)
	}
	dbHash, dbSize, err := fileContentIdentity(databaseStage)
	if err != nil {
		return fail(fmt.Errorf("hash staged database: %w", err))
	}
	cfgHash, cfgSize, err := fileContentIdentity(configStage)
	if err != nil {
		return fail(fmt.Errorf("hash staged configuration: %w", err))
	}
	// Fail closed if the bundle was mutated after Verify (including during PID
	// probes / lock acquisition): staged bytes must still match the manifest.
	if verified != nil {
		if err := requireStagedMatchesManifest(dbHash, dbSize, verified.Manifest.Files["database.sqlite"], "database.sqlite"); err != nil {
			return fail(err)
		}
		if err := requireStagedMatchesManifest(cfgHash, cfgSize, verified.Manifest.Files[configName], configName); err != nil {
			return fail(err)
		}
	} else {
		// No manifest: still require staged bytes equal the source files now.
		if same, err := sameRegularFileContent(databaseStage, databaseSource); err != nil || !same {
			if err != nil {
				return fail(err)
			}
			return fail(fmt.Errorf("staged database does not match bundle source after copy"))
		}
		if same, err := sameRegularFileContent(configStage, configSource); err != nil || !same {
			if err != nil {
				return fail(err)
			}
			return fail(fmt.Errorf("staged configuration does not match bundle source after copy"))
		}
	}
	journal.Entries[0].StagedSHA256 = dbHash
	journal.Entries[0].StagedSize = dbSize
	journal.Entries[3].StagedSHA256 = cfgHash
	journal.Entries[3].StagedSize = cfgSize
	for index, state := range states {
		if err := confirmTargetUnchanged(state, targetDescription(index)); err != nil {
			return fail(err)
		}
	}
	// Persist the staged identities before moving any original target aside.
	// If the process stops during preparation, the phase-prepared journal still
	// names both stage paths and Recover can remove them without guessing.
	if err := persistPhase(journalPath, &journal, phasePrepared, operations); err != nil {
		return fail(err)
	}

	for index, entry := range journal.Entries {
		if !entry.HadOriginal {
			continue
		}
		// Detach then re-verify inode content so a concurrent atomic replace
		// between confirmTargetUnchanged and rename is not adopted as undo.
		if err := operations.rename(entry.TargetPath, entry.UndoPath); err != nil {
			return fail(fmt.Errorf("move original %s to undo path: %w", entry.Name, err))
		}
		if err := confirmUndoMatchesInspectedOriginal(states[index], entry); err != nil {
			// Put the detached file back without replacing a recreate at target.
			_ = installNoReplace(entry.UndoPath, entry.TargetPath, "original "+entry.Name+" (undo put-back)")
			return fail(err)
		}
	}
	if err := syncEntryDirectories(journal.Entries); err != nil {
		return fail(fmt.Errorf("sync directories after moving originals: %w", err))
	}
	if err := persistPhase(journalPath, &journal, phaseOriginalsMoved, operations); err != nil {
		return fail(err)
	}

	// Publish staged files with no-replace so a concurrent recreate at the
	// target after undo is not clobbered by os.Rename.
	if err := publishNoReplace(operations, databaseStage, source.DatabasePath, "restored database"); err != nil {
		return fail(fmt.Errorf("install restored database: %w", err))
	}
	if err := syncDirectories(filepath.Dir(source.DatabasePath)); err != nil {
		return fail(fmt.Errorf("sync restored database directory: %w", err))
	}
	if err := persistPhase(journalPath, &journal, phaseDatabaseInstalled, operations); err != nil {
		return fail(err)
	}

	if err := publishNoReplace(operations, configStage, source.ConfigPath, "restored configuration"); err != nil {
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

	// The committed journal and undo files are the last recovery boundary. A
	// concurrent writer may have removed or rewritten a restored target after
	// the phase became durable; preserve the journal and undo files instead of
	// reporting success after deleting the only rollback copy.
	if err := confirmCommittedTargetsIntact(journal); err != nil {
		return fmt.Errorf("restore committed but cleanup is unsafe; run Recover(%q): %w", journalPath, err)
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
	// Content identity catches in-place rewrites that keep the same inode.
	hash, size, err := fileContentIdentity(path)
	if err != nil {
		return targetState{}, fmt.Errorf("hash existing %s: %w", description, err)
	}
	state.hash = hash
	state.size = size
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

// confirmCommittedTargetsIntact ensures database and config still match the
// staged content hashes recorded in the journal before undo artifacts are removed.
func confirmCommittedTargetsIntact(journal restoreJournal) error {
	for _, entry := range journal.Entries {
		if entry.Name != entryDatabase && entry.Name != entryConfig {
			continue
		}
		if strings.TrimSpace(entry.StagedSHA256) == "" {
			return fmt.Errorf("committed restore journal lacks staged identity for %s; refusing to discard undo files", entry.Name)
		}
		hash, size, err := fileContentIdentity(entry.TargetPath)
		if err != nil {
			return fmt.Errorf("committed restore target %s is missing or unreadable; undo preserved: %w", entry.Name, err)
		}
		if hash != entry.StagedSHA256 || size != entry.StagedSize {
			return fmt.Errorf("committed restore target %s no longer matches staged identity; undo preserved", entry.Name)
		}
	}
	return nil
}

// confirmUndoMatchesInspectedOriginal ensures the file renamed to undo is still
// the same inode and content inspected before the move (not a concurrent
// replace or in-place rewrite).
func confirmUndoMatchesInspectedOriginal(state targetState, entry journalEntry) error {
	if !state.present || state.info == nil {
		return nil
	}
	undoInfo, err := os.Lstat(entry.UndoPath)
	if err != nil {
		return fmt.Errorf("inspect undo for %s after move: %w", entry.Name, err)
	}
	if !os.SameFile(state.info, undoInfo) {
		return fmt.Errorf("undo for %s is not the inspected original (target was replaced during move)", entry.Name)
	}
	if state.hash != "" {
		hash, size, err := fileContentIdentity(entry.UndoPath)
		if err != nil {
			return fmt.Errorf("hash undo for %s after move: %w", entry.Name, err)
		}
		if hash != state.hash || size != state.size {
			return fmt.Errorf("undo for %s content changed after inspect (in-place rewrite before move)", entry.Name)
		}
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

func stageSourceAt(sourcePath, temporaryPath string, mode os.FileMode, description string) error {
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect bundle %s: %w", description, err)
	}
	if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("bundle %s must be a regular file and not a symlink", description)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open bundle %s: %w", description, err)
	}
	defer source.Close()
	openedInfo, err := source.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened bundle %s: %w", description, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, openedInfo) {
		return fmt.Errorf("bundle %s changed while it was opened", description)
	}

	temporary, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create staged %s beside target: %w", description, err)
	}
	fail := func(operationErr error) error {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return operationErr
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
		return fmt.Errorf("close staged %s: %w", description, err)
	}
	return nil
}

func reserveUndoPath(targetPath string) (string, error) {
	return reserveRestoreArtifactPath(targetPath, "undo")
}

func reserveRestoreArtifactPath(targetPath, kind string) (string, error) {
	reservation, err := os.CreateTemp(filepath.Dir(targetPath), temporaryPattern(targetPath, kind))
	if err != nil {
		return "", fmt.Errorf("reserve %s path for %s: %w", kind, targetPath, err)
	}
	path := reservation.Name()
	if err := reservation.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close %s reservation for %s: %w", kind, targetPath, err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("release %s reservation for %s: %w", kind, targetPath, err)
	}
	return path, nil
}

func temporaryPattern(targetPath, kind string) string {
	return "." + filepath.Base(targetPath) + ".looper-restore-" + kind + "-*"
}

func createJournal(path string, journal restoreJournal) error {
	return createJournalWithOperations(path, journal, defaultFileOperations)
}

func createJournalWithOperations(path string, journal restoreJournal, operations fileOperations) error {
	encoded, err := encodeJournal(journal)
	if err != nil {
		return err
	}
	if operations.link == nil {
		operations.link = os.Link
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".looper-restore-journal-create-*")
	if err != nil {
		return fmt.Errorf("create restore journal: %w", err)
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
		return fail(fmt.Errorf("write restore journal: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		return fail(fmt.Errorf("sync restore journal: %w", err))
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close restore journal: %w", err)
	}
	// Publish only after the complete temporary file is durable. The hard-link
	// path in publishNoReplace is atomic and refuses to replace a journal that
	// appeared concurrently; its directory sync makes the new authoritative
	// name durable before the temporary name is removed.
	if err := publishNoReplace(operations, temporaryPath, path, "restore journal"); err != nil {
		// publishNoReplace may have already created a durable destination before
		// failing to remove the temporary name. Never remove the authoritative
		// journal here; Recover must be able to inspect it after the error.
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("publish restore journal: %w", err)
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
			// Undo holds the pre-restore original. Detach-and-verify before
			// deleting the current target so a recreated file cannot be removed
			// after a stale ownership hash (check-then-act race).
			targetInfo, targetPresent, err := inspectRegularArtifact(entry.TargetPath, "current target for "+entry.Name)
			if err != nil {
				rollbackErrors = append(rollbackErrors, err)
				continue
			}
			if targetPresent {
				// Recovery may have been interrupted after publishing the undo
				// file back to its target but before removing the undo artifact.
				// If both paths already contain the same original, keep the live
				// target and only remove the duplicate artifact; treating it as a
				// staged target would make an otherwise successful rollback fail
				// forever on every retry.
				alreadyRestored, compareErr := sameRestoredOriginal(targetInfo, undoInfo, entry.TargetPath, entry.UndoPath)
				if compareErr != nil {
					rollbackErrors = append(rollbackErrors, compareErr)
					continue
				}
				if alreadyRestored {
					if err := os.Remove(entry.UndoPath); err != nil {
						rollbackErrors = append(rollbackErrors, fmt.Errorf("remove duplicate undo for %s: %w", entry.Name, err))
					}
					continue
				}
				if err := detachAndRemoveOwnedRestoreTarget(entry, "current target for "+entry.Name); err != nil {
					rollbackErrors = append(rollbackErrors, err)
					continue
				}
			}
			// Install undo with no-replace so a recreate that landed after detach
			// is not silently overwritten by os.Rename.
			if err := installNoReplace(entry.UndoPath, entry.TargetPath, "original "+entry.Name); err != nil {
				rollbackErrors = append(rollbackErrors, err)
			}
			continue
		}
		if !entry.HadOriginal {
			// Target was absent when the journal was prepared. Detach-and-verify
			// before delete so an external recreate after ownership proof is kept.
			_, targetPresent, err := inspectRegularArtifact(entry.TargetPath, "new target for "+entry.Name)
			if err != nil {
				rollbackErrors = append(rollbackErrors, err)
				continue
			}
			if !targetPresent {
				continue
			}
			if err := detachAndRemoveOwnedRestoreTarget(entry, "new target for "+entry.Name); err != nil {
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

func sameRestoredOriginal(targetInfo, undoInfo os.FileInfo, targetPath, undoPath string) (bool, error) {
	if targetInfo == nil || undoInfo == nil {
		return false, nil
	}
	if targetInfo.Mode().Perm() != undoInfo.Mode().Perm() {
		return false, nil
	}
	if os.SameFile(targetInfo, undoInfo) {
		return true, nil
	}
	same, err := sameRegularFileContent(targetPath, undoPath)
	if err != nil {
		return false, fmt.Errorf("compare restored target %s with undo %s: %w", targetPath, undoPath, err)
	}
	return same, nil
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

// expectedStagedIdentity returns the content identity of the restore transaction's
// staged install for entry (live staged file when present, else durable journal fields).
func expectedStagedIdentity(entry journalEntry) (hash string, size int64, err error) {
	if strings.TrimSpace(entry.StagedPath) != "" {
		if _, stagedPresent, err := inspectRegularArtifact(entry.StagedPath, "staged file for "+entry.Name); err != nil {
			return "", 0, err
		} else if stagedPresent {
			return fileContentIdentity(entry.StagedPath)
		}
	}
	if strings.TrimSpace(entry.StagedSHA256) == "" {
		return "", 0, nil
	}
	return entry.StagedSHA256, entry.StagedSize, nil
}

// detachAndRemoveOwnedRestoreTarget renames the target aside, re-verifies it is
// still this transaction's staged content, then deletes the detached file.
// A concurrent recreate at the original path is left alone; a race that renames
// a non-owned file is restored to the path.
func detachAndRemoveOwnedRestoreTarget(entry journalEntry, description string) error {
	wantHash, wantSize, err := expectedStagedIdentity(entry)
	if err != nil {
		return err
	}
	if wantHash == "" {
		return fmt.Errorf("refusing to delete %s: target is not proven to belong to this restore transaction", entry.Name)
	}
	hash, size, err := fileContentIdentity(entry.TargetPath)
	if err != nil {
		return err
	}
	if hash != wantHash || size != wantSize {
		return fmt.Errorf("refusing to replace %s: target was recreated outside this restore transaction", entry.Name)
	}
	dir := filepath.Dir(entry.TargetPath)
	trash, err := os.CreateTemp(dir, ".restore-rm-*")
	if err != nil {
		return fmt.Errorf("reserve detach path for %s: %w", description, err)
	}
	trashPath := trash.Name()
	if err := trash.Close(); err != nil {
		_ = os.Remove(trashPath)
		return fmt.Errorf("close detach path for %s: %w", description, err)
	}
	if err := os.Remove(trashPath); err != nil {
		return fmt.Errorf("prepare detach path for %s: %w", description, err)
	}
	if err := os.Rename(entry.TargetPath, trashPath); err != nil {
		return fmt.Errorf("detach %s: %w", description, err)
	}
	hash2, size2, err := fileContentIdentity(trashPath)
	if err != nil || hash2 != wantHash || size2 != wantSize {
		// Put the detached file back only if the path is still empty — never
		// replace a concurrent recreate (os.Rename would clobber it).
		if putBackErr := installNoReplace(trashPath, entry.TargetPath, description+" (detach put-back)"); putBackErr != nil {
			return fmt.Errorf("detach verify failed for %s; trash kept at %s: verify=%v put-back=%w", description, trashPath, err, putBackErr)
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("refusing to replace %s: target content changed during detach", entry.Name)
	}
	if err := os.Remove(trashPath); err != nil {
		return fmt.Errorf("remove detached %s: %w", description, err)
	}
	return nil
}

// installNoReplace moves source onto dest only when dest is absent.
func installNoReplace(source, dest, description string) error {
	return publishNoReplace(defaultFileOperations, source, dest, description)
}

// publishNoReplace publishes source to dest without replacing an existing dest.
// Prefer hard-link (fails if dest exists). The copy fallback publishes through
// a complete temporary file and never exposes a partially copied destination.
func publishNoReplace(operations fileOperations, source, dest, description string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("refusing to install %s: target %s already exists", description, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s destination: %w", description, err)
	}
	link := operations.link
	if link == nil {
		link = os.Link
	}
	syncDirectory := operations.syncDirectory
	if syncDirectory == nil {
		syncDirectory = func(directory string) error {
			return syncDirectories(directory)
		}
	}
	if err := link(source, dest); err == nil {
		// The destination link must be durable before unlinking the only other
		// name for the restored bytes. Rollback/recovery can then survive a crash
		// between publication and source cleanup without losing both names.
		if err := syncDirectory(filepath.Dir(dest)); err != nil {
			return fmt.Errorf("sync destination directory after installing %s: %w", description, err)
		}
		if err := os.Remove(source); err != nil {
			return fmt.Errorf("remove source after installing %s: %w", description, err)
		}
		return nil
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("refusing to install %s: target %s already exists", description, dest)
	}
	if operations.exclusivePublish != nil {
		if err := operations.exclusivePublish(source, dest); err != nil {
			return fmt.Errorf("install %s: %w", description, err)
		}
		if err := syncDirectory(filepath.Dir(dest)); err != nil {
			return fmt.Errorf("sync destination directory after installing %s: %w", description, err)
		}
		_ = os.Remove(source)
		return nil
	}
	if err := copyFileAtomicNoReplace(source, dest, description, syncDirectory); err != nil {
		return fmt.Errorf("install %s: %w", description, err)
	}
	if err := os.Remove(source); err != nil {
		return fmt.Errorf("remove source after installing %s: %w", description, err)
	}
	return nil
}

// copyFileAtomicNoReplace keeps an exclusive-copy fallback from exposing a
// partially written destination. It first makes and syncs a complete private
// copy, then publishes that copy with a hard link (or, on filesystems without
// hard-link support, an atomic rename after a no-replace check). A crash before
// publication therefore leaves the source and/or publish temporary intact,
// never a destination that looks like a candidate but contains a prefix.
func copyFileAtomicNoReplace(source, dest, description string, syncDirectory func(string) error) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(dest), ".looper-restore-publish-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = temporary.Close(); _ = os.Remove(temporaryPath) }
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Truncate(0); err != nil {
		cleanup()
		return err
	}
	if _, err := io.Copy(temporary, in); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("refusing to install %s: target %s already exists", description, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("inspect %s destination: %w", description, err)
	}
	if err := os.Link(temporaryPath, dest); err == nil {
		if err := syncDirectory(filepath.Dir(dest)); err != nil {
			return fmt.Errorf("sync destination directory after installing %s: %w", description, err)
		}
		if err := os.Remove(temporaryPath); err != nil {
			return fmt.Errorf("remove publish temporary after installing %s: %w", description, err)
		}
		return nil
	}
	if _, err := os.Lstat(dest); err == nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("refusing to install %s: target %s already exists", description, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("inspect %s destination: %w", description, err)
	}
	if err := os.Rename(temporaryPath, dest); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("publish complete %s: %w", description, err)
	}
	if err := syncDirectory(filepath.Dir(dest)); err != nil {
		return fmt.Errorf("sync destination directory after installing %s: %w", description, err)
	}
	return nil
}

func requireRestoreSourceMatchesManifest(source upgradebackup.Source, manifest upgradebackup.Manifest) error {
	manifestSource, err := upgradebackup.RestoreSource(manifest)
	if err != nil {
		// Legacy/v1 has no Source; production restore already requires v2 via CLI.
		return nil
	}
	if filepath.Clean(source.ConfigPath) != filepath.Clean(manifestSource.ConfigPath) ||
		filepath.Clean(source.DatabasePath) != filepath.Clean(manifestSource.DatabasePath) {
		return fmt.Errorf("re-verified backup manifest source destinations do not match the restore request (config/database paths diverged after initial verify)")
	}
	return nil
}

func bundleConfigFileName(manifest upgradebackup.Manifest) (string, error) {
	for _, name := range upgradebackup.SortedFileNames(manifest) {
		if name == "config" || strings.HasPrefix(name, "config.") {
			return name, nil
		}
	}
	return "", fmt.Errorf("backup manifest has no config artifact")
}

func requireStagedMatchesManifest(hash string, size int64, expected upgradebackup.File, name string) error {
	if expected.Size < 0 || len(expected.SHA256) != 64 || strings.ToLower(expected.SHA256) != expected.SHA256 {
		return fmt.Errorf("backup manifest entry for %s is invalid", name)
	}
	if hash != expected.SHA256 || size != expected.Size {
		return fmt.Errorf("staged %s does not match verified manifest (hash/size changed after verify)", name)
	}
	return nil
}

func fileContentIdentity(path string) (string, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s must be a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), info.Size(), nil
}

func sameRegularFileContent(left, right string) (bool, error) {
	leftHash, leftSize, err := fileContentIdentity(left)
	if err != nil {
		return false, err
	}
	rightHash, rightSize, err := fileContentIdentity(right)
	if err != nil {
		return false, err
	}
	return leftHash == rightHash && leftSize == rightSize, nil
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
