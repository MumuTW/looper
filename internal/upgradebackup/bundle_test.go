package upgradebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	result, err := Create(context.Background(), Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, CLIBinaryPath: cli, DaemonBinaryPath: daemon, Now: func() time.Time { return time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC) }, Snapshot: func(context.Context) (string, error) {
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
}

func TestCreateRemovesPartialBundleOnCopyFailure(t *testing.T) {
	root := t.TempDir()
	snapshot := writeBundleFile(t, root, "snapshot.sqlite", "sqlite")
	_, err := Create(context.Background(), Input{RootDir: filepath.Join(root, "backups"), ConfigPath: filepath.Join(root, "missing.toml"), CLIBinaryPath: "/missing/looper", DaemonBinaryPath: "/missing/looperd", Now: time.Now, Snapshot: func(context.Context) (string, error) { return snapshot, nil }})
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

func writeBundleFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
