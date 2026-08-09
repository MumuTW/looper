package upgradebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MumuTW/looper/internal/version"
)

func TestCreateBundlesOneSnapshotWithManifestedInputs(t *testing.T) {
	root := t.TempDir()
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
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

func TestCreateSyncsNewBackupRootParent(t *testing.T) {
	root := t.TempDir()
	backupRoot := filepath.Join(root, "new-parent", "backups")
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
	originalSyncDirectory := syncDirectory
	t.Cleanup(func() { syncDirectory = originalSyncDirectory })
	calls := map[string]int{}
	syncDirectory = func(path string) error {
		calls[path]++
		return originalSyncDirectory(path)
	}
	if _, err := Create(context.Background(), Input{
		RootDir: backupRoot, ConfigPath: config, DatabasePath: filepath.Join(root, "looper.sqlite"),
		CLIBinaryPath: cli, DaemonBinaryPath: daemon,
		Snapshot: func(context.Context) (string, error) {
			return writeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if calls[filepath.Dir(backupRoot)] == 0 {
		t.Fatalf("syncDirectory calls = %#v, want newly-created backup root parent %q", calls, filepath.Dir(backupRoot))
	}
}

func TestCreatePinsConfigContentsAcrossSnapshotWindow(t *testing.T) {
	// Regression for the backup race: validated bytes must be what lands in the
	// bundle even if ConfigPath is rewritten while Snapshot runs.
	root := t.TempDir()
	configPath := writeBundleFile(t, root, "config.toml", "revision-checked\n")
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
	pinned := []byte("revision-checked\n")
	result, err := Create(context.Background(), Input{
		RootDir:          filepath.Join(root, "backups"),
		ConfigPath:       configPath,
		ConfigContents:   pinned,
		DatabasePath:     filepath.Join(root, "looper.sqlite"),
		CLIBinaryPath:    cli,
		DaemonBinaryPath: daemon,
		Now:              func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) },
		Snapshot: func(context.Context) (string, error) {
			if err := os.WriteFile(configPath, []byte("mutated-after-check\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return writeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(result.Directory, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(pinned) {
		t.Fatalf("bundle config = %q, want pinned %q (not on-disk mutation)", got, pinned)
	}
}

func TestCreatePinsBinaryContentsAcrossSnapshotWindow(t *testing.T) {
	// Regression for cutover races: the release-root/current path may switch
	// while the SQLite snapshot is in progress, but the bundle must retain the
	// executable images verified before the snapshot began.
	root := t.TempDir()
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
	pinnedCLI, err := os.ReadFile(cli)
	if err != nil {
		t.Fatal(err)
	}
	pinnedDaemon, err := os.ReadFile(daemon)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Create(context.Background(), Input{
		RootDir:              filepath.Join(root, "backups"),
		ConfigPath:           config,
		DatabasePath:         filepath.Join(root, "looper.sqlite"),
		CLIBinaryPath:        cli,
		CLIBinaryContents:    pinnedCLI,
		DaemonBinaryPath:     daemon,
		DaemonBinaryContents: pinnedDaemon,
		Snapshot: func(context.Context) (string, error) {
			if err := os.WriteFile(cli, []byte("new-cli-after-check"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(daemon, []byte("new-daemon-after-check"), 0o700); err != nil {
				t.Fatal(err)
			}
			return writeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		want []byte
	}{
		{name: "looper", want: pinnedCLI},
		{name: "looperd", want: pinnedDaemon},
	} {
		got, err := os.ReadFile(filepath.Join(result.Directory, test.name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(test.want) {
			t.Fatalf("bundle %s = %q, want pinned %q", test.name, got, test.want)
		}
	}
}

func TestCreateUsesUniqueBundleWhenTimestampDirectoryAlreadyExists(t *testing.T) {
	root := t.TempDir()
	backupRoot := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	prefix := "upgrade-" + strings.ReplaceAll(now.UTC().Format("20060102T150405.000Z"), ":", "-")
	if err := os.Mkdir(filepath.Join(backupRoot, prefix), 0o700); err != nil {
		t.Fatal(err)
	}
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
	result, err := Create(context.Background(), Input{
		RootDir: backupRoot, ConfigPath: config, DatabasePath: filepath.Join(root, "looper.sqlite"),
		CLIBinaryPath: cli, DaemonBinaryPath: daemon, Now: func() time.Time { return now },
		Snapshot: func(context.Context) (string, error) {
			return writeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Directory == filepath.Join(backupRoot, prefix) || !strings.HasPrefix(filepath.Base(result.Directory), prefix+"-") {
		t.Fatalf("bundle directory = %q, want unique %s-* directory", result.Directory, prefix)
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
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
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

func TestCreateRejectsVersionSkewedBinaryPair(t *testing.T) {
	root := t.TempDir()
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-b")
	snapshot := writeBundleFile(t, root, "snapshot.sqlite", "sqlite")
	if _, err := Create(context.Background(), Input{RootDir: filepath.Join(root, "backups"), ConfigPath: config, DatabasePath: filepath.Join(root, "looper.sqlite"), CLIBinaryPath: cli, DaemonBinaryPath: daemon, Now: time.Now, Snapshot: func(context.Context) (string, error) { return snapshot, nil }}); err == nil {
		t.Fatal("Create() error = nil for version-skewed binary pair")
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("snapshot = %v, want source snapshot preserved after pair rejection", err)
	}
}

func TestCreateAcceptsEmptyConfigContents(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
	result, err := Create(context.Background(), Input{
		RootDir: filepath.Join(root, "backups"), ConfigPath: configPath, ConfigContents: []byte{},
		DatabasePath: filepath.Join(root, "looper.sqlite"), CLIBinaryPath: cli, DaemonBinaryPath: daemon,
		Snapshot: func(context.Context) (string, error) {
			return writeBundleFile(t, root, "snapshot.sqlite", "sqlite"), nil
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(result.Directory, "config.toml"))
	if err != nil {
		t.Fatalf("stat empty config artifact: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("empty config artifact size = %d, want 0", info.Size())
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
	cli := writeIdentityExecutable(t, root, "looper-bin", "release-a")
	daemon := writeIdentityExecutable(t, root, "looperd-bin", "release-a")
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
func writeIdentityExecutable(t *testing.T, dir, name, commit string) string {
	t.Helper()
	dirty := false
	ts := "2026-07-31T01:02:03Z"
	identity := version.Info{Version: "1.2.3", Metadata: version.BuildMetadata{VersionSource: "test", Channel: "release", APIVersion: "v1", GitCommitSHA: &commit, BuildTimestamp: &ts, Dirty: &dirty}}
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+string(encoded)+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreateRefusesSymlinkedDatabasePath(t *testing.T) {
	root := t.TempDir()
	realDB := writeBundleFile(t, root, "real.sqlite", "db")
	link := filepath.Join(root, "link.sqlite")
	if err := os.Symlink(realDB, link); err != nil {
		t.Fatal(err)
	}
	config := writeBundleFile(t, root, "config.toml", "[server]\n")
	cli := writeBundleFile(t, root, "looper", "cli")
	daemon := writeBundleFile(t, root, "looperd", "daemon")
	_, err := Create(context.Background(), Input{
		RootDir: filepath.Join(root, "backups"), ConfigPath: config, DatabasePath: link,
		CLIBinaryPath: cli, DaemonBinaryPath: daemon,
		Snapshot: func(context.Context) (string, error) {
			return writeBundleFile(t, root, "snap.sqlite", "snap"), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Create() error = %v, want symlink refusal", err)
	}
}
