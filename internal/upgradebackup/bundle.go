// Package upgradebackup creates self-describing rollback bundles from a
// daemon-owned SQLite snapshot and explicitly supplied installation files.
package upgradebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	LegacyManifestVersion = 1
	ManifestVersion       = 2
)

type Input struct {
	RootDir          string
	ConfigPath       string
	DatabasePath     string
	CLIBinaryPath    string
	DaemonBinaryPath string
	Now              func() time.Time
	Snapshot         func(context.Context) (string, error)
}

type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	Version   int             `json:"version"`
	CreatedAt string          `json:"createdAt"`
	Files     map[string]File `json:"files"`
	Source    *Source         `json:"source,omitempty"`
}

// Source records the exact pre-cutover destinations a v2 bundle can restore.
// The bundle directory is 0700 and manifest.json is 0600 because these paths
// can expose an operator's installation layout. A v1 manifest has no Source:
// it remains verifiable but restore must fail closed instead of guessing.
type Source struct {
	ConfigPath       string `json:"configPath"`
	DatabasePath     string `json:"databasePath"`
	CLIBinaryPath    string `json:"cliBinaryPath"`
	DaemonBinaryPath string `json:"daemonBinaryPath"`
}
type Result struct {
	Directory string   `json:"directory"`
	Manifest  Manifest `json:"manifest"`
}

type Verification struct {
	Directory string   `json:"directory"`
	Manifest  Manifest `json:"manifest"`
	Valid     bool     `json:"valid"`
}

// Create invokes the supplied daemon-owned snapshot operation once, then moves
// that consistent snapshot into a new bundle and copies the exact operator
// config/CLI/daemon files beside it. A manifest is written last, so its
// presence means every listed file was copied and checksummed successfully.
func Create(ctx context.Context, input Input) (Result, error) {
	if strings.TrimSpace(input.RootDir) == "" || input.Snapshot == nil {
		return Result{}, fmt.Errorf("backup root and snapshot operation are required")
	}
	if strings.TrimSpace(input.ConfigPath) == "" || strings.TrimSpace(input.DatabasePath) == "" || strings.TrimSpace(input.CLIBinaryPath) == "" || strings.TrimSpace(input.DaemonBinaryPath) == "" {
		return Result{}, fmt.Errorf("config, database, CLI binary, and daemon binary paths are required")
	}
	configPath, err := absolutePath(input.ConfigPath, "config")
	if err != nil {
		return Result{}, err
	}
	databasePath, err := absolutePath(input.DatabasePath, "database")
	if err != nil {
		return Result{}, err
	}
	cliBinaryPath, err := absolutePath(input.CLIBinaryPath, "CLI binary")
	if err != nil {
		return Result{}, err
	}
	daemonBinaryPath, err := absolutePath(input.DaemonBinaryPath, "daemon binary")
	if err != nil {
		return Result{}, err
	}
	now := time.Now
	if input.Now != nil {
		now = input.Now
	}
	if err := os.MkdirAll(input.RootDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create backup root: %w", err)
	}
	snapshot, err := input.Snapshot(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("create sqlite snapshot: %w", err)
	}
	bundle := filepath.Join(input.RootDir, "upgrade-"+strings.ReplaceAll(now().UTC().Format("20060102T150405.000Z"), ":", "-"))
	if err := os.Mkdir(bundle, 0o700); err != nil {
		return Result{}, fmt.Errorf("create backup bundle: %w", err)
	}
	fail := func(err error) (Result, error) { _ = os.RemoveAll(bundle); return Result{}, err }
	files := map[string]File{}
	if err := moveAndRecord(snapshot, filepath.Join(bundle, "database.sqlite"), files, "database.sqlite"); err != nil {
		return fail(err)
	}
	for _, item := range []struct{ source, name string }{{configPath, "config" + filepath.Ext(configPath)}, {cliBinaryPath, "looper"}, {daemonBinaryPath, "looperd"}} {
		if err := copyAndRecord(item.source, filepath.Join(bundle, item.name), files, item.name); err != nil {
			return fail(err)
		}
	}
	manifest := Manifest{Version: ManifestVersion, CreatedAt: now().UTC().Format(time.RFC3339Nano), Files: files, Source: &Source{ConfigPath: configPath, DatabasePath: databasePath, CLIBinaryPath: cliBinaryPath, DaemonBinaryPath: daemonBinaryPath}}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("encode backup manifest: %w", err))
	}
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), append(encoded, '\n'), 0o600); err != nil {
		return fail(fmt.Errorf("write backup manifest: %w", err))
	}
	return Result{Directory: bundle, Manifest: manifest}, nil
}

func moveAndRecord(source, destination string, files map[string]File, name string) error {
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("move %s: %w", name, err)
	}
	return record(destination, files, name)
}
func copyAndRecord(source, destination string, files map[string]File, name string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", name, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return record(destination, files, name)
}
func record(path string, files map[string]File, name string) error {
	file, err := describeFile(path, name)
	if err != nil {
		return err
	}
	files[name] = file
	return nil
}

