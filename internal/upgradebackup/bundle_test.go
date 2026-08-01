package upgradebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateBundlesOneSnapshotWithManifestedInputs(t *testing.T) {
	root := t.TempDir()
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeBundleFile(t, root, "looper-bin", "cli")
	daemon := writeBundleFile(t, root, "looperd-bin", "daemon")
	snapshots := 0
	database := filepath.Join(root, "looper.sqlite")
	result, err := Create(context.Background(), Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, DatabasePath: database, CLIBinaryPath: cli, DaemonBinaryPath: daemon, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC) }, Snapshot: func(context.Context) (string, error) {
		snapshots++
		return writeBundleFile(t, root, "snapshot.sqlite", "sqlite-consistent"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 {
		t.Fatalf("snapshots = %d, want 1", snapshots)
	}
	if got := SortedFileNames(result.Manifest); len(got) != 4 || got[0] != "config.toml" || got[1] != "database.sqlite" || got[2] != "looper" || got[3] != "looperd" {
		t.Fatalf("manifest files = %v", got)
	}
	for name, file := range result.Manifest.Files {
		contents, err := os.ReadFile(filepath.Join(result.Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(contents)
		if file.SHA256 != hex.EncodeToString(sum[:]) || file.Size != int64(len(contents)) {
			t.Fatalf("manifest %s = %#v", name, file)
		}
	}
	if _, err := os.Stat(filepath.Join(result.Directory, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if source, err := RestoreSource(result.Manifest); err != nil || source.ConfigPath != config || source.DatabasePath != database || source.CLIBinaryPath != cli || source.DaemonBinaryPath != daemon {
		t.Fatalf("RestoreSource() = (%#v, %v)", source, err)
	}
}

func TestCreateRemovesPartialBundleOnCopyFailure(t *testing.T) {
	root := t.TempDir()
	snapshot := writeBundleFile(t, root, "snapshot.sqlite", "sqlite")
	_, err := Create(context.Background(), Input{RootDir: filepath.Join(root, "backups"), ConfigPath: filepath.Join(root, "missing.toml"), DatabasePath: filepath.Join(root, "looper.sqlite"), CLIBinaryPath: "/missing/looper", DaemonBinaryPath: "/missing/looperd", Now: time.Now, Snapshot: func(context.Context) (string, error) { return snapshot, nil }})
	if err == nil {
		t.Fatal("Create() error = nil")
	}
	entries, readErr := os.ReadDir(filepath.Join(root, "backups"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial bundles = %v", entries)
	}
}

func TestVerifyAcceptsCreatedBundleAndRejectsChangedLayoutOrContent(t *testing.T) {
	root := t.TempDir()
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeBundleFile(t, root, "looper-bin", "cli")
	daemon := writeBundleFile(t, root, "looperd-bin", "daemon")
	result, err := Create(context.Background(), Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, DatabasePath: filepath.Join(root, "looper.sqlite"), CLIBinaryPath: cli, DaemonBinaryPath: daemon, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC) }, Snapshot: func(context.Context) (string, error) {
		return writeBundleFile(t, root, "snapshot.sqlite", "sqlite-consistent"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(result.Directory)
	if err != nil || !verified.Valid || len(verified.Manifest.Files) != len(result.Manifest.Files) {
		t.Fatalf("Verify() = (%#v, %v)", verified, err)
	}
	if err := os.WriteFile(filepath.Join(result.Directory, "looper"), []byte("changed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(result.Directory); err == nil {
		t.Fatal("Verify() error = nil after changed binary")
	}
	if err := os.WriteFile(filepath.Join(result.Directory, "extra"), []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(result.Directory); err == nil {
		t.Fatal("Verify() error = nil after unexpected entry")
	}
}

func TestRestoreSourceRejectsLegacyManifest(t *testing.T) {
	if _, err := RestoreSource(Manifest{Version: LegacyManifestVersion}); err == nil {
		t.Fatal("RestoreSource() error = nil for v1 manifest")
	}
}

func TestVerifyAcceptsLegacyManifestButRejectsRelativeV2RestorePaths(t *testing.T) {
	root := t.TempDir()
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeBundleFile(t, root, "looper-bin", "cli")
	daemon := writeBundleFile(t, root, "looperd-bin", "daemon")
	result, err := Create(context.Background(), Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, DatabasePath: filepath.Join(root, "looper.sqlite"), CLIBinaryPath: cli, DaemonBinaryPath: daemon, Snapshot: func(context.Context) (string, error) {
		return writeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	legacy := result.Manifest
	legacy.Version, legacy.Source = LegacyManifestVersion, nil
	writeBundleManifest(t, result.Directory, legacy)
	if _, err := Verify(result.Directory); err != nil {
		t.Fatalf("Verify legacy bundle: %v", err)
	}
	invalid := result.Manifest
	invalid.Source.ConfigPath = "config.toml"
	writeBundleManifest(t, result.Directory, invalid)
	if _, err := Verify(result.Directory); err == nil {
		t.Fatal("Verify relative v2 source path error = nil")
	}
}

func writeBundleManifest(t *testing.T, directory string, manifest Manifest) {
	t.Helper()
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeBundleFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
