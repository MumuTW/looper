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

const ManifestVersion = 1

type Input struct {
	RootDir          string
	ConfigPath       string
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
}
type Result struct {
	Directory string   `json:"directory"`
	Manifest  Manifest `json:"manifest"`
}

// Create invokes the supplied daemon-owned snapshot operation once, then moves
// that consistent snapshot into a new bundle and copies the exact operator
// config/CLI/daemon files beside it. A manifest is written last, so its
// presence means every listed file was copied and checksummed successfully.
func Create(ctx context.Context, input Input) (Result, error) {
	if strings.TrimSpace(input.RootDir) == "" || input.Snapshot == nil {
		return Result{}, fmt.Errorf("backup root and snapshot operation are required")
	}
	if strings.TrimSpace(input.ConfigPath) == "" || strings.TrimSpace(input.CLIBinaryPath) == "" || strings.TrimSpace(input.DaemonBinaryPath) == "" {
		return Result{}, fmt.Errorf("config, CLI binary, and daemon binary paths are required")
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
	for _, item := range []struct{ source, name string }{{input.ConfigPath, "config" + filepath.Ext(input.ConfigPath)}, {input.CLIBinaryPath, "looper"}, {input.DaemonBinaryPath, "looperd"}} {
		if err := copyAndRecord(item.source, filepath.Join(bundle, item.name), files, item.name); err != nil {
			return fail(err)
		}
	}
	manifest := Manifest{Version: ManifestVersion, CreatedAt: now().UTC().Format(time.RFC3339Nano), Files: files}
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
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s for checksum: %w", name, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	sum := sha256.Sum256(contents)
	files[name] = File{SHA256: hex.EncodeToString(sum[:]), Size: info.Size()}
	return nil
}
func SortedFileNames(manifest Manifest) []string {
	names := make([]string, 0, len(manifest.Files))
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