func describeFile(path, name string) (File, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read %s for checksum: %w", name, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return File{}, fmt.Errorf("stat %s: %w", name, err)
	}
	sum := sha256.Sum256(contents)
	return File{SHA256: hex.EncodeToString(sum[:]), Size: info.Size()}, nil
}

// Verify confirms that a bundle still has the exact small layout Create
// produces and that each named file matches its manifest entry. This is an
// accidental-corruption and wrong-bundle check, not a signature: anyone able
// to replace both a file and manifest.json can make a different self-consistent
// bundle.
func Verify(directory string) (Verification, error) {
	if strings.TrimSpace(directory) == "" {
		return Verification{}, fmt.Errorf("backup bundle directory is required")
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return Verification{}, fmt.Errorf("stat backup manifest: %w", err)
	}
	if !manifestInfo.Mode().IsRegular() {
		return Verification{}, fmt.Errorf("backup manifest must be a regular file")
	}
	encoded, err := os.ReadFile(manifestPath)
	if err != nil {
		return Verification{}, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return Verification{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return Verification{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return Verification{}, fmt.Errorf("read backup bundle: %w", err)
	}
	expected := map[string]bool{"manifest.json": true}
	for name, want := range manifest.Files {
		expected[name] = true
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil {
			return Verification{}, fmt.Errorf("stat bundle file %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return Verification{}, fmt.Errorf("bundle file %s must be a regular file", name)
		}
		got, err := describeFile(path, name)
		if err != nil {
			return Verification{}, err
		}
		if got != want {
			return Verification{}, fmt.Errorf("bundle file %s does not match manifest", name)
		}
	}
	for _, entry := range entries {
		if !expected[entry.Name()] {
			return Verification{}, fmt.Errorf("unexpected bundle entry %s", entry.Name())
		}
	}
	return Verification{Directory: directory, Manifest: manifest, Valid: true}, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Version != LegacyManifestVersion && manifest.Version != ManifestVersion {
		return fmt.Errorf("unsupported backup manifest version %d", manifest.Version)
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt); err != nil {
		return fmt.Errorf("invalid backup manifest createdAt: %w", err)
	}
	if len(manifest.Files) != 4 {
		return fmt.Errorf("backup manifest must name exactly four files")
	}
	configFiles := 0
	for name, file := range manifest.Files {
		if filepath.Base(name) != name || name == "manifest.json" || strings.TrimSpace(name) == "" {
			return fmt.Errorf("invalid backup manifest file name %q", name)
		}
		if len(file.SHA256) != sha256.Size*2 || strings.ToLower(file.SHA256) != file.SHA256 {
			return fmt.Errorf("invalid backup manifest checksum for %s", name)
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil || file.Size < 0 {
			return fmt.Errorf("invalid backup manifest file metadata for %s", name)
		}
		if name == "config" || strings.HasPrefix(name, "config.") {
			configFiles++
		}
	}
	if !manifest.Files["database.sqlite"].valid() || !manifest.Files["looper"].valid() || !manifest.Files["looperd"].valid() || configFiles != 1 {
		return fmt.Errorf("backup manifest must name database.sqlite, looper, looperd, and one config file")
	}
	if manifest.Version == ManifestVersion {
		if manifest.Source == nil || strings.TrimSpace(manifest.Source.ConfigPath) == "" || strings.TrimSpace(manifest.Source.DatabasePath) == "" || strings.TrimSpace(manifest.Source.CLIBinaryPath) == "" || strings.TrimSpace(manifest.Source.DaemonBinaryPath) == "" {
			return fmt.Errorf("backup manifest v%d requires source restore metadata", ManifestVersion)
		}
		for _, item := range []struct{ name, path string }{{"config", manifest.Source.ConfigPath}, {"database", manifest.Source.DatabasePath}, {"CLI binary", manifest.Source.CLIBinaryPath}, {"daemon binary", manifest.Source.DaemonBinaryPath}} {
			if !filepath.IsAbs(item.path) {
				return fmt.Errorf("backup manifest v%d %s path must be absolute", ManifestVersion, item.name)
			}
		}
	}
	return nil
}

// RestoreSource returns the exact original destinations recorded in a v2
// bundle. Older bundles deliberately return an error: guessing paths during a
// destructive restore would make the manifest less safe than no restore.
func RestoreSource(manifest Manifest) (Source, error) {
	if manifest.Version != ManifestVersion || manifest.Source == nil {
		return Source{}, fmt.Errorf("backup manifest v%d has no restore target metadata; create a new backup before using restore", manifest.Version)
	}
	return *manifest.Source, nil
}

func absolutePath(path, description string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", description, err)
	}
	return filepath.Clean(abs), nil
}

func (file File) valid() bool {
	return file.Size >= 0 && len(file.SHA256) == sha256.Size*2 && strings.ToLower(file.SHA256) == file.SHA256
}
func SortedFileNames(manifest Manifest) []string {
	names := make([]string, 0, len(manifest.Files))
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
