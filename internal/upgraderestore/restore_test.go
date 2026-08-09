package upgraderestore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MumuTW/looper/internal/upgradebackup"
)

func TestRestoreReplacesConfigAndDatabaseAndRemovesSidecars(t *testing.T) {
	fixture := newRestoreFixture(t, true)

	if err := Restore(fixture.bundleDirectory, fixture.source); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	assertFileContents(t, fixture.configPath, "new config")
	assertFileContents(t, fixture.databasePath, "new database")
	assertFileMode(t, fixture.configPath, 0o640)
	assertFileMode(t, fixture.databasePath, 0o660)
	assertPathMissing(t, fixture.databasePath+"-wal")
	assertPathMissing(t, fixture.databasePath+"-shm")
	assertPathMissing(t, JournalPath(fixture.databasePath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.configPath))
	assertNoRestoreArtifacts(t, filepath.Dir(fixture.databasePath))
}

func TestRestoreUsesPrivatePermissionsForMissingTargets(t *testing.T) {
	fixture := newRestoreFixture(t, false)

	if err := Restore(fixture.bundleDirectory, fixture.source); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	assertFileContents(t, fixture.configPath, "new config")
	assertFileContents(t, fixture.databasePath, "new database")
	assertFileMode(t, fixture.configPath, 0o600)
	assertFileMode(t, fixture.databasePath, 0o600)
}

func TestRestoreRejectsSymlinkedBundleSources(t *testing.T) {
	for _, name := range []string{"config.toml", "database.sqlite"} {
		t.Run(name, func(t *testing.T) {
			fixture := newRestoreFixture(t, true)
			sourcePath := filepath.Join(fixture.bundleDirectory, name)
			referent := writeTestFile(t, filepath.Join(fixture.root, "referent-"+strings.ReplaceAll(name, ".", "-")), "replacement", 0o600)
			if err := os.Remove(sourcePath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(referent, sourcePath); err != nil {
				t.Fatal(err)
			}

			err := Restore(fixture.bundleDirectory, fixture.source)
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("Restore() error = %v, want regular-file rejection", err)
			}
			fixture.assertOriginalTargets(t)
			assertPathMissing(t, JournalPath(fixture.databasePath))
		})
	}
}

func TestRestoreRejectsSymlinkedExistingTargets(t *testing.T) {
	for _, targetName := range []string{"config", "database", "wal", "shm"} {
		t.Run(targetName, func(t *testing.T) {
			fixture := newRestoreFixture(t, true)
			targetPath := fixture.targetPath(targetName)
			referent := writeTestFile(t, filepath.Join(fixture.root, "symlink-referent-"+targetName), "do not change", 0o600)
			if err := os.Remove(targetPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(referent, targetPath); err != nil {
				t.Fatal(err)
			}

			err := Restore(fixture.bundleDirectory, fixture.source)
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("Restore() error = %v, want regular-file rejection", err)
			}
			info, statErr := os.Lstat(targetPath)
			if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("target symlink after rejection = (%v, %v)", info, statErr)
			}
			assertFileContents(t, referent, "do not change")
			assertPathMissing(t, JournalPath(fixture.databasePath))
		})
	}
}

func TestRestoreRejectsNonRegularExistingTargets(t *testing.T) {
	for _, targetName := range []string{"config", "database", "wal", "shm"} {
		t.Run(targetName, func(t *testing.T) {
			fixture := newRestoreFixture(t, true)
			targetPath := fixture.targetPath(targetName)
			if err := os.Remove(targetPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(targetPath, 0o700); err != nil {
				t.Fatal(err)
			}

			err := Restore(fixture.bundleDirectory, fixture.source)
			if err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("Restore() error = %v, want regular-file rejection", err)
			}
			info, statErr := os.Lstat(targetPath)
			if statErr != nil || !info.IsDir() {
				t.Fatalf("target directory after rejection = (%v, %v)", info, statErr)
			}
			assertPathMissing(t, JournalPath(fixture.databasePath))
		})
	}
}

type restoreFixture struct {
	root            string
	bundleDirectory string
	configPath      string
	databasePath    string
	source          upgradebackup.Source
}

func newRestoreFixture(t *testing.T, existingTargets bool) restoreFixture {
	t.Helper()
	root := t.TempDir()
	bundleDirectory := filepath.Join(root, "bundle")
	configDirectory := filepath.Join(root, "etc")
	databaseDirectory := filepath.Join(root, "data")
	for _, directory := range []string{bundleDirectory, configDirectory, databaseDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(configDirectory, "config.toml")
	databasePath := filepath.Join(databaseDirectory, "looper.sqlite")
	writeTestFile(t, filepath.Join(bundleDirectory, "config.toml"), "new config", 0o600)
	writeTestFile(t, filepath.Join(bundleDirectory, "database.sqlite"), "new database", 0o600)
	if existingTargets {
		writeTestFile(t, configPath, "old config", 0o640)
		writeTestFile(t, databasePath, "old database", 0o660)
		writeTestFile(t, databasePath+"-wal", "old wal", 0o604)
		writeTestFile(t, databasePath+"-shm", "old shm", 0o620)
	}
	return restoreFixture{
		root:            root,
		bundleDirectory: bundleDirectory,
		configPath:      configPath,
		databasePath:    databasePath,
		source:          upgradebackup.Source{ConfigPath: configPath, DatabasePath: databasePath},
	}
}

func (fixture restoreFixture) targetPath(name string) string {
	switch name {
	case "config":
		return fixture.configPath
	case "database":
		return fixture.databasePath
	case "wal":
		return fixture.databasePath + "-wal"
	case "shm":
		return fixture.databasePath + "-shm"
	default:
		panic("unknown target " + name)
	}
}

func (fixture restoreFixture) assertOriginalTargets(t *testing.T) {
	t.Helper()
	assertFileContents(t, fixture.configPath, "old config")
	assertFileContents(t, fixture.databasePath, "old database")
	assertFileContents(t, fixture.databasePath+"-wal", "old wal")
	assertFileContents(t, fixture.databasePath+"-shm", "old shm")
}

func writeTestFile(t *testing.T, path, contents string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(contents) != want {
		t.Fatalf("contents of %s = %q, want %q", path, contents, want)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode of %s = %#o, want %#o", path, got, want)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(%s) error = %v, want not exist", path, err)
	}
}

func assertNoRestoreArtifacts(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".looper-restore-") || strings.HasSuffix(entry.Name(), journalSuffix) {
			t.Errorf("unexpected restore artifact %s", filepath.Join(directory, entry.Name()))
		}
	}
}
